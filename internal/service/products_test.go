package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"emlyupdater/internal/config"
	"emlyupdater/internal/download"
	"emlyupdater/internal/installer"
	"emlyupdater/internal/notify"
	"emlyupdater/internal/policy"
	"emlyupdater/internal/product"
	"emlyupdater/internal/state"
	"emlyupdater/internal/wsclient"
)

const rcSlug = "3g-rocketchat"

var rcSetup = []byte("rocketchat-setup-bytes")

// productServer serves EMLy's manifest at the installed version (EMLy is a
// no-op) and RocketChat's manifest and setup. rcManifest "" answers 404.
type productServer struct {
	*httptest.Server
	mu              sync.Mutex
	rcManifest      string
	rcManifestCalls atomic.Int32
	emlyVersion     string
	// emlySetup, when set, makes EMLy's manifest point at a real setup with
	// its true checksum (an EMLy update that can actually be downloaded).
	emlySetup []byte
	// onRequest, when set, sees every request path before it is served.
	onRequest func(path string)
	// rcDownloads counts requests for any RocketChat setup; with
	// rcRefuseAfterFirst every request after the first answers 429 with the
	// API's queue-full body.
	rcDownloads        atomic.Int32
	rcRefuseAfterFirst bool
}

func newProductServer(t *testing.T) *productServer {
	ps := &productServer{emlyVersion: "2.0.0"}
	sum := sha256.Sum256(rcSetup)
	ps.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ps.onRequest != nil {
			ps.onRequest(r.URL.Path)
		}
		ps.mu.Lock()
		defer ps.mu.Unlock()
		switch r.URL.Path {
		case "/v2/updates/manifest":
			emlySum := "00"
			if ps.emlySetup != nil {
				s := sha256.Sum256(ps.emlySetup)
				emlySum = hex.EncodeToString(s[:])
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"stableVersion":"` + ps.emlyVersion + `","stableDownload":"` + ps.URL +
				`/v2/updates/releases/x/download","sha256Checksums":{"` + ps.emlyVersion + `":"` + emlySum + `"}}`))
		case "/v2/updates/releases/x/download":
			if ps.emlySetup == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(ps.emlySetup)
		case "/v2/updates/zzz/manifest":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(ps.rcManifest))
		case "/v2/updates/" + rcSlug + "/manifest":
			ps.rcManifestCalls.Add(1)
			if ps.rcManifest == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(ps.rcManifest))
		default:
			// Any RocketChat release serves rcSetup (see rcManifestFor).
			if strings.HasPrefix(r.URL.Path, "/v2/updates/"+rcSlug+"/releases/") && strings.HasSuffix(r.URL.Path, "/download") {
				if n := ps.rcDownloads.Add(1); n > 1 && ps.rcRefuseAfterFirst {
					w.Header().Set("Retry-After", "3600")
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = w.Write([]byte(`{"error":"download queue full","capacity":4,"active":4}`))
					return
				}
				_, _ = w.Write(rcSetup)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ps.Close)
	ps.rcManifest = `{"stableVersion":"1.1.0","stableDownload":"` + ps.URL + `/v2/updates/` + rcSlug +
		`/releases/1.1.0/download","sha256Checksums":{"1.1.0":"` + hex.EncodeToString(sum[:]) + `"}}`
	return ps
}

// rcManifestFor is RocketChat's manifest offering version (served rcSetup).
func (ps *productServer) rcManifestFor(version string) string {
	sum := sha256.Sum256(rcSetup)
	return `{"stableVersion":"` + version + `","stableDownload":"` + ps.URL + `/v2/updates/` + rcSlug +
		`/releases/` + version + `/download","sha256Checksums":{"` + version + `":"` + hex.EncodeToString(sum[:]) + `"}}`
}

func (ps *productServer) setRC(body string) { ps.mu.Lock(); ps.rcManifest = body; ps.mu.Unlock() }

type productHarness struct {
	u       *Updater
	rcDir   string
	driver  *fakeDriver
	running map[string]bool
	boxes   []notify.Message
}

// newProductHarness: EMLy installed at the manifest's version, RocketChat
// installed at 1.0.0 (version.txt) and enabled in the document.
func newProductHarness(t *testing.T, srv *productServer) *productHarness {
	t.Helper()
	cfg := internalCfg(t, config.SourceInternal)
	cfg.InternalManifestURL = srv.URL + "/v2/updates/manifest"
	cfg.ExternalManifestURL = srv.URL + "/v2/updates/manifest"
	emlyDir := t.TempDir()
	cfg.EMLyConfigFile = filepath.Join(emlyDir, "config.ini")
	_ = os.WriteFile(cfg.EMLyConfigFile, []byte("[EMLy]\nGUI_SEMVER="+srv.emlyVersion+"\n"), 0o644)

	h := &productHarness{rcDir: t.TempDir(), running: map[string]bool{}}
	_ = os.WriteFile(filepath.Join(h.rcDir, "version.txt"), []byte("1.0.0\r\n"), 0o644)
	h.driver = &fakeDriver{dir: h.rcDir}

	u := newTestUpdater(t, cfg, dcNamed("DC-RM2"), ipsOf("172.16.96.50"))
	u.Store = &state.Store{Path: filepath.Join(t.TempDir(), "state.json")}
	u.Downloads = &download.Manager{Dir: t.TempDir()}
	u.runningFn = func(exe string) bool { return h.running[exe] }
	u.driverFn = func(p *product.Product) installer.Driver {
		if p.Legacy {
			t.Fatalf("EMLy setup must not run in these tests")
		}
		return h.driver
	}
	u.notifyBoxFn = func(m notify.Message, _ int) bool { h.boxes = append(h.boxes, m); return true }
	u.toastFn = func(string, string, string) bool { return true }

	snap := u.Policy.Current()
	snap.Parsed.Global.Updater.Resolver = policy.ResolverSettings{Attempts: 1}
	snap.Parsed.Global.Updater.SelfUpdate.Enabled = false
	snap.Parsed.Global.Products = map[string]policy.ProductSettings{rcSlug: {
		Enabled: true, Name: "3g-RocketChat", InstallDir: h.rcDir, ExeName: "3g-RocketChat.exe",
		Detect:    []policy.ProductDetect{{Type: "file", Path: "version.txt"}},
		Installer: policy.ProductInstaller{Type: "nsis"},
	}}
	h.u = u
	return h
}

func (h *productHarness) cycle(t *testing.T) error {
	t.Helper()
	cyc := h.u.beginCycle(context.Background(), true)
	return h.u.Cycle(context.Background(), cyc)
}

func TestCycleUpdatesAProduct(t *testing.T) {
	h := newProductHarness(t, newProductServer(t))
	if err := h.cycle(t); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	if len(h.driver.installs) != 1 || h.driver.installs[0] != "1.1.0" {
		t.Fatalf("installs = %v, want [1.1.0]", h.driver.installs)
	}
	if p, _ := h.u.Store.PendingFor(rcSlug); p != nil {
		t.Errorf("pending not cleared: %+v", p)
	}
}

// A product kept open never blocks the cycle, and the user is told once per
// version, not once per cycle.
func TestRunningProductDefersWithOneNotification(t *testing.T) {
	h := newProductHarness(t, newProductServer(t))
	h.running["3g-RocketChat.exe"] = true
	for i := 0; i < 3; i++ {
		if err := h.cycle(t); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}
	if len(h.driver.installs) != 0 {
		t.Fatalf("installed while running: %v", h.driver.installs)
	}
	if len(h.boxes) != 1 {
		t.Fatalf("notifications = %d, want 1", len(h.boxes))
	}
	if p, _ := h.u.Store.PendingFor(rcSlug); p == nil || p.Version != "1.1.0" {
		t.Fatalf("pending = %+v, want 1.1.0 kept", p)
	}

	h.running["3g-RocketChat.exe"] = false
	if err := h.cycle(t); err != nil {
		t.Fatal(err)
	}
	if len(h.driver.installs) != 1 {
		t.Fatalf("installs after close = %v", h.driver.installs)
	}
}

// EMLy is last in the round: a product before it never waits on EMLy.
func TestCycleProductsOrderPutsEMLyLast(t *testing.T) {
	h := newProductHarness(t, newProductServer(t))
	snap := h.u.Policy.Current()
	snap.Parsed.Global.Products["aaa"] = snap.Parsed.Global.Products[rcSlug]
	list := h.u.cycleProducts(h.u.beginCycle(context.Background(), true))
	var slugs []string
	for _, p := range list {
		slugs = append(slugs, p.Slug)
	}
	if got := strings.Join(slugs, ","); got != "3g-rocketchat,aaa,emly" {
		t.Fatalf("order = %s", got)
	}
}

func TestDisabledProductIsNotPolled(t *testing.T) {
	srv := newProductServer(t)
	h := newProductHarness(t, srv)
	s := h.u.Policy.Current().Parsed.Global.Products[rcSlug]
	s.Enabled = false
	h.u.Policy.Current().Parsed.Global.Products[rcSlug] = s
	_ = h.cycle(t)
	if srv.rcManifestCalls.Load() != 0 {
		t.Fatalf("disabled product polled %d times", srv.rcManifestCalls.Load())
	}
}

// Absent and installWhenAbsent false: not polled at all.
func TestAbsentProductIsLeftAlone(t *testing.T) {
	srv := newProductServer(t)
	h := newProductHarness(t, srv)
	_ = os.Remove(filepath.Join(h.rcDir, "version.txt"))
	_ = h.cycle(t)
	if srv.rcManifestCalls.Load() != 0 || len(h.driver.installs) != 0 {
		t.Fatalf("absent product touched: polls=%d installs=%v", srv.rcManifestCalls.Load(), h.driver.installs)
	}
}

func TestNewerInstalledVersionIsNotDowngraded(t *testing.T) {
	h := newProductHarness(t, newProductServer(t))
	_ = os.WriteFile(filepath.Join(h.rcDir, "version.txt"), []byte("2.0.0"), 0o644)
	if err := h.cycle(t); err != nil {
		t.Fatal(err)
	}
	if len(h.driver.installs) != 0 {
		t.Fatalf("downgraded: %v", h.driver.installs)
	}
}

// 404 and an empty release both mean "no update"; the product is not asked
// again for productUnavailableFor.
func TestUnavailableProductIsBackedOff(t *testing.T) {
	for name, body := range map[string]string{
		"404":           "",
		"empty release": `{"stableVersion":"","stableDownload":"","sha256Checksums":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := newProductServer(t)
			srv.setRC(body)
			h := newProductHarness(t, srv)
			now := time.Now()
			h.u.nowFn = func() time.Time { return now }
			if err := h.cycle(t); err != nil {
				t.Fatalf("an unavailable product failed the cycle: %v", err)
			}
			calls := srv.rcManifestCalls.Load()
			_ = h.cycle(t)
			if srv.rcManifestCalls.Load() != calls {
				t.Fatalf("asked again within the back-off window")
			}
			now = now.Add(productUnavailableFor + time.Minute)
			_ = h.cycle(t)
			if srv.rcManifestCalls.Load() == calls {
				t.Fatalf("not asked again after the back-off window")
			}
		})
	}
}

// A failing setup is tried maxProductAttempts times for the same version,
// then left alone until the manifest offers another one. No uninstall runs:
// cleanReinstall is off by default.
func TestFailingProductGivesUpAfterThreeAttempts(t *testing.T) {
	srv := newProductServer(t)
	h := newProductHarness(t, srv)
	h.driver.failInstall = true
	for i := 0; i < 5; i++ {
		_ = h.cycle(t)
	}
	// Two setup runs per attempt (first + retry), three attempts.
	if len(h.driver.installs) != 2*maxProductAttempts {
		t.Fatalf("setup runs = %d, want %d", len(h.driver.installs), 2*maxProductAttempts)
	}
	if h.driver.uninstalls != 0 {
		t.Fatalf("uninstall ran %d times with cleanReinstall off", h.driver.uninstalls)
	}
	p, _ := h.u.Store.PendingFor(rcSlug)
	if p == nil || !p.GaveUp || p.Attempts != maxProductAttempts {
		t.Fatalf("pending = %+v, want gaveUp after %d attempts", p, maxProductAttempts)
	}
}

// A destructive command already committed when the cycle starts stops it
// before the round (Cycle's pre-round gate).
func TestDestructivePendingStopsTheRound(t *testing.T) {
	srv := newProductServer(t)
	h := newProductHarness(t, srv)
	h.u.destructivePending = true
	h.u.destructiveDeadline = time.Now().Add(time.Hour)
	_ = h.cycle(t)
	if srv.rcManifestCalls.Load() != 0 {
		t.Fatalf("product polled while a destructive command is pending")
	}
}

// A site mirror older than multi-product answers 404 to slug routes: the
// document's defaultServer is asked before the product is deemed unavailable.
func TestProduct404OnSiteMirrorFallsBackToDefaultServer(t *testing.T) {
	mirror := newProductServer(t)
	mirror.setRC("")
	central := newProductServer(t)
	h := newProductHarness(t, mirror)
	g := h.u.Policy.Current().Parsed.Global
	g.Servers = map[string]string{"srv-site": mirror.URL, "srv-central": central.URL}
	g.DefaultServer = "srv-central"
	g.DCLookupMap = map[string]policy.Site{"DC-RM2": {InternalSubnets: []string{"172.16.96.0/24"}, BaseServer: "srv-site"}}

	if err := h.cycle(t); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	if central.rcManifestCalls.Load() == 0 || len(h.driver.installs) != 1 {
		t.Fatalf("central asked %d times, installs %v", central.rcManifestCalls.Load(), h.driver.installs)
	}
}

// The "close the app" box is recorded as shown only when a user session
// actually saw it: a cycle with nobody at the machine does not use up the
// one notification of that version.
func TestNotifyWaitingOnlyCountsAShownBox(t *testing.T) {
	h := newProductHarness(t, newProductServer(t))
	h.running["3g-RocketChat.exe"] = true
	var attempts, shown int
	h.u.notifyBoxFn = func(notify.Message, int) bool {
		attempts++
		if attempts == 1 {
			return false // no user session on the first cycle
		}
		shown++
		return true
	}
	for i := 0; i < 4; i++ {
		if err := h.cycle(t); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}
	if attempts != 2 || shown != 1 {
		t.Fatalf("attempts = %d, shown = %d; want 2 attempts (first unseen) and 1 shown", attempts, shown)
	}
}

// A destructive command committed while one product is being handled stops
// the round before the next product: rcSlug sorts before "zzz", and
// rcSlug's manifest request is where the command lands.
func TestDestructiveCommittedMidRoundStopsTheNextProduct(t *testing.T) {
	srv := newProductServer(t)
	h := newProductHarness(t, srv)
	snap := h.u.Policy.Current()
	snap.Parsed.Global.Products["zzz"] = snap.Parsed.Global.Products[rcSlug]
	var zzzAsked atomic.Int32
	srv.onRequest = func(path string) {
		switch path {
		case "/v2/updates/zzz/manifest":
			zzzAsked.Add(1)
		case "/v2/updates/" + rcSlug + "/manifest":
			h.u.destructiveMu.Lock()
			h.u.destructivePending = true
			h.u.destructiveDeadline = time.Now().Add(time.Hour)
			h.u.destructiveMu.Unlock()
		}
	}
	if err := h.cycle(t); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	if srv.rcManifestCalls.Load() == 0 {
		t.Fatalf("the first product of the round was never polled")
	}
	if n := zzzAsked.Load(); n != 0 {
		t.Fatalf("product after the destructive command polled %d times", n)
	}
	if len(h.driver.installs) != 0 {
		t.Fatalf("setup ran after the destructive command: %v", h.driver.installs)
	}
}

// emlyConfigDriver fakes EMLy's setup: it writes config.ini's GUI_SEMVER.
type emlyConfigDriver struct{ configFile string }

func (d emlyConfigDriver) Install(_ string, version string) error {
	return os.WriteFile(d.configFile, []byte("[EMLy]\nGUI_SEMVER="+version+"\n"), 0o644)
}
func (emlyConfigDriver) Uninstall() error { return nil }

// A product resuming its pending entry earlier in the round must not hand
// its "resume" trigger to EMLy's update.started in the same cycle.
func TestProductResumeDoesNotLeakTriggerIntoEMLy(t *testing.T) {
	srv := newProductServer(t)
	srv.emlySetup = []byte("emly-setup-bytes")
	h := newProductHarness(t, srv)
	h.u.Cfg.EMLyInstallDir = t.TempDir() // no EMLy.exe: association repair stays off the real registry
	h.u.driverFn = func(p *product.Product) installer.Driver {
		if p.Legacy {
			return emlyConfigDriver{configFile: h.u.Cfg.EMLyConfigFile}
		}
		return h.driver
	}
	h.running["3g-RocketChat.exe"] = true

	// Cycle 1: EMLy is current; RocketChat downloads and is deferred, which
	// leaves a pending entry to resume.
	if err := h.cycle(t); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if p, _ := h.u.Store.PendingFor(rcSlug); p == nil {
		t.Fatalf("no RocketChat pending entry to resume")
	}

	// Cycle 2: RocketChat resumes (and is deferred again), then EMLy updates.
	_ = os.WriteFile(h.u.Cfg.EMLyConfigFile, []byte("[EMLy]\nGUI_SEMVER=1.0.0\n"), 0o644)
	var started []updateEvent
	h.u.emitFn = func(name string, p any) {
		if ev, ok := p.(updateEvent); ok && name == wsclient.EvtUpdateStarted {
			started = append(started, ev)
		}
	}
	if err := h.cycle(t); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if len(started) != 1 || started[0].Target != "emly" {
		t.Fatalf("update.started events = %+v, want one for emly", started)
	}
	if started[0].Trigger != "cycle" {
		t.Fatalf("EMLy update.started trigger = %q, want %q", started[0].Trigger, "cycle")
	}
	if len(h.driver.installs) != 0 {
		t.Fatalf("RocketChat installed while running: %v", h.driver.installs)
	}
}

// A re-download refused by a busy server (429, queue full) must not reset the
// attempt counter: the cached setup and the pending entry stay, the retry
// runs the cached copy, and the cap still gives the version up. Without that,
// every cycle would wipe the cache, fail to fetch, and start from zero.
func TestProductRedownloadRefusedKeepsTheAttemptCap(t *testing.T) {
	srv := newProductServer(t)
	srv.rcRefuseAfterFirst = true
	h := newProductHarness(t, srv)
	h.driver.failInstall = true
	for i := 0; i < 6; i++ {
		_ = h.cycle(t)
	}
	p, _ := h.u.Store.PendingFor(rcSlug)
	if p == nil || !p.GaveUp || p.Attempts != maxProductAttempts {
		t.Fatalf("pending = %+v, want gaveUp after %d attempts", p, maxProductAttempts)
	}
	if len(h.driver.installs) != 2*maxProductAttempts {
		t.Fatalf("setup runs = %d, want %d", len(h.driver.installs), 2*maxProductAttempts)
	}
	// One real download, then one refused re-download per attempt; nothing
	// once the version is given up.
	if n := srv.rcDownloads.Load(); n != 1+maxProductAttempts {
		t.Fatalf("setup downloads = %d, want %d", n, 1+maxProductAttempts)
	}
	if err := download.VerifyFile(p.SetupPath, p.SHA256); err != nil {
		t.Fatalf("cached setup lost: %v", err)
	}
}

// A version given up is retried as soon as the manifest offers another one.
func TestGivenUpVersionIsResetByADifferentRelease(t *testing.T) {
	srv := newProductServer(t)
	h := newProductHarness(t, srv)
	h.driver.failInstall = true
	for i := 0; i < maxProductAttempts+1; i++ {
		_ = h.cycle(t)
	}
	if p, _ := h.u.Store.PendingFor(rcSlug); p == nil || !p.GaveUp || p.Version != "1.1.0" {
		t.Fatalf("pending = %+v, want 1.1.0 given up", p)
	}

	h.driver.failInstall = false
	srv.setRC(srv.rcManifestFor("1.2.0"))
	if err := h.cycle(t); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	if last := h.driver.installs[len(h.driver.installs)-1]; last != "1.2.0" {
		t.Fatalf("last setup run = %s, want 1.2.0", last)
	}
	if p, _ := h.u.Store.PendingFor(rcSlug); p != nil {
		t.Fatalf("pending not cleared after installing 1.2.0: %+v", p)
	}
}

// Slugs may contain '-': product "a" being up to date clears its own cache
// only, never product "a-b"'s pending setup.
func TestProductCachesDoNotCollideOnSlugPrefixes(t *testing.T) {
	h := newProductHarness(t, newProductServer(t))
	a := &product.Product{Slug: "a"}
	ab := &product.Product{Slug: "a-b"}
	abSetup := h.u.downloadsFor(ab).SetupPath("1.0.0")
	if err := os.MkdirAll(filepath.Dir(abSetup), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abSetup, rcSetup, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.u.downloadsFor(a).CleanupExcept(""); err != nil {
		t.Fatalf("CleanupExcept: %v", err)
	}
	if _, err := os.Stat(abSetup); err != nil {
		t.Fatalf("product a's cleanup removed a-b's setup: %v", err)
	}
	if err := h.u.Downloads.CleanupExcept(""); err != nil {
		t.Fatalf("EMLy CleanupExcept: %v", err)
	}
	if _, err := os.Stat(abSetup); err != nil {
		t.Fatalf("EMLy's cleanup removed a-b's setup: %v", err)
	}
}
