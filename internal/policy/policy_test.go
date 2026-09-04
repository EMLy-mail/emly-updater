package policy

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"emlyupdater/internal/config"
)

// fixtureDir is the conformance fixture set shared verbatim with the API
// repo (testdata/remoteconfig there too): the same files must pass on both
// sides, which is what keeps the two validators equal.
const fixtureDir = "../../testdata/remoteconfig"

func fixtureDefaults(t *testing.T) Defaults {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir, "defaults.json"))
	if err != nil {
		t.Fatalf("read defaults: %v", err)
	}
	var d Defaults
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatalf("decode defaults: %v", err)
	}
	return d
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func listFixtures(t *testing.T, sub, suffix string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(fixtureDir, sub, "*"+suffix))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no fixtures under %s/%s: %v", fixtureDir, sub, err)
	}
	sort.Strings(matches)
	return matches
}

func TestValidFixtures(t *testing.T) {
	defaults := fixtureDefaults(t)
	for _, path := range listFixtures(t, "valid", ".json") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			p, probs := Parse(readFixture(t, path), defaults)
			if probs != nil {
				t.Fatalf("expected valid, got: %v", probs)
			}
			if p.Global.SchemaVersion != SchemaVersion {
				t.Errorf("schemaVersion = %d", p.Global.SchemaVersion)
			}
		})
	}
}

func TestInvalidFixtures(t *testing.T) {
	defaults := fixtureDefaults(t)
	for _, path := range listFixtures(t, "invalid", ".problems.json") {
		docPath := strings.TrimSuffix(path, ".problems.json") + ".json"
		t.Run(filepath.Base(docPath), func(t *testing.T) {
			var expected []string
			if err := json.Unmarshal(readFixture(t, path), &expected); err != nil {
				t.Fatal(err)
			}
			_, probs := Parse(readFixture(t, docPath), defaults)
			if probs == nil {
				t.Fatalf("expected problems %v, document was accepted", expected)
			}
			// Every expected path must be reported (as itself or as a prefix
			// of a more specific path), and nothing else may be.
			for _, want := range expected {
				found := false
				for _, p := range probs {
					if strings.HasPrefix(p.Path, want) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected a problem at %s, got %v", want, probs)
				}
			}
			for _, p := range probs {
				covered := false
				for _, want := range expected {
					if strings.HasPrefix(p.Path, want) {
						covered = true
						break
					}
				}
				if !covered {
					t.Errorf("unexpected problem %s (expected only %v)", p, expected)
				}
			}
		})
	}
}

type effectiveFixture struct {
	Document json.RawMessage `json:"document"`
	Host     struct {
		HWID     string   `json:"hwid"`
		Hostname string   `json:"hostname"`
		DC       string   `json:"dc"`
		IPs      []string `json:"ips"`
		Domain   string   `json:"domain"`
		Now      string   `json:"now"`
	} `json:"host"`
	Expect struct {
		Applied             []string `json:"applied"`
		Site                string   `json:"site"`
		Chain               []string `json:"chain"`
		UpdaterEnabled      bool     `json:"updaterEnabled"`
		PollIntervalMinutes int      `json:"pollIntervalMinutes"`
		ChannelOverride     string   `json:"channelOverride"`
		LoggingLevel        string   `json:"loggingLevel"`
		Whitelisted         bool     `json:"whitelisted"`
	} `json:"expect"`
}

func TestEffectiveFixtures(t *testing.T) {
	defaults := fixtureDefaults(t)
	for _, path := range listFixtures(t, "effective", ".json") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			var fx effectiveFixture
			if err := json.Unmarshal(readFixture(t, path), &fx); err != nil {
				t.Fatal(err)
			}
			p, probs := Parse(fx.Document, defaults)
			if probs != nil {
				t.Fatalf("fixture document invalid: %v", probs)
			}
			now, err := time.Parse(time.RFC3339, fx.Host.Now)
			if err != nil {
				t.Fatal(err)
			}
			h := Host{HWID: fx.Host.HWID, Hostname: fx.Host.Hostname, DC: fx.Host.DC,
				IPs: fx.Host.IPs, Domain: fx.Host.Domain, Now: now}

			eff, err := p.Effective(h)
			if err != nil {
				t.Fatalf("Effective: %v", err)
			}
			if got, want := strings.Join(eff.Applied, ","), strings.Join(fx.Expect.Applied, ","); got != want {
				t.Errorf("applied = [%s], want [%s]", got, want)
			}
			site, chain := eff.Chain(h)
			if site != fx.Expect.Site {
				t.Errorf("site = %q, want %q", site, fx.Expect.Site)
			}
			if got, want := strings.Join(chain, ","), strings.Join(fx.Expect.Chain, ","); got != want {
				t.Errorf("chain = [%s], want [%s]", got, want)
			}
			if enabled, _ := eff.UpdaterEnabled(now); enabled != fx.Expect.UpdaterEnabled {
				t.Errorf("updaterEnabled = %v, want %v", enabled, fx.Expect.UpdaterEnabled)
			}
			if got := eff.Doc.Updater.PollIntervalMinutes; got != fx.Expect.PollIntervalMinutes {
				t.Errorf("pollIntervalMinutes = %d, want %d", got, fx.Expect.PollIntervalMinutes)
			}
			if got := eff.Doc.Updater.Channel(); got != fx.Expect.ChannelOverride {
				t.Errorf("channelOverride = %q, want %q", got, fx.Expect.ChannelOverride)
			}
			if got := eff.Doc.Logging.Level; got != fx.Expect.LoggingLevel {
				t.Errorf("logging.level = %q, want %q", got, fx.Expect.LoggingLevel)
			}
			if got := eff.IsWhitelisted(h); got != fx.Expect.Whitelisted {
				t.Errorf("whitelisted = %v, want %v", got, fx.Expect.Whitelisted)
			}
		})
	}
}

// The global document must survive Effective untouched: an override's
// patch is applied to a copy, never to the raw sections it was parsed from.
func TestEffectiveDoesNotMutateGlobal(t *testing.T) {
	defaults := fixtureDefaults(t)
	p, probs := Parse(readFixture(t, filepath.Join(fixtureDir, "valid", "full.json")), defaults)
	if probs != nil {
		t.Fatal(probs)
	}
	now, _ := time.Parse(time.RFC3339, "2026-09-05T09:00:00Z")
	h := Host{HWID: "9A3F1C77-0000-0000-0000-000000000001", Now: now}
	for range 3 {
		eff, err := p.Effective(h)
		if err != nil {
			t.Fatal(err)
		}
		if eff.Doc.Updater.Channel() != "beta" {
			t.Fatalf("override not applied on repeated evaluation")
		}
		if p.Global.Updater.Channel() != "" {
			t.Fatalf("global document mutated by Effective")
		}
	}
	unmatched, _ := p.Effective(Host{Now: now})
	if unmatched.Doc.Updater.PollIntervalMinutes != 5 {
		// rollout-window is `all` and still active on the 5th.
		t.Fatalf("all-override not applied to an anonymous host")
	}
}

func TestMergePatch(t *testing.T) {
	target := map[string]any{
		"a": map[string]any{"x": 1.0, "y": 2.0},
		"b": []any{1.0, 2.0},
		"c": "keep",
	}
	patch := map[string]any{
		"a": map[string]any{"y": nil, "z": 3.0},
		"b": []any{9.0},
		"d": map[string]any{"n": nil},
	}
	got := mergePatch(target, patch).(map[string]any)
	a := got["a"].(map[string]any)
	if a["x"] != 1.0 || a["z"] != 3.0 {
		t.Errorf("nested merge wrong: %v", a)
	}
	if _, ok := a["y"]; ok {
		t.Error("null did not delete a.y")
	}
	if b := got["b"].([]any); len(b) != 1 || b[0] != 9.0 {
		t.Errorf("array not replaced: %v", b)
	}
	if got["c"] != "keep" {
		t.Error("untouched key changed")
	}
	if d := got["d"].(map[string]any); len(d) != 0 {
		t.Errorf("null inside a new object should yield an empty object, got %v", d)
	}
	if target["a"].(map[string]any)["y"] != 2.0 {
		t.Error("target mutated")
	}
}

func TestSelectorMatching(t *testing.T) {
	h := Host{HWID: "abc-1", Hostname: "RM095", DC: "dc-rm2.tregcc.local", Domain: "tregcc.local", IPs: []string{"172.16.96.5", "10.8.0.2"}}
	cases := []struct {
		name string
		sel  Selector
		want bool
	}{
		{"all", Selector{All: true}, true},
		{"hwid case-insensitive", Selector{HWIDs: []string{"ABC-1"}}, true},
		{"hostname", Selector{Hostnames: []string{"rm095"}}, true},
		{"dc without suffix", Selector{DCs: []string{"DC-RM2"}}, true},
		{"dc other", Selector{DCs: []string{"DC-CB"}}, false},
		{"domain", Selector{Domains: []string{"TREGCC.LOCAL"}}, true},
		{"subnet second ip", Selector{Subnets: []string{"10.8.0.0/24"}}, true},
		{"subnet miss", Selector{Subnets: []string{"192.168.0.0/16"}}, false},
		{"and across keys, one fails", Selector{DCs: []string{"DC-RM2"}, Subnets: []string{"192.168.0.0/16"}}, false},
		{"and across keys, both pass", Selector{DCs: []string{"DC-RM2"}, Hostnames: []string{"RM095"}}, true},
		{"or within a list", Selector{Hostnames: []string{"OTHER", "rm095"}}, true},
	}
	for _, c := range cases {
		if got := c.sel.matches(h); got != c.want {
			t.Errorf("%s: matches = %v, want %v", c.name, got, c.want)
		}
	}
	off := Host{HWID: "x", IPs: []string{"1.2.3.4"}}
	if (Selector{DCs: []string{"DC-RM2"}}).matches(off) {
		t.Error("an off-domain host must never match a dcs selector")
	}
	if (Selector{Domains: []string{"tregcc.local"}}).matches(off) {
		t.Error("an off-domain host must never match a domains selector")
	}
}

func TestControlExpiry(t *testing.T) {
	past, future := "2026-01-01T00:00:00Z", "2030-01-01T00:00:00Z"
	now, _ := time.Parse(time.RFC3339, "2026-09-05T00:00:00Z")
	reason := "freeze"
	e := &Effective{Doc: &Document{Control: Control{
		Updater: ControlUpdater{Enabled: false, Reason: &reason, Until: &past},
		App:     ControlApp{Enabled: false, Mode: AppModeMaintenance, Until: &past},
	}}}
	if on, _ := e.UpdaterEnabled(now); !on {
		t.Error("an expired updater block must read as enabled")
	}
	if app := e.AppControl(now); !app.Enabled || app.Mode != AppModeNormal {
		t.Errorf("an expired app block must read as enabled/normal, got %+v", app)
	}
	e.Doc.Control.Updater.Until = &future
	if on, r := e.UpdaterEnabled(now); on || r != reason {
		t.Errorf("a live block must stay off with its reason, got %v %q", on, r)
	}
	e.Doc.Control.Updater.Until = nil
	if on, _ := e.UpdaterEnabled(now); on {
		t.Error("a block with no until stays until the next push")
	}
}

func legacyConfig(t *testing.T) *config.Config {
	t.Helper()
	dcs, err := config.ParseDCSubnetMap("DC-RM2:172.16.96.0/24,10.12.8.0/24|DC-CB:172.16.33.0/24")
	if err != nil {
		t.Fatal(err)
	}
	return &config.Config{
		Primary:                   config.SourceInternal,
		ExternalManifestURL:       "https://api.emly.ffois.it/v2/updates/manifest",
		InternalManifestURL:       "http://172.16.33.72:8080/v2/updates/manifest",
		BackupInternalManifestURL: "http://172.23.85.160:8080/custom/manifest.json",
		DCSubnetMap:               dcs,
		PollInterval:              15 * time.Minute,
		CriticalWarningEnabled:    true,
		CriticalWarningSeconds:    30,
		DCLookupRetryAttempts:     6,
		DCLookupRetryDelay:        5 * time.Second,
		SelfUpdateEnabled:         true,
		CertificateEnabled:        true,
	}
}

// The default policy has to reproduce the resolver chain the legacy config
// produced: internal, then the backup internal, then external; and the
// public API for anyone not at a mapped site.
func TestFromLegacyReproducesTheOldChain(t *testing.T) {
	cfg := legacyConfig(t)
	p := FromLegacy(cfg, DefaultsFromConfig(cfg, fixtureDefaults(t).IPCProtocol, false))
	if probs := validateDocument(p.Global, false); len(probs) > 0 {
		t.Fatalf("default policy does not validate: %v", probs)
	}

	eff, err := p.Effective(Host{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	onSite := Host{DC: "dc-rm2.tregcc.local", IPs: []string{"10.12.8.4"}}
	site, chain := eff.Chain(onSite)
	if site != "DC-RM2" || strings.Join(chain, ",") != "internal,internalBackup,external" {
		t.Errorf("on-site chain = %s %v", site, chain)
	}
	if got := eff.ManifestURL(chain[0]); got != cfg.InternalManifestURL {
		t.Errorf("internal manifest URL = %q", got)
	}
	// A legacy URL off the standard path is preserved whole.
	if got := eff.ManifestURL(chain[1]); got != cfg.BackupInternalManifestURL {
		t.Errorf("backup manifest URL = %q, want the legacy URL kept verbatim", got)
	}
	if got := eff.ManifestURL(chain[2]); got != cfg.ExternalManifestURL {
		t.Errorf("external manifest URL = %q", got)
	}

	site, chain = eff.Chain(Host{DC: "DC-MI1", IPs: []string{"172.16.96.9"}})
	if site != "" || strings.Join(chain, ",") != "external" {
		t.Errorf("unmapped DC chain = %s %v, want just external", site, chain)
	}
	if p.Global.Updater.PollIntervalMinutes != 15 || !p.Global.Updater.InstallCertificate.Enabled {
		t.Errorf("updater settings not carried over: %+v", p.Global.Updater)
	}
}

func TestFromLegacyWithoutInternal(t *testing.T) {
	cfg := legacyConfig(t)
	cfg.Primary = config.SourceExternal
	cfg.InternalManifestURL = ""
	cfg.BackupInternalManifestURL = ""
	p := FromLegacy(cfg, DefaultsFromConfig(cfg, fixtureDefaults(t).IPCProtocol, false))
	if len(p.Global.DCLookupMap) != 0 {
		t.Errorf("sites without an internal server must be dropped, got %v", p.Global.DCLookupMap)
	}
	if p.Global.DefaultServer != LegacyExternal {
		t.Errorf("defaultServer = %q", p.Global.DefaultServer)
	}
	_, n, _ := net.ParseCIDR("172.16.96.0/24")
	_ = n
}

func TestCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "remote-config.json")
	if c, err := LoadCache(path); c != nil || err != nil {
		t.Fatalf("missing cache should be (nil, nil), got %v %v", c, err)
	}
	doc := readFixture(t, filepath.Join(fixtureDir, "valid", "full.json"))
	in := &CacheFile{FetchedAt: time.Now().Round(time.Second), FetchedFrom: "srv-cloud", ETag: `"abc"`, Document: doc}
	if err := SaveCache(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.ETag != in.ETag || out.FetchedFrom != in.FetchedFrom || !out.FetchedAt.Equal(in.FetchedAt) {
		t.Errorf("metadata changed: %+v", out)
	}
	if out.Revision() != 42 {
		t.Errorf("Revision() = %d", out.Revision())
	}
	if _, probs := Parse(out.Document, fixtureDefaults(t)); probs != nil {
		t.Errorf("cached document no longer parses: %v", probs)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("temp file left behind: %v", entries)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCache(path); err == nil {
		t.Fatal("corrupt cache must be reported")
	}
	bad := filepath.Join(dir, "remote-config.bad.json")
	if err := QuarantineCache(path, bad); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("quarantine left the corrupt cache in place")
	}
	if _, err := os.Stat(bad); err != nil {
		t.Error("quarantine did not keep a copy")
	}
}

func TestSnapshotStale(t *testing.T) {
	defaults := fixtureDefaults(t)
	p, probs := Parse(readFixture(t, filepath.Join(fixtureDir, "valid", "full.json")), defaults)
	if probs != nil {
		t.Fatal(probs)
	}
	now := time.Now()
	fresh := &Snapshot{Parsed: p, Source: SourceCache, FetchedAt: now.Add(-6 * 24 * time.Hour)}
	old := &Snapshot{Parsed: p, Source: SourceCache, FetchedAt: now.Add(-8 * 24 * time.Hour)}
	if fresh.Stale(now) {
		t.Error("6 days old with staleAfterDays 7 must not be stale")
	}
	if !old.Stale(now) {
		t.Error("8 days old with staleAfterDays 7 must be stale")
	}
	def := &Snapshot{Parsed: p, Source: SourceDefault}
	if def.Stale(now) {
		t.Error("the default policy is never stale")
	}
}
