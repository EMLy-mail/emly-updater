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
}

func newProductServer(t *testing.T) *productServer {
	ps := &productServer{emlyVersion: "2.0.0"}
	sum := sha256.Sum256(rcSetup)
	ps.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ps.mu.Lock()
		defer ps.mu.Unlock()
		switch r.URL.Path {
		case "/v2/updates/manifest":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"stableVersion":"` + ps.emlyVersion + `","stableDownload":"` + ps.URL +
				`/v2/updates/releases/x/download","sha256Checksums":{"` + ps.emlyVersion + `":"00"}}`))
		case "/v2/updates/" + rcSlug + "/manifest":
			ps.rcManifestCalls.Add(1)
			if ps.rcManifest == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(ps.rcManifest))
		case "/v2/updates/" + rcSlug + "/releases/1.1.0/download":
			_, _ = w.Write(rcSetup)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ps.Close)
	ps.rcManifest = `{"stableVersion":"1.1.0","stableDownload":"` + ps.URL + `/v2/updates/` + rcSlug +
		`/releases/1.1.0/download","sha256Checksums":{"1.1.0":"` + hex.EncodeToString(sum[:]) + `"}}`
	return ps
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

// A destructive command committed mid-round stops the products after it.
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
