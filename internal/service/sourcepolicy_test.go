package service

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"emlyupdater/internal/config"
	"emlyupdater/internal/logging"
	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/policy"
)

// internalCfg mirrors a real deployed config: two office DCs, each with its
// own subnets, and every manifest URL populated so the default policy has a
// full chain to build.
func internalCfg(t *testing.T, primary string) *config.Config {
	t.Helper()
	dcSubnets, err := config.ParseDCSubnetMap("DC-RM2:172.16.96.0/24;DC-CB:172.16.33.0/24,172.16.34.0/24")
	if err != nil {
		t.Fatalf("ParseDCSubnetMap: %v", err)
	}
	return &config.Config{
		Primary:                primary,
		ExternalManifestURL:    "https://api.emly.ffois.it/v2/updates/manifest",
		InternalManifestURL:    "http://172.16.96.73:8080/v2/updates/manifest",
		DCSubnetMap:            dcSubnets,
		PollInterval:           15 * time.Minute,
		CriticalWarningEnabled: true,
		CriticalWarningSeconds: 30,
		DCLookupRetryAttempts:  6,
		DCLookupRetryDelay:     5 * time.Second,
		SelfUpdateEnabled:      true,
		CertificateEnabled:     true,
		EMLyInstallDir:         `C:\3gIT\EMLy`,
		EMLyExeName:            "EMLy.exe",
		EMLyConfigFile:         `C:\3gIT\EMLy\config.ini`,
	}
}

// newTestUpdater builds an Updater wired to fake machine facts and no
// network: remote configuration is off, so it runs the policy derived from
// config.ini - the behaviour every machine has until it first reaches the
// endpoint.
func newTestUpdater(t *testing.T, cfg *config.Config, dc dcLookup, ips localIPsLookup) *Updater {
	t.Helper()
	cfg.RemoteConfigEnabled = false
	if cfg.DCLookupRetryDelay == 5*time.Second {
		// The shipped default is 6 attempts 5 seconds apart, and the fake
		// machine here reports a domain, so a lookup that fails on purpose
		// would sit out the full 30s window. Tests that exercise the retry
		// set their own values before calling this.
		cfg.DCLookupRetryAttempts, cfg.DCLookupRetryDelay = 0, 0
	}
	u := &Updater{
		Cfg:     cfg,
		Log:     testLogger(t),
		Machine: machineinfo.Info{Hostname: "RM095", HWID: "HW-1", ADDomain: "tregcc.local"},
		dcFn:    dc,
		ipsFn:   ips,
	}
	u.initPolicy()
	return u
}

func dcNamed(name string) dcLookup {
	return func(string) (*machineinfo.DomainControllerInfo, error) {
		return &machineinfo.DomainControllerInfo{Name: name, Site: name}, nil
	}
}

func ipsOf(addrs ...string) localIPsLookup {
	return func() ([]string, error) { return addrs, nil }
}

// The site decision, expressed as the server chain a cycle would use. This
// replaces the old decideSource table: "internal" now means "the site's base
// server won", "external" means "no site matched, defaultServer".
func TestSiteDecision(t *testing.T) {
	cases := []struct {
		name      string
		dc        dcLookup
		ips       localIPsLookup
		wantSite  string
		wantChain string
	}{
		{
			name: "office DC, DNS name, matching local IP",
			dc:   dcNamed("DC-RM2.tregcc.local"), ips: ipsOf("172.16.96.50"),
			wantSite: "DC-RM2", wantChain: "internal,external",
		},
		{
			name: "office DC, NetBIOS name, matching local IP",
			dc:   dcNamed("DC-RM2"), ips: ipsOf("172.16.96.50"),
			wantSite: "DC-RM2", wantChain: "internal,external",
		},
		{
			name: "second site's DC and subnet",
			dc:   dcNamed("DC-CB.tregcc.local"), ips: ipsOf("172.16.34.10"),
			wantSite: "DC-CB", wantChain: "internal,external",
		},
		{
			name: "machine has multiple local IPs, only one matches",
			dc:   dcNamed("DC-RM2"), ips: ipsOf("10.0.0.5", "172.16.96.50"),
			wantSite: "DC-RM2", wantChain: "internal,external",
		},
		{
			name: "office DC seen, but no local IP on its subnets",
			dc:   dcNamed("DC-RM2.tregcc.local"), ips: ipsOf("10.12.8.253"),
			wantSite: "", wantChain: "external",
		},
		{
			name: "a domain controller with no configured mapping",
			dc:   dcNamed("DC-MI1.tregcc.local"), ips: ipsOf("172.16.96.50"),
			wantSite: "", wantChain: "external",
		},
		{
			name: "machine off the domain",
			dc: func(string) (*machineinfo.DomainControllerInfo, error) {
				return nil, errors.New("DsGetDcName failed: the specified domain does not exist")
			},
			ips:      ipsOf("172.16.96.50"),
			wantSite: "", wantChain: "external",
		},
		{
			name: "local IP enumeration fails",
			dc:   dcNamed("DC-RM2"),
			ips: func() ([]string, error) {
				return nil, errors.New("net.Interfaces: access denied")
			},
			wantSite: "", wantChain: "external",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := newTestUpdater(t, internalCfg(t, config.SourceInternal), c.dc, c.ips)
			cyc := u.beginCycle(context.Background(), true)
			if cyc.site != c.wantSite {
				t.Errorf("site = %q, want %q", cyc.site, c.wantSite)
			}
			if got := strings.Join(cyc.chain, ","); got != c.wantChain {
				t.Errorf("chain = %q, want %q", got, c.wantChain)
			}
		})
	}
}

// An empty mapping leaves every machine on the default server rather than
// inventing a site: an unconfigured policy is not an instruction.
func TestSiteDecisionWithoutAMapping(t *testing.T) {
	cfg := internalCfg(t, config.SourceInternal)
	cfg.DCSubnetMap = nil
	u := newTestUpdater(t, cfg, dcNamed("DC-RM2"), ipsOf("172.16.96.50"))

	cyc := u.beginCycle(context.Background(), true)
	if cyc.site != "" {
		t.Errorf("site = %q, want none", cyc.site)
	}
	// primary = internal with no mapping used to mean "internal, no
	// questions asked"; the default policy keeps that as defaultServer.
	if got := strings.Join(cyc.chain, ","); got != policy.LegacyInternal {
		t.Errorf("chain = %q, want %q", got, policy.LegacyInternal)
	}
}

// The resolver a cycle builds must follow the chain: the site's base server
// as primary, its backups as one-shot fallbacks, in order.
func TestResolverFollowsTheChain(t *testing.T) {
	cfg := internalCfg(t, config.SourceInternal)
	cfg.BackupInternalManifestURL = "http://172.23.85.160:8080/v2/updates/manifest"
	u := newTestUpdater(t, cfg, dcNamed("DC-RM2"), ipsOf("172.16.96.50"))

	cyc := u.beginCycle(context.Background(), true)
	r := u.newResolver(cyc)
	if got := r.Primary.Name(); !strings.Contains(got, cfg.InternalManifestURL) {
		t.Errorf("primary = %q, want the internal manifest URL", got)
	}
	if len(r.Fallbacks) != 2 {
		t.Fatalf("fallbacks = %d, want 2 (backup internal, then external)", len(r.Fallbacks))
	}
	if got := r.Fallbacks[0].Name(); !strings.Contains(got, cfg.BackupInternalManifestURL) {
		t.Errorf("first fallback = %q, want the backup internal URL", got)
	}
	if got := r.Fallbacks[1].Name(); !strings.Contains(got, cfg.ExternalManifestURL) {
		t.Errorf("second fallback = %q, want the external URL", got)
	}

	// Off-site: the default server alone, no fallback to invent.
	u2 := newTestUpdater(t, cfg, dcNamed("DC-MI1"), ipsOf("10.0.0.9"))
	r2 := u2.newResolver(u2.beginCycle(context.Background(), true))
	if len(r2.Fallbacks) != 0 {
		t.Errorf("off-site fallbacks = %d, want none", len(r2.Fallbacks))
	}
}

// A laptop that changes subnet between cycles must follow it: the site is
// re-evaluated every cycle, not pinned at startup.
func TestSiteIsReevaluatedWhenTheAddressesChange(t *testing.T) {
	cfg := internalCfg(t, config.SourceInternal)
	addrs := []string{"172.16.96.50"}
	dcCalls := 0
	dc := func(string) (*machineinfo.DomainControllerInfo, error) {
		dcCalls++
		if addrs[0] == "172.16.96.50" {
			return &machineinfo.DomainControllerInfo{Name: "DC-RM2"}, nil
		}
		return nil, errors.New("no domain here")
	}
	u := newTestUpdater(t, cfg, dc, func() ([]string, error) { return addrs, nil })

	if cyc := u.beginCycle(context.Background(), true); cyc.site != "DC-RM2" {
		t.Fatalf("first cycle site = %q, want DC-RM2", cyc.site)
	}
	if dcCalls != 1 {
		t.Fatalf("dc lookups = %d, want 1", dcCalls)
	}

	// Same addresses: the cached DC answer is reused, no second lookup.
	if cyc := u.beginCycle(context.Background(), false); cyc.site != "DC-RM2" {
		t.Fatalf("second cycle site = %q, want DC-RM2", cyc.site)
	}
	if dcCalls != 1 {
		t.Errorf("dc lookups = %d, want the cached answer reused", dcCalls)
	}

	// The machine moves: the address change forces a fresh lookup and the
	// chain falls back to the default server.
	addrs = []string{"192.168.1.30"}
	cyc := u.beginCycle(context.Background(), false)
	if dcCalls != 2 {
		t.Errorf("dc lookups = %d, want a fresh one after the addresses changed", dcCalls)
	}
	if cyc.site != "" || strings.Join(cyc.chain, ",") != policy.LegacyExternal {
		t.Errorf("after moving: site %q chain %v, want the default server", cyc.site, cyc.chain)
	}
}

// The boot-time retry window applies to the first lookup only: a machine
// that moves later must not stall a whole cycle on a doomed retry.
func TestOnlyTheFirstCycleRetriesTheDCLookup(t *testing.T) {
	cfg := internalCfg(t, config.SourceInternal)
	// The document carries the delay in whole seconds, so one attempt with
	// the shortest legal delay is what a test can afford to wait for.
	cfg.DCLookupRetryAttempts = 1
	cfg.DCLookupRetryDelay = time.Second

	addrs := []string{"172.16.96.50"}
	calls := 0
	dc := func(string) (*machineinfo.DomainControllerInfo, error) {
		calls++
		return nil, bootRaceError
	}
	u := newTestUpdater(t, cfg, dc, func() ([]string, error) { return addrs, nil })

	u.beginCycle(context.Background(), true)
	if calls != 2 {
		t.Fatalf("first cycle lookups = %d, want 1 + 1 retry", calls)
	}

	calls = 0
	addrs = []string{"10.0.0.7"}
	u.beginCycle(context.Background(), false)
	if calls != 1 {
		t.Errorf("later cycle lookups = %d, want a single attempt", calls)
	}
}

func TestSameDCName(t *testing.T) {
	cases := []struct {
		resolved, want string
		match          bool
	}{
		{"DC-RM2.tregcc.local", "DC-RM2", true},
		{"DC-RM2", "DC-RM2", true},
		{"dc-rm2.tregcc.local", "DC-RM2", true},
		{"DC-RM2", "dc-rm2.tregcc.local", true},
		{"DC-RM20", "DC-RM2", false},
		{"DC-MI1.tregcc.local", "DC-RM2", false},
		{"", "DC-RM2", false},
	}
	for _, c := range cases {
		if got := policy.SameDCName(c.resolved, c.want); got != c.match {
			t.Errorf("SameDCName(%q, %q) = %v, want %v", c.resolved, c.want, got, c.match)
		}
	}
}

// testLogger writes to a throwaway directory and never attaches the Windows
// Event Log, so the *Event calls exercise their nil-event-source path.
//
// The log directory is not t.TempDir(): the rolling file sink keeps
// updater.log open for the life of the process, and Windows refuses to
// unlink an open file, so t.TempDir() cleanup would fail the test for a
// reason that has nothing to do with what it asserts.
func testLogger(t *testing.T) *logging.Logger {
	t.Helper()
	dir, err := os.MkdirTemp("", "emlyupdater-log-*")
	if err != nil {
		t.Fatalf("creating log dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return logging.New(dir, "", false)
}

// retry is the DC-retry policy wound down to something a test can wait for;
// the shipped default is 6 attempts 5 seconds apart.
func retry(attempts int) policy.DCLookupRetry {
	return policy.DCLookupRetry{Attempts: attempts, DelaySeconds: 0}
}

// retryFast keeps a non-zero delay (zero disables retrying entirely) short.
func retryFast(attempts int) policy.DCLookupRetry {
	r := retry(attempts)
	if attempts > 0 {
		r.DelaySeconds = 1
	}
	return r
}

// bootRaceError is what DsGetDcName reports when the service starts before the
// network, DNS and netlogon are ready - the same error a machine that is
// genuinely off the domain gets, which is why the retry is gated on
// domain membership rather than on the error.
var bootRaceError = errors.New("DsGetDcName failed: the specified domain either does not exist or could not be contacted")

// The failure this whole retry exists for: the first lookups fail at boot,
// then the domain answers and the machine is correctly seen as on-site.
func TestResolveDCRetriesUntilTheDomainAnswers(t *testing.T) {
	calls := 0
	lookup := func(string) (*machineinfo.DomainControllerInfo, error) {
		calls++
		if calls < 3 {
			return nil, bootRaceError
		}
		return &machineinfo.DomainControllerInfo{Name: "DC-RM2.tregcc.local"}, nil
	}

	dc, err := resolveDC(context.Background(), retryFast(6), testLogger(t), lookup, true)
	if err != nil {
		t.Fatalf("resolveDC after retrying: %v", err)
	}
	if calls != 3 {
		t.Errorf("lookup called %d times, want 3", calls)
	}
	if dc.Name != "DC-RM2.tregcc.local" {
		t.Errorf("dc.Name = %q, want DC-RM2.tregcc.local", dc.Name)
	}
}

// On a machine that is not domain-joined the error is the truth, so retrying
// only delays the first update cycle by the whole retry window.
func TestResolveDCDoesNotRetryOffDomain(t *testing.T) {
	calls := 0
	lookup := func(string) (*machineinfo.DomainControllerInfo, error) {
		calls++
		return nil, bootRaceError
	}

	if _, err := resolveDC(context.Background(), retryFast(6), testLogger(t), lookup, false); err == nil {
		t.Fatal("expected the lookup error to be returned")
	}
	if calls != 1 {
		t.Errorf("lookup called %d times, want 1 (no retry off the domain)", calls)
	}
}

// Attempts or delay at zero is the documented way to switch retrying off.
func TestResolveDCDoesNotRetryWhenDisabled(t *testing.T) {
	calls := 0
	lookup := func(string) (*machineinfo.DomainControllerInfo, error) {
		calls++
		return nil, bootRaceError
	}

	if _, err := resolveDC(context.Background(), retry(0), testLogger(t), lookup, true); err == nil {
		t.Fatal("expected the lookup error to be returned")
	}
	if calls != 1 {
		t.Errorf("lookup called %d times, want 1 (retry disabled)", calls)
	}

	calls = 0
	if _, err := resolveDC(context.Background(), policy.DCLookupRetry{Attempts: 6}, testLogger(t), lookup, true); err == nil {
		t.Fatal("expected the lookup error to be returned")
	}
	if calls != 1 {
		t.Errorf("lookup called %d times with a zero delay, want 1", calls)
	}
}

// A stop request arriving inside the retry window must not be sat out: the SCM
// gives the service 30 seconds to stop, less than a generously configured
// retry window.
func TestResolveDCStopsRetryingWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	lookup := func(string) (*machineinfo.DomainControllerInfo, error) {
		calls++
		return nil, bootRaceError
	}

	start := time.Now()
	if _, err := resolveDC(ctx, policy.DCLookupRetry{Attempts: 6, DelaySeconds: 3600}, testLogger(t), lookup, true); err == nil {
		t.Fatal("expected the lookup error to be returned")
	}
	if calls != 1 {
		t.Errorf("lookup called %d times, want 1 (cancelled before the second attempt)", calls)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("resolveDC took %v, want it to return as soon as the context was done", elapsed)
	}
}

// End to end: a machine whose first lookup fails at boot is put on its own
// site once the retry resolves the office DC.
func TestFirstCycleRecoversFromABootTimeLookupFailure(t *testing.T) {
	cfg := internalCfg(t, config.SourceExternal)
	cfg.DCLookupRetryAttempts = 6
	cfg.DCLookupRetryDelay = time.Second

	calls := 0
	dc := func(string) (*machineinfo.DomainControllerInfo, error) {
		calls++
		if calls == 1 {
			return nil, bootRaceError
		}
		return &machineinfo.DomainControllerInfo{Name: "DC-RM2.tregcc.local", Site: "RM2"}, nil
	}
	u := newTestUpdater(t, cfg, dc, ipsOf("172.16.96.50"))

	cyc := u.beginCycle(context.Background(), true)
	if cyc.site != "DC-RM2" {
		t.Errorf("site = %q, want DC-RM2 after the retry resolved the DC", cyc.site)
	}
	if got := strings.Join(cyc.chain, ","); got != "internal,external" {
		t.Errorf("chain = %q, want the site's own server first", got)
	}
}
