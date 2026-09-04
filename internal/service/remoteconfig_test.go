package service

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"emlyupdater/internal/config"
	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/policy"
	"emlyupdater/internal/source"
)

var testNow = time.Date(2026, 9, 5, 9, 0, 0, 0, time.UTC)

// document builds a minimal valid document with the given revision and the
// extra top-level fields the case needs.
func document(t *testing.T, revision int64, extra map[string]any) []byte {
	t.Helper()
	doc := map[string]any{
		"schemaVersion": 1,
		"revision":      revision,
		"generatedAt":   "2026-09-01T00:00:00Z",
		"servers": map[string]any{
			"srv-cloud": "https://api.emly.ffois.it",
			"srv-site":  "http://172.16.96.73:8080",
		},
		"defaultServer": "srv-cloud",
	}
	for k, v := range extra {
		doc[k] = v
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// remoteUpdater is an Updater with remote configuration on, a pinned clock,
// a temp cache and a scripted fetch: no network, no ProgramData.
func remoteUpdater(t *testing.T, responses ...*source.ConfigResponse) (*Updater, *int) {
	t.Helper()
	cfg := internalCfg(t, config.SourceExternal)
	cfg.RemoteConfigEnabled = true
	cfg.RemoteConfigEndpoints = []string{"https://api.emly.ffois.it"}
	cfg.RemoteConfigRoute = "/v2/config"
	cfg.RemoteConfigTimeout = 5 * time.Second

	calls := 0
	u := &Updater{
		Cfg:       cfg,
		Log:       testLogger(t),
		Machine:   machineinfo.Info{Hostname: "RM095", HWID: "HW-1", ADDomain: "tregcc.local"},
		CachePath: filepath.Join(tempDir(t), "remote-config.json"),
		dcFn:      dcNamed("DC-RM2"),
		ipsFn:     ipsOf("172.16.96.50"),
		nowFn:     func() time.Time { return testNow },
	}
	u.fetchConfigFn = func(ctx context.Context, url, etag string) (*source.ConfigResponse, error) {
		i := calls
		calls++
		if i < len(responses) {
			if responses[i] == nil {
				return nil, http.ErrServerClosed
			}
			return responses[i], nil
		}
		return nil, http.ErrServerClosed
	}
	return u, &calls
}

// tempDir is not t.TempDir(): the cache is written and renamed repeatedly,
// and on Windows a file the scanner still holds open makes the automatic
// cleanup fail the test for a reason unrelated to what it asserts. Same
// reasoning as testLogger.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "emlyupdater-cache-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func ok200(body []byte, etag string) *source.ConfigResponse {
	return &source.ConfigResponse{Status: http.StatusOK, Body: body, ETag: etag}
}

// A document that validates becomes the policy, is written to the cache and
// drives the cycle: the site chain now comes from the document, not from
// config.ini.
func TestRefreshConfigAppliesAndCaches(t *testing.T) {
	doc := document(t, 42, map[string]any{
		"dcLookupMap": map[string]any{
			"DC-RM2": map[string]any{
				"internalSubnets": []string{"172.16.96.0/24"},
				"baseServer":      "srv-site",
				"backupServer":    []string{"srv-cloud"},
			},
		},
		"updater": map[string]any{"pollIntervalMinutes": 7},
	})
	u, calls := remoteUpdater(t, ok200(doc, `"v42"`))
	u.initPolicy()
	if got := u.Policy.Current().Source; got != policy.SourceDefault {
		t.Fatalf("before the first fetch the source is %v, want default", got)
	}

	u.refreshConfig(context.Background(), true)
	if *calls != 1 {
		t.Fatalf("fetch calls = %d, want 1", *calls)
	}
	snap := u.Policy.Current()
	if snap.Source != policy.SourceRemote || snap.Revision() != 42 || snap.ETag != `"v42"` {
		t.Fatalf("snapshot = %+v", snap)
	}

	cache, err := policy.LoadCache(u.CachePath)
	if err != nil || cache == nil {
		t.Fatalf("cache not written: %v", err)
	}
	if cache.Revision() != 42 || cache.ETag != `"v42"` || !cache.FetchedAt.Equal(testNow) {
		t.Errorf("cache = %+v", cache)
	}

	cyc := u.beginCycle(context.Background(), true)
	if cyc.site != "DC-RM2" || strings.Join(cyc.chain, ",") != "srv-site,srv-cloud" {
		t.Errorf("cycle site/chain = %q %v, want the document's own site", cyc.site, cyc.chain)
	}
	if got := cyc.eff.Doc.Updater.PollInterval(); got != 7*time.Minute {
		t.Errorf("poll interval = %v, want the document's 7m", got)
	}
}

// A cached document is the policy from the very first cycle, before any
// fetch: a machine that boots with no network still runs its real policy.
func TestInitPolicyLoadsTheCache(t *testing.T) {
	u, _ := remoteUpdater(t)
	if err := policy.SaveCache(u.CachePath, &policy.CacheFile{
		FetchedAt: testNow.Add(-time.Hour), FetchedFrom: "srv-cloud", ETag: `"v7"`,
		Document: document(t, 7, nil),
	}); err != nil {
		t.Fatal(err)
	}

	u.initPolicy()
	snap := u.Policy.Current()
	if snap.Source != policy.SourceCache || snap.Revision() != 7 {
		t.Fatalf("snapshot = %+v", snap)
	}
}

// A cache that no longer validates (a schema this build does not know, a
// truncated file) must not take the machine down: it is moved aside and the
// default policy carries the run.
func TestInitPolicyQuarantinesABadCache(t *testing.T) {
	u, _ := remoteUpdater(t)
	bad := u.CachePath + ".bad"

	if err := os.WriteFile(u.CachePath, []byte(`{"fetchedAt":"2026-09-01T00:00:00Z","document":{"schemaVersion":99}}`), 0644); err != nil {
		t.Fatal(err)
	}
	u.initPolicy()
	if got := u.Policy.Current().Source; got != policy.SourceDefault {
		t.Errorf("source = %v, want the default policy", got)
	}
	if _, err := os.Stat(bad); err != nil {
		t.Errorf("the rejected cache was not kept for inspection: %v", err)
	}
}

// 304 confirms the cached document: it stays in force, becomes "remote",
// and its fetchedAt moves so staleness is measured from the confirmation.
func TestRefreshConfigNotModifiedConfirmsTheCache(t *testing.T) {
	u, _ := remoteUpdater(t, &source.ConfigResponse{Status: http.StatusNotModified, ETag: `"v7"`})
	if err := policy.SaveCache(u.CachePath, &policy.CacheFile{
		FetchedAt: testNow.Add(-48 * time.Hour), FetchedFrom: "srv-cloud", ETag: `"v7"`,
		Document: document(t, 7, nil),
	}); err != nil {
		t.Fatal(err)
	}
	u.initPolicy()
	u.refreshConfig(context.Background(), true)

	snap := u.Policy.Current()
	if snap.Source != policy.SourceRemote || snap.Revision() != 7 {
		t.Errorf("snapshot = %+v", snap)
	}
	if !snap.FetchedAt.Equal(testNow) {
		t.Errorf("fetchedAt = %v, want it moved to the confirmation", snap.FetchedAt)
	}
}

// 204 means the server has nothing published: keep whatever policy is in
// force, and do not treat it as an outage.
func TestRefreshConfigNoContentKeepsThePolicy(t *testing.T) {
	u, _ := remoteUpdater(t, &source.ConfigResponse{Status: http.StatusNoContent})
	u.initPolicy()
	u.refreshConfig(context.Background(), true)

	if got := u.Policy.Current().Source; got != policy.SourceDefault {
		t.Errorf("source = %v, want the default policy kept", got)
	}
	if u.configUnreachable {
		t.Error("204 must not count as an outage")
	}
}

// An invalid document is rejected whole and the cached one stays in force.
func TestRefreshConfigRejectsAnInvalidDocument(t *testing.T) {
	bad := document(t, 43, map[string]any{"defaultServer": "srv-nope"})
	u, _ := remoteUpdater(t, ok200(bad, `"v43"`))
	if err := policy.SaveCache(u.CachePath, &policy.CacheFile{
		FetchedAt: testNow, FetchedFrom: "srv-cloud", ETag: `"v7"`,
		Document: document(t, 7, nil),
	}); err != nil {
		t.Fatal(err)
	}
	u.initPolicy()
	u.refreshConfig(context.Background(), true)

	if got := u.Policy.Current().Revision(); got != 7 {
		t.Errorf("revision = %d, want the cached 7 kept", got)
	}
	cache, _ := policy.LoadCache(u.CachePath)
	if cache.Revision() != 7 {
		t.Errorf("cache revision = %d, want it untouched", cache.Revision())
	}
}

// A mirror serving an older copy must not roll the machine back.
func TestRefreshConfigIgnoresAnOlderRevision(t *testing.T) {
	u, _ := remoteUpdater(t, ok200(document(t, 10, nil), `"v10"`), ok200(document(t, 4, nil), `"v4"`))
	u.initPolicy()
	u.refreshConfig(context.Background(), true)
	if got := u.Policy.Current().Revision(); got != 10 {
		t.Fatalf("revision = %d, want 10", got)
	}

	u.refreshConfig(context.Background(), true)
	if got := u.Policy.Current().Revision(); got != 10 {
		t.Errorf("revision = %d, want the older document ignored", got)
	}
}

// Every candidate failing keeps the policy and logs the outage once.
func TestRefreshConfigUnreachableKeepsThePolicy(t *testing.T) {
	u, calls := remoteUpdater(t, nil, nil)
	u.initPolicy()
	u.refreshConfig(context.Background(), true)

	if got := u.Policy.Current().Source; got != policy.SourceDefault {
		t.Errorf("source = %v, want the default policy kept", got)
	}
	if !u.configUnreachable {
		t.Error("the outage flag was not set")
	}
	if *calls == 0 {
		t.Error("no candidate was tried")
	}

	// A second round must not re-log: the flag stays until a fetch works.
	u.refreshConfig(context.Background(), true)
	if !u.configUnreachable {
		t.Error("the outage flag was cleared without a successful fetch")
	}
}

// The fetch is paced by refresh.intervalMinutes: a cycle that comes round
// again too soon does not ask the server.
func TestRefreshConfigRespectsTheInterval(t *testing.T) {
	doc := document(t, 1, map[string]any{"refresh": map[string]any{"intervalMinutes": 30}})
	u, calls := remoteUpdater(t, ok200(doc, `"v1"`), ok200(doc, `"v1"`))
	u.initPolicy()

	u.refreshConfig(context.Background(), true)
	u.refreshConfig(context.Background(), false)
	if *calls != 1 {
		t.Errorf("fetch calls = %d, want the interval to suppress the second", *calls)
	}

	u.nowFn = func() time.Time { return testNow.Add(31 * time.Minute) }
	u.refreshConfig(context.Background(), false)
	if *calls != 2 {
		t.Errorf("fetch calls = %d, want a fetch once the interval elapsed", *calls)
	}
}

// The servers the policy assigns to this machine are asked before the
// bootstrap endpoints, so a site keeps talking to its own mirror.
func TestConfigCandidatesPreferTheSiteServers(t *testing.T) {
	doc := document(t, 5, map[string]any{
		"dcLookupMap": map[string]any{
			"DC-RM2": map[string]any{
				"internalSubnets": []string{"172.16.96.0/24"},
				"baseServer":      "srv-site",
				"backupServer":    []string{"srv-cloud"},
			},
		},
	})
	u, _ := remoteUpdater(t, ok200(doc, `"v5"`))
	u.initPolicy()
	u.refreshConfig(context.Background(), true)
	u.beginCycle(context.Background(), true)

	var got []string
	for _, c := range u.configCandidates() {
		got = append(got, c.base)
	}
	want := "http://172.16.96.73:8080,https://api.emly.ffois.it"
	if strings.Join(got, ",") != want {
		t.Errorf("candidates = %v, want %q (site first, bootstrap deduplicated)", got, want)
	}
}

// control.updater.enabled = false pauses the update machinery. The gate is
// what Cycle checks; an expired `until` re-opens it on its own.
func TestControlGate(t *testing.T) {
	paused := document(t, 6, map[string]any{
		"control": map[string]any{"updater": map[string]any{
			"enabled": false, "reason": "chiusura contabile", "until": "2026-09-06T00:00:00Z"}},
	})
	u, _ := remoteUpdater(t, ok200(paused, `"v6"`))
	u.initPolicy()
	u.refreshConfig(context.Background(), true)

	cyc := u.beginCycle(context.Background(), true)
	if u.applyControlGate(cyc) {
		t.Fatal("the gate must be closed while the block is live")
	}
	if !u.paused {
		t.Error("paused flag not set")
	}

	// Past the expiry the same document reads as enabled.
	u.nowFn = func() time.Time { return testNow.Add(48 * time.Hour) }
	cyc = u.beginCycle(context.Background(), false)
	if !u.applyControlGate(cyc) {
		t.Error("an expired block must re-open the gate")
	}
	if u.paused {
		t.Error("paused flag not cleared")
	}
}

// A paused updater still refreshes its configuration - that is what can
// un-pause it - and still heals the trust store, but installs nothing.
func TestCycleStopsAtTheControlGate(t *testing.T) {
	paused := document(t, 6, map[string]any{
		"control": map[string]any{"updater": map[string]any{"enabled": false}},
		"updater": map[string]any{"installCertificate": map[string]any{"enabled": false}},
	})
	u, _ := remoteUpdater(t, ok200(paused, `"v6"`))
	u.Store = nil // any attempt to read state would panic: the gate must return first
	u.initPolicy()
	u.refreshConfig(context.Background(), true)

	if err := u.Cycle(context.Background(), u.beginCycle(context.Background(), true)); err != nil {
		t.Fatalf("a paused cycle must be a clean no-op, got %v", err)
	}
}

// The logging section is pushed into the running logger, and only when it
// actually changed.
func TestApplyLogging(t *testing.T) {
	u, _ := remoteUpdater(t)
	u.initPolicy()
	if got := u.Log.Level(); got != "info" {
		t.Fatalf("initial level = %q", got)
	}

	u.applyLogging(policy.LoggingSettings{Level: "debug", MaxSizeMB: 5, Backups: 2, Compress: true, EventLog: false})
	if got := u.Log.Level(); got != "debug" {
		t.Errorf("level = %q, want debug", got)
	}
	if u.appliedLogging == nil || u.appliedLogging.MaxSizeMB != 5 {
		t.Errorf("applied settings = %+v", u.appliedLogging)
	}

	// An unknown level leaves the current one alone rather than silencing
	// the log.
	u.applyLogging(policy.LoggingSettings{Level: "trace", MaxSizeMB: 5, Backups: 2, Compress: true, EventLog: false})
	if got := u.Log.Level(); got != "debug" {
		t.Errorf("level = %q, want the previous one kept", got)
	}
}

// What EMLy receives over IPC is the effective document for this host, with
// the overrides applied, the expiries resolved and the selectors dropped.
func TestPolicyViewForIPC(t *testing.T) {
	doc := document(t, 11, map[string]any{
		"hostIntegrity": map[string]any{
			"enabled":   true,
			"whitelist": map[string]any{"hostnames": []string{"rm095"}},
		},
		"overrides": []any{map[string]any{
			"id":    "pilot",
			"match": map[string]any{"hwids": []string{"hw-1"}},
			"patch": map[string]any{"updater": map[string]any{"channelOverride": "beta"}},
		}},
	})
	u, _ := remoteUpdater(t, ok200(doc, `"v11"`))
	u.initPolicy()
	u.refreshConfig(context.Background(), true)
	u.beginCycle(context.Background(), true)

	view := u.policyView()
	if view == nil {
		t.Fatal("no policy view")
	}
	if view.Revision != 11 || view.GeneratedAt != "2026-09-01T00:00:00Z" {
		t.Errorf("view = %+v", view)
	}
	if !view.HostWhitelisted {
		t.Error("the host is on the whitelist by hostname")
	}
	if !view.FetchedAt.Equal(testNow) {
		t.Errorf("fetchedAt = %v", view.FetchedAt)
	}

	var sent policy.Document
	if err := json.Unmarshal(view.DocumentJSON, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Updater.Channel() != "beta" {
		t.Errorf("the override was not applied for this host: %q", sent.Updater.Channel())
	}
	if len(sent.Overrides) != 0 {
		t.Errorf("the selectors must not be sent to EMLy: %v", sent.Overrides)
	}
}

// With remote configuration switched off in config.ini nothing is fetched
// and the legacy settings stand.
func TestRemoteConfigDisabled(t *testing.T) {
	u, calls := remoteUpdater(t, ok200(document(t, 1, nil), `"v1"`))
	u.Cfg.RemoteConfigEnabled = false
	u.initPolicy()
	u.refreshConfig(context.Background(), true)

	if *calls != 0 {
		t.Errorf("fetch calls = %d, want none", *calls)
	}
	if got := u.Policy.Current().Source; got != policy.SourceDefault {
		t.Errorf("source = %v, want the default policy", got)
	}
}

// A cache that cannot be written (read-only directory) must not cost the
// run its policy: it stays in memory and the write is retried next fetch.
func TestCacheWriteFailureKeepsThePolicyInMemory(t *testing.T) {
	u, _ := remoteUpdater(t, ok200(document(t, 12, nil), `"v12"`))
	// A path whose parent is a file, not a directory: MkdirAll fails.
	blocker := filepath.Join(tempDir(t), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	u.CachePath = filepath.Join(blocker, "remote-config.json")

	u.initPolicy()
	u.refreshConfig(context.Background(), true)

	snap := u.Policy.Current()
	if snap.Source != policy.SourceRemote || snap.Revision() != 12 {
		t.Errorf("snapshot = %+v, want the document applied despite the failed write", snap)
	}
}
