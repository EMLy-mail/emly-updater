package service

import (
	"context"
	"errors"
	"runtime/debug"
	"slices"
	"time"

	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/process"
	"emlyupdater/internal/state"
	"emlyupdater/internal/version"
	"emlyupdater/internal/wsclient"
)

// recoverGoroutine recovers a panic in one of the goroutines started for
// remote-triggered work - a command (executeCommand, clientcmd.go), the
// welcome burst (below) or a notify's delayed wake (scheduleWake,
// clientnotify.go) - and logs it instead of taking the whole service down.
// None of these run on RunLoop's own goroutine, so an unhandled panic here
// would otherwise crash the process over a single bad command, a session
// change, or a redelivered notify that only this one machine ever triggers -
// out of proportion for work the rest of the update cycle does not depend
// on. where and kv are only used when a panic actually happened.
func (u *Updater) recoverGoroutine(where string, kv ...any) {
	if r := recover(); r != nil {
		kv = append(kv, "panic", r, "stack", string(debug.Stack()))
		u.Log.Error("recovered from a panic in "+where, kv...)
	}
}

// eventBufferSize bounds the events kept while no v2 session is up. They
// are flushed after service.started on the next welcome; the oldest go
// first when it fills. In memory only: a service restart loses them, and the
// poll path still carries everything that matters (spec §2.3).
const eventBufferSize = 32

// bootWindow is how soon after boot a service start counts as "boot".
const bootWindow = 10 * time.Minute

type bufferedEvent struct {
	name    string
	payload any
}

// capabilities is what this build implements (spec §4): every command,
// event and topic of the wire list. Whether a command may run here is the
// policy's call, answered per command with disabled_by_policy.
//
// Deduplicated, order preserved: "machine.info" names both a command and an
// event, and the wire list must not repeat a capability the server already
// has in accepted_capabilities.
func (u *Updater) capabilities() []string {
	all := slices.Concat(wsclient.CommandNames, wsclient.EventNames, wsclient.TopicNames)
	seen := make(map[string]bool, len(all))
	out := make([]string, 0, len(all))
	for _, c := range all {
		if seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

// emit sends an event on the current v2 session, or buffers it.
//
// Two events never reach the buffer: session.changed while no session is up
// (spec §8.1 - it only anticipates what the next poll's X-EMLy-LoggedUser*
// headers would carry anyway, so it is stale, not lost, by the time a
// channel reconnects) is dropped outright, and any event that a live
// session refused as ErrTooLarge is dropped rather than kept for a retry
// that can only ever fail the same way again.
func (u *Updater) emit(name string, payload any) {
	if u.emitFn != nil {
		u.emitFn(name, payload)
		return
	}
	s := u.wsSession.Load()
	if s == nil {
		if name == wsclient.EvtSessionChanged {
			u.Log.Debug("no client channel session, session.changed dropped (not buffered)", "name", name)
			return
		}
	} else {
		if !s.Accepted(name) {
			u.Log.Debug("event not accepted by the server, dropped", "name", name)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := s.Event(ctx, name, payload)
		if err == nil {
			return
		}
		if errors.Is(err, wsclient.ErrTooLarge) {
			u.Log.Warn("event too large to send, dropped", "name", name)
			return
		}
		// Connection going away: fall through and keep it for the next one.
	}
	u.eventsMu.Lock()
	defer u.eventsMu.Unlock()
	u.eventBuf = append(u.eventBuf, bufferedEvent{name, payload})
	if len(u.eventBuf) > eventBufferSize {
		u.eventBuf = u.eventBuf[len(u.eventBuf)-eventBufferSize:]
	}
}

// flushEvents hands the buffered events to send, oldest first, and empties
// the buffer. A failed send stops the flush and keeps the rest - except an
// ErrTooLarge event, which is dropped instead of kept at the head: retrying
// it would only reproduce the same failure and block every event queued
// behind it forever.
func (u *Updater) flushEvents(send func(name string, payload any) error) {
	u.eventsMu.Lock()
	buf := u.eventBuf
	u.eventBuf = nil
	u.eventsMu.Unlock()
	for i, ev := range buf {
		err := send(ev.name, ev.payload)
		if err == nil {
			continue
		}
		if errors.Is(err, wsclient.ErrTooLarge) {
			u.Log.Warn("buffered event too large to send, dropped", "name", ev.name)
			continue
		}
		u.eventsMu.Lock()
		u.eventBuf = append(buf[i:], u.eventBuf...)
		u.eventsMu.Unlock()
		return
	}
}

// clientHandler is the service side of wsclient.Handler. gen ties a
// connection's Welcome - and the burst goroutine it starts - to the specific
// connection attempt runClientWS made it for: see storeWSSession /
// clearWSSession.
type clientHandler struct {
	u   *Updater
	gen uint64
}

// Welcome must return immediately: it runs on the connection's own read
// goroutine, the same one that answers the server's pings, and the burst of
// work a welcome triggers - sending service.started, flushing the buffer,
// sending machine.info, up to 34 outbound writes plus the WTS/registry/disk
// reads machineInfo does - can each take up to the per-send 10s timeout,
// which would starve pongs well past the server's ~20s idle timeout if run
// synchronously here. The actual work happens in welcomeBurst, on its own
// goroutine.
func (h clientHandler) Welcome(s *wsclient.Session, _ wsclient.Welcome) {
	go h.u.welcomeBurst(h.gen, s)
}

func (h clientHandler) Command(ctx context.Context, s *wsclient.Session, msg wsclient.Message, cmd wsclient.Command) {
	h.u.executeCommand(ctx, s, msg, cmd)
}

func (h clientHandler) Notify(_ *wsclient.Session, _ wsclient.Message, n wsclient.Notify) {
	h.u.handleNotify(n)
}

// welcomeBurst runs the work Welcome defers to its own goroutine. Tests
// override it wholesale via welcomeBurstFn to verify Welcome itself never
// blocks, without needing a real *wsclient.Session.
func (u *Updater) welcomeBurst(gen uint64, s *wsclient.Session) {
	defer u.recoverGoroutine("welcomeBurst")
	if u.welcomeBurstFn != nil {
		u.welcomeBurstFn(gen, s)
		return
	}
	u.handleWelcomeBurst(gen, s)
}

// handleWelcomeBurst is the real welcome burst. Order matters: service.started
// first (so a machine that restarted for a client-channel command reports it
// before anything else), then the pre-connection buffer, then - only once
// that first flush is done - the session is published for emit to use, so an
// event emitted while the burst is still running cannot reach the server
// ahead of service.started; a second flush then picks up anything emitted
// during the gap between the two; finally machine.info.
func (u *Updater) handleWelcomeBurst(gen uint64, s *wsclient.Session) {
	send := u.wsSend(s)
	if u.startedSent.CompareAndSwap(false, true) {
		if err := send(wsclient.EvtServiceStarted, u.cachedServiceStarted()); err != nil {
			u.startedSent.Store(false)
		}
	}
	u.flushEvents(send)
	u.storeWSSession(gen, s)
	u.flushEvents(send)
	_ = send(wsclient.EvtMachineInfo, u.machineInfo(nil))
}

// wsSend returns the function that writes one v2 event on s, gated on the
// server having accepted that capability in welcome, each bounded by a 10s
// timeout. Tests override the whole thing via wsSendFn, so a failure (and
// the service.started retry it causes) can be exercised without a real
// connection.
func (u *Updater) wsSend(s *wsclient.Session) func(name string, payload any) error {
	if u.wsSendFn != nil {
		return u.wsSendFn
	}
	return func(name string, payload any) error {
		if !s.Accepted(name) {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.Event(ctx, name, payload)
	}
}

// nextWSGeneration starts a new presence-channel connection attempt's
// generation. runClientWS calls it once per attempt, before constructing the
// wsclient.Client; the value it returns ties that attempt's Welcome (and the
// burst goroutine it starts) to storeWSSession/clearWSSession below.
func (u *Updater) nextWSGeneration() uint64 {
	u.wsSessionMu.Lock()
	defer u.wsSessionMu.Unlock()
	u.wsGen++
	return u.wsGen
}

// storeWSSession publishes s as the session emit uses, but only if gen is
// still the current connection attempt.
//
// Without this check, a Welcome burst goroutine from a connection
// runClientWS has already torn down - a dropped connection, or a
// policy-driven move to a different server - could still be running (e.g.
// blocked on a slow send) and store a session object for a connection that
// no longer exists; a later emit would then try to write to it and only
// fall back to the buffer once that write timed out.
func (u *Updater) storeWSSession(gen uint64, s *wsclient.Session) {
	u.wsSessionMu.Lock()
	defer u.wsSessionMu.Unlock()
	if u.wsGen == gen {
		u.wsSession.Store(s)
	}
}

// clearWSSession retires gen's connection: it always clears wsSession (the
// connection really did end), and - if this is still the current generation
// - also advances it, so a storeWSSession call still in flight from this same
// connection's Welcome burst is permanently locked out, even during the
// idle/backoff window before the next attempt calls nextWSGeneration (i.e.
// before the generation would otherwise change on its own).
func (u *Updater) clearWSSession(gen uint64) {
	u.wsSessionMu.Lock()
	defer u.wsSessionMu.Unlock()
	if u.wsGen == gen {
		u.wsGen++
	}
	u.wsSession.Store(nil)
}

// cachedServiceStarted builds service.started (CLIENT_WS_PROTOCOL.md §8.6)
// at most once per process and returns the same value on every later call.
//
// buildServiceStarted's TakePendingCommands is destructive: building the
// payload twice would report the pending restart/reboot command ids on
// whichever attempt happened to consume them and nothing on every other one
// - in particular, the retry after a failed send (see handleWelcomeBurst)
// would silently report no completed_commands, and possibly a different
// reason, from the one the first attempt would have sent.
func (u *Updater) cachedServiceStarted() wsclient.ServiceStarted {
	u.serviceStartedOnce.Do(func() {
		u.serviceStartedPayload = u.buildServiceStarted()
	})
	return u.serviceStartedPayload
}

// buildServiceStarted composes service.started, consuming the pending
// restart/reboot command ids. Only ever called once per process, through
// cachedServiceStarted's sync.Once.
func (u *Updater) buildServiceStarted() wsclient.ServiceStarted {
	now := u.clock()
	facts := u.systemFacts(now)
	cmds, err := u.Store.TakePendingCommands()
	if err != nil {
		u.Log.Warn("could not read the pending command ids", "error", err.Error())
	}
	landed := u.selfLanded.Load()
	p := wsclient.ServiceStarted{
		Reason:         serviceStartedReason(cmds, landed, facts.BootTime, u.startedAt),
		StartedAt:      u.startedAt.UTC().Format(time.RFC3339),
		UpdaterVersion: version.Version,
	}
	if !facts.BootTime.IsZero() {
		p.BootTime = facts.BootTime.Format(time.RFC3339)
	}
	if landed != nil && landed.FromVersion != version.Version {
		p.PreviousVersion = landed.FromVersion
	}
	for _, c := range cmds {
		p.CompletedCommands = append(p.CompletedCommands, c.ID)
	}
	return p
}

func serviceStartedReason(cmds []state.PendingCommand, landed *state.SelfUpdate, boot, started time.Time) string {
	for _, c := range cmds {
		if c.Name == wsclient.CmdMachineReboot {
			return "boot"
		}
	}
	for _, c := range cmds {
		if c.Name == wsclient.CmdServiceRestart {
			return "command"
		}
	}
	switch {
	case landed != nil:
		return "self_update"
	case !boot.IsZero() && started.Sub(boot) < bootWindow:
		return "boot"
	default:
		return "unknown"
	}
}

func (u *Updater) systemFacts(now time.Time) machineinfo.SystemFacts {
	if u.systemFactsFn != nil {
		return u.systemFactsFn(now)
	}
	return machineinfo.CollectSystemFacts(now)
}

func sessionChangedPayload(kinds []machineinfo.SessionChangeKind, c machineinfo.SessionChange, changed bool) wsclient.SessionChanged {
	p := wsclient.SessionChanged{
		Events:      make([]string, 0, len(kinds)),
		SessionID:   c.SessionID,
		SessionUser: c.SessionUser,
		Changed:     changed,
	}
	for _, k := range kinds {
		p.Events = append(p.Events, string(k))
	}
	if !c.At.IsZero() {
		p.At = c.At.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	}
	if c.LoggedUser.User != "" {
		p.LoggedUser = userSession(c.LoggedUser)
	}
	return p
}

func userSession(s machineinfo.UserSession) *wsclient.UserSession {
	out := &wsclient.UserSession{User: s.User, State: string(s.State)}
	if !s.DisconnectedAt.IsZero() {
		out.DisconnectedAt = s.DisconnectedAt.UTC().Format(time.RFC3339)
	}
	return out
}

type machineInfoPayload struct {
	HWID           string                `json:"hwid,omitempty"`
	Hostname       string                `json:"hostname,omitempty"`
	ADDomain       string                `json:"ad_domain,omitempty"`
	Serial         string                `json:"serial,omitempty"`
	Product        string                `json:"product,omitempty"`
	OSVersion      string                `json:"os_version,omitempty"`
	EMLyVersion    string                `json:"emly_version,omitempty"`
	UpdaterVersion string                `json:"updater_version,omitempty"`
	LoggedUser     *wsclient.UserSession `json:"logged_user,omitempty"`
	BootTime       string                `json:"boot_time,omitempty"`
	UptimeSeconds  int64                 `json:"uptime_seconds,omitempty"`
	Network        *networkInfo          `json:"network,omitempty"`
	Site           *siteInfo             `json:"site,omitempty"`
	Config         *configInfo           `json:"config,omitempty"`
	Hardware       *hardwareInfo         `json:"hardware,omitempty"`
	EMLy           *emlyInfo             `json:"emly,omitempty"`
	PendingUpdate  *wsclient.PendingInfo `json:"pending_update,omitempty"`
	SelfUpdate     *selfUpdateInfo       `json:"self_update,omitempty"`
}

type networkInfo struct {
	Interfaces []machineinfo.NetInterface `json:"interfaces"`
}
type siteInfo struct {
	DC          string              `json:"dc,omitempty"`
	MatchedSite string              `json:"matched_site,omitempty"`
	Server      *wsclient.ServerRef `json:"server,omitempty"`
}
type configInfo struct {
	Revision  int64  `json:"revision"`
	FetchedAt string `json:"fetched_at,omitempty"`
	Source    string `json:"source"`
}
type hardwareInfo struct {
	CPU         string     `json:"cpu,omitempty"`
	Cores       int        `json:"cores,omitempty"`
	MemoryMB    uint64     `json:"memory_mb,omitempty"`
	SystemDrive *driveInfo `json:"system_drive,omitempty"`
}
type driveInfo struct {
	TotalMB uint64 `json:"total_mb"`
	FreeMB  uint64 `json:"free_mb"`
}
type emlyInfo struct {
	Installed  bool   `json:"installed"`
	Running    bool   `json:"running"`
	InstallDir string `json:"install_dir,omitempty"`
	Channel    string `json:"channel,omitempty"`
	Language   string `json:"language,omitempty"`
}
type selfUpdateInfo struct {
	Version  string `json:"version"`
	Attempts int    `json:"attempts"`
	GaveUp   bool   `json:"gave_up,omitempty"`
}

// machineInfo builds spec §6.5. sections nil = everything. It reads the
// current cycle through u.cur (atomic) and never u.site, which the poll
// goroutine mutates.
func (u *Updater) machineInfo(sections []string) machineInfoPayload {
	want := func(s string) bool { return len(sections) == 0 || slices.Contains(sections, s) }
	var p machineInfoPayload
	now := u.clock()

	if want("identity") {
		src := u.newHTTPSource("")
		p.HWID, p.Hostname, p.ADDomain = src.HWID, src.Hostname, src.ADDomain
		p.Serial, p.Product, p.OSVersion = src.Serial, src.Product, src.OSVersion
		p.EMLyVersion, p.UpdaterVersion = src.EMLyVersion, version.Version
	}
	if want("logged_user") {
		if s := u.loggedUser(); s.User != "" {
			p.LoggedUser = userSession(s)
		}
	}
	if want("hardware") || want("identity") {
		f := u.systemFacts(now)
		if !f.BootTime.IsZero() {
			p.BootTime = f.BootTime.Format(time.RFC3339)
			p.UptimeSeconds = int64(now.Sub(f.BootTime).Seconds())
		}
		if want("hardware") {
			p.Hardware = &hardwareInfo{CPU: f.CPU, Cores: f.Cores, MemoryMB: f.MemoryMB}
			if f.SystemDriveTotalMB > 0 {
				p.Hardware.SystemDrive = &driveInfo{TotalMB: f.SystemDriveTotalMB, FreeMB: f.SystemDriveFreeMB}
			}
		}
	}
	if want("network") {
		p.Network = &networkInfo{Interfaces: u.netInterfaces()}
	}
	if cyc := u.cur.Load(); cyc != nil {
		if want("site") {
			p.Site = &siteInfo{DC: cyc.host.DC, MatchedSite: cyc.site}
			if chain := u.preferredChain(cyc); len(chain) > 0 {
				p.Site.Server = u.serverRef(cyc, cyc.eff.ManifestURL(chain[0]))
			}
		}
		if want("config") {
			src := cyc.snap.Source.String()
			if src == "default" {
				src = "legacy"
			}
			p.Config = &configInfo{Revision: cyc.snap.Revision(), Source: src}
			if !cyc.snap.FetchedAt.IsZero() {
				p.Config.FetchedAt = cyc.snap.FetchedAt.UTC().Format(time.RFC3339)
			}
		}
	}
	if want("emly") {
		info := u.Cfg.ResolveEMLy()
		p.EMLy = &emlyInfo{Installed: !info.FreshInstall, Running: u.emlyRunning(),
			InstallDir: u.Cfg.EMLyInstallDir, Channel: info.Channel, Language: info.Language}
	}
	if want("update") {
		if st, err := u.Store.Load(); err == nil {
			if st.Pending != nil {
				p.PendingUpdate = &wsclient.PendingInfo{Version: st.Pending.Version, Forced: st.Pending.Forced,
					DownloadedAt: st.Pending.DownloadedAt.UTC().Format(time.RFC3339)}
			}
			if st.SelfUpdate != nil {
				p.SelfUpdate = &selfUpdateInfo{Version: st.SelfUpdate.Version, Attempts: st.SelfUpdate.Attempts, GaveUp: st.SelfUpdate.GaveUp}
			}
		}
	}
	return p
}

// serverRef names the role of the server that answered (spec §6.2).
func (u *Updater) serverRef(cyc *cycleState, url string) *wsclient.ServerRef {
	if url == "" {
		return nil
	}
	role := "backup"
	switch {
	case cyc.site == "":
		role = "default"
	case len(cyc.chain) > 0 && url == cyc.eff.ManifestURL(cyc.chain[0]):
		role = "primary"
	}
	return &wsclient.ServerRef{URL: url, Role: role}
}

func (u *Updater) netInterfaces() []machineinfo.NetInterface {
	if u.netInterfacesFn != nil {
		return u.netInterfacesFn()
	}
	return machineinfo.NetworkInterfaces()
}

func (u *Updater) emlyRunning() bool {
	if u.emlyRunningFn != nil {
		return u.emlyRunningFn()
	}
	return process.IsRunning(u.Cfg.EMLyExeName)
}
