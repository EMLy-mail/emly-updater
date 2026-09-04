package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"emlyupdater/internal/config"
	"emlyupdater/internal/ipc"
	"emlyupdater/internal/ipc/ipcpb"
	"emlyupdater/internal/logging"
	"emlyupdater/internal/policy"
	"emlyupdater/internal/source"
)

// staleWarnEvery bounds event 903 to once a day per outage.
const staleWarnEvery = 24 * time.Hour

// cycleState is everything one poll cycle decided about this machine at its
// start, taken as a unit so a policy swap mid-cycle cannot mix two policies.
// The IPC server reads the latest one to answer ConfigRequest.
type cycleState struct {
	snap  *policy.Snapshot
	host  policy.Host
	eff   *policy.Effective
	site  string
	chain []string // server names, in the order to try
}

// initPolicy loads the last-known-good document from disk into the policy
// store, or falls back to the default policy derived from config.ini. Runs
// once, before the first fetch, so a machine with a valid cache starts with
// that policy even if the network is not up yet.
func (u *Updater) initPolicy() {
	if u.Policy != nil {
		return
	}
	u.Defaults = policy.DefaultsFromConfig(u.Cfg, ipc.CompiledIPCProtocol(), u.consoleDebug)
	fallback := &policy.Snapshot{Parsed: policy.FromLegacy(u.Cfg, u.Defaults), Source: policy.SourceDefault}

	if !u.Cfg.RemoteConfigEnabled {
		u.Policy = policy.NewStore(fallback)
		u.Log.Info("remote configuration disabled by config.ini, using the legacy [source] settings")
		return
	}

	path := u.cachePath()
	cache, err := policy.LoadCache(path)
	if err != nil {
		u.Log.Warn("remote configuration cache unreadable, moving it aside and using the default policy",
			"path", path, "error", err.Error())
		_ = policy.QuarantineCache(path, u.cacheBadPath())
		u.Policy = policy.NewStore(fallback)
		return
	}
	if cache == nil {
		u.Policy = policy.NewStore(fallback)
		u.Log.Info("no remote configuration cached yet, using the default policy derived from config.ini")
		return
	}

	parsed, probs := policy.Parse(cache.Document, u.Defaults)
	if probs != nil {
		u.Log.ErrorEvent(logging.EventRemoteConfigRejected,
			"cached remote configuration failed validation, moving it aside and using the default policy",
			"path", path, "revision", cache.Revision(), "problems", probs.Error())
		_ = policy.QuarantineCache(path, u.cacheBadPath())
		u.Policy = policy.NewStore(fallback)
		return
	}

	snap := &policy.Snapshot{
		Parsed:      parsed,
		Source:      policy.SourceCache,
		FetchedAt:   cache.FetchedAt,
		FetchedFrom: cache.FetchedFrom,
		ETag:        cache.ETag,
	}
	u.Policy = policy.NewStore(snap)
	u.Log.InfoEvent(logging.EventRemoteConfigApplied, "remote configuration loaded from cache",
		"revision", snap.Revision(), "generatedAt", snap.GeneratedAt(),
		"fetchedAt", snap.FetchedAt.Format(time.RFC3339), "fetchedFrom", snap.FetchedFrom,
		"source", snap.Source.String())
}

// configCandidates lists the base URLs to ask for the document, in order:
// the servers the current policy assigns to this machine (base, backups,
// default), then the bootstrap endpoints from config.ini. Deduplicated, so
// a bootstrap endpoint that is also this site's server is tried once.
func (u *Updater) configCandidates() []configCandidate {
	var out []configCandidate
	seen := map[string]bool{}
	add := func(name, base string) {
		base = strings.TrimRight(base, "/")
		if base == "" || seen[base] {
			return
		}
		seen[base] = true
		out = append(out, configCandidate{name: name, base: base})
	}

	snap := u.Policy.Current()
	eff := snap.Parsed
	host := policy.Host{Now: u.clock()}
	if cur := u.cur.Load(); cur != nil {
		host = cur.host
	}
	if e, err := eff.Effective(host); err == nil {
		_, chain := e.Chain(host)
		for _, name := range chain {
			add(name, e.BaseURL(name))
		}
		if e.Doc.DefaultServer != "" {
			add(e.Doc.DefaultServer, e.BaseURL(e.Doc.DefaultServer))
		}
	}
	for _, ep := range u.Cfg.RemoteConfigEndpoints {
		add(ep, ep)
	}
	return out
}

type configCandidate struct {
	name string // server name from the policy, or the bootstrap URL itself
	base string
}

// refreshConfig fetches the document when it is due (or when force is set),
// validates it and swaps the policy. Never returns an error: the fetch is
// best-effort and the cycle carries on with whatever policy it has.
func (u *Updater) refreshConfig(ctx context.Context, force bool) {
	if !u.Cfg.RemoteConfigEnabled {
		return
	}
	now := u.clock()
	snap := u.Policy.Current()
	if !force && !u.lastConfigAttempt.IsZero() && now.Sub(u.lastConfigAttempt) < snap.Parsed.Global.Refresh.Interval() {
		return
	}
	u.lastConfigAttempt = now

	etag := ""
	if snap.Source != policy.SourceDefault {
		etag = snap.ETag
	}

	var lastErr error
	for _, c := range u.configCandidates() {
		if ctx.Err() != nil {
			return
		}
		url := c.base + u.Cfg.RemoteConfigRoute
		resp, err := u.fetchConfig(ctx, url, etag)
		if err != nil {
			lastErr = err
			u.Log.Debug("remote configuration fetch failed, trying the next server",
				"server", c.name, "url", url, "error", err.Error())
			continue
		}
		u.configUnreachable = false

		switch resp.Status {
		case 304:
			u.confirmSnapshot(snap, c.name, now)
			u.Log.Debug("remote configuration unchanged", "server", c.name, "revision", snap.Revision())
		case 204:
			u.Log.Debug("remote configuration: nothing published on this server, keeping the current policy",
				"server", c.name, "revision", snap.Revision(), "policySource", snap.Source.String())
		default:
			u.acceptDocument(resp, c.name, url, now)
		}
		return
	}

	if lastErr == nil {
		lastErr = errors.New("no server to ask")
	}
	if !u.configUnreachable {
		u.Log.WarnEvent(logging.EventRemoteConfigUnreachable,
			"remote configuration unreachable on every server, keeping the current policy",
			"revision", snap.Revision(), "policySource", snap.Source.String(), "lastError", lastErr.Error())
		u.configUnreachable = true
	}
}

// fetchConfig performs one GET through the standard HTTP source so the
// identification headers and the API key go out like on every other request.
func (u *Updater) fetchConfig(ctx context.Context, url, etag string) (*source.ConfigResponse, error) {
	if u.fetchConfigFn != nil {
		return u.fetchConfigFn(ctx, url, etag)
	}
	src := u.newHTTPSource(url)
	return src.FetchConfig(ctx, url, etag, u.Cfg.RemoteConfigTimeout, policy.MaxDocumentSize)
}

// confirmSnapshot records that the server just revalidated the cached
// document: it is now "remote", fetched now, and the cache's fetchedAt moves
// so staleness is measured from this confirmation.
func (u *Updater) confirmSnapshot(snap *policy.Snapshot, from string, now time.Time) {
	fresh := *snap
	fresh.Source = policy.SourceRemote
	fresh.FetchedAt = now
	fresh.FetchedFrom = from
	u.Policy.Set(&fresh)
	u.saveCache(&fresh)
}

// acceptDocument validates a freshly served document and, if it passes and
// is not older than what is cached, makes it the current policy.
func (u *Updater) acceptDocument(resp *source.ConfigResponse, from, url string, now time.Time) {
	cur := u.Policy.Current()
	parsed, probs := policy.Parse(resp.Body, u.Defaults)
	if probs != nil {
		var head struct {
			Revision int64 `json:"revision"`
		}
		_ = json.Unmarshal(resp.Body, &head)
		u.Log.ErrorEvent(logging.EventRemoteConfigRejected,
			"remote configuration rejected, keeping the current policy",
			"server", from, "url", url, "revision", head.Revision, "currentRevision", cur.Revision(),
			"problems", probs.Error())
		return
	}
	if cur.Source != policy.SourceDefault && parsed.Global.Revision < cur.Revision() {
		u.Log.Debug("remote configuration older than the cached one, ignored",
			"server", from, "revision", parsed.Global.Revision, "currentRevision", cur.Revision())
		return
	}

	snap := &policy.Snapshot{
		Parsed:      parsed,
		Source:      policy.SourceRemote,
		FetchedAt:   now,
		FetchedFrom: from,
		ETag:        resp.ETag,
	}
	u.Policy.Set(snap)
	u.saveCache(snap)
	u.Log.InfoEvent(logging.EventRemoteConfigApplied, "remote configuration applied",
		"revision", snap.Revision(), "previousRevision", cur.Revision(), "generatedAt", snap.GeneratedAt(),
		"fetchedFrom", from, "source", snap.Source.String(), "overrides", len(parsed.Global.Overrides))
}

// saveCache persists snap as the last-known-good document. A failure (disk
// full, ACL) is logged and tolerated: the policy stays in memory for this
// run and the write is retried at the next fetch.
func (u *Updater) saveCache(snap *policy.Snapshot) {
	if snap.Source == policy.SourceDefault {
		return
	}
	raw, err := json.Marshal(snap.Parsed.Global)
	if err != nil {
		u.Log.Warn("could not serialise the remote configuration for the cache", "error", err.Error())
		return
	}
	c := &policy.CacheFile{
		FetchedAt:   snap.FetchedAt,
		FetchedFrom: snap.FetchedFrom,
		ETag:        snap.ETag,
		Document:    raw,
	}
	if err := policy.SaveCache(u.cachePath(), c); err != nil {
		u.Log.Warn("could not write the remote configuration cache, the policy stays in memory for this run",
			"path", u.cachePath(), "revision", snap.Revision(), "error", err.Error())
	}
}

func (u *Updater) cachePath() string {
	if u.CachePath != "" {
		return u.CachePath
	}
	return config.RemoteConfigPath()
}

// cacheBadPath is where a cache that failed to load or validate is moved
// aside, next to the cache itself so both travel together off a machine.
func (u *Updater) cacheBadPath() string {
	if u.CachePath != "" {
		return u.CachePath + ".bad"
	}
	return config.RemoteConfigBadPath()
}

// applyLogging pushes the effective logging section into the running logger
// when it differs from what was applied last.
func (u *Updater) applyLogging(lg policy.LoggingSettings) {
	if u.appliedLogging != nil && *u.appliedLogging == lg {
		return
	}
	if !u.Log.SetLevel(lg.Level) {
		u.Log.Warn("unknown log level in the remote configuration, keeping the current one", "level", lg.Level)
	}
	u.Log.Reconfigure(logging.FileSettings{MaxSizeMB: lg.MaxSizeMB, MaxBackups: lg.Backups, Compress: lg.Compress})
	u.Log.SetEventLog(lg.EventLog)
	u.Log.Info("logging settings applied", "level", lg.Level, "maxSizeMB", lg.MaxSizeMB,
		"backups", lg.Backups, "compress", lg.Compress, "eventLog", lg.EventLog)
	copyOf := lg
	u.appliedLogging = &copyOf
}

// warnIfStale logs event 903 at most once a day while the cache is older
// than refresh.staleAfterDays. The policy stays in force regardless.
func (u *Updater) warnIfStale(snap *policy.Snapshot, now time.Time) {
	if !snap.Stale(now) {
		u.lastStaleWarn = time.Time{}
		return
	}
	if !u.lastStaleWarn.IsZero() && now.Sub(u.lastStaleWarn) < staleWarnEvery {
		return
	}
	u.lastStaleWarn = now
	u.Log.WarnEvent(logging.EventRemoteConfigStale,
		"remote configuration cache is stale, still in force",
		"revision", snap.Revision(), "fetchedAt", snap.FetchedAt.Format(time.RFC3339),
		"staleAfterDays", snap.Parsed.Global.Refresh.StaleAfterDays)
}

// applyControlGate resolves control.updater for this cycle and logs event
// 904 on every transition. It returns whether the update machinery may run.
func (u *Updater) applyControlGate(cyc *cycleState) bool {
	enabled, reason := cyc.eff.UpdaterEnabled(cyc.host.Now)
	c := cyc.eff.Doc.Control.Updater
	until := ""
	if c.Until != nil {
		until = *c.Until
	}
	switch {
	case !enabled && !u.paused:
		u.Log.WarnEvent(logging.EventControlGate, "updater paused by the remote configuration",
			"reason", reason, "until", until, "revision", cyc.snap.Revision(), "overrides", strings.Join(cyc.eff.Applied, ","))
	case enabled && u.paused:
		u.Log.WarnEvent(logging.EventControlGate, "updater resumed by the remote configuration",
			"revision", cyc.snap.Revision(), "overrides", strings.Join(cyc.eff.Applied, ","))
	}
	u.paused = !enabled
	return enabled
}

// policyView builds the IPC payload from the latest cycle (or, before the
// first cycle, from the current snapshot with no host facts).
func (u *Updater) policyView() *ipc.PolicyView {
	if u.Policy == nil {
		return nil
	}
	now := u.clock()
	cyc := u.cur.Load()
	var snap *policy.Snapshot
	var eff *policy.Effective
	var host policy.Host
	if cyc != nil {
		snap, eff, host = cyc.snap, cyc.eff, cyc.host
	} else {
		snap = u.Policy.Current()
		host = policy.Host{HWID: u.Machine.HWID, Hostname: u.Machine.Hostname, Now: now}
		e, err := snap.Parsed.Effective(host)
		if err != nil {
			u.Log.Warn("effective policy for IPC fell back to the global document", "error", err.Error())
		}
		eff = e
	}

	// What EMLy gets is the effective document with the expiring blocks
	// already resolved and the override list dropped: they have been
	// applied, and EMLy has no use for the selectors.
	doc := *eff.Doc
	doc.Overrides = nil
	if on, _ := eff.UpdaterEnabled(now); on {
		doc.Control.Updater = policy.ControlUpdater{Enabled: true}
	}
	doc.Control.App = eff.AppControl(now)
	raw, err := json.Marshal(&doc)
	if err != nil {
		u.Log.Warn("could not serialise the effective policy for IPC", "error", err.Error())
		return nil
	}

	src := ipcpb.ConfigResponse_DEFAULT
	switch snap.Source {
	case policy.SourceCache:
		src = ipcpb.ConfigResponse_CACHE
	case policy.SourceRemote:
		src = ipcpb.ConfigResponse_REMOTE
	}
	return &ipc.PolicyView{
		DocumentJSON:    raw,
		Revision:        snap.Revision(),
		GeneratedAt:     snap.GeneratedAt(),
		FetchedAt:       snap.FetchedAt,
		Source:          src,
		Stale:           snap.Stale(now),
		HostWhitelisted: eff.IsWhitelisted(host),
		IPC:             eff.Doc.IPCProtocol,
	}
}

// describeChain renders the server chain for the log: "name(url), ...".
func describeChain(eff *policy.Effective, chain []string) string {
	parts := make([]string, 0, len(chain))
	for _, name := range chain {
		parts = append(parts, fmt.Sprintf("%s(%s)", name, eff.ManifestURL(name)))
	}
	return strings.Join(parts, ", ")
}
