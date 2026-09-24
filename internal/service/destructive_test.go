package service

// Fix-round-2 coverage for the destructive-command gate: beginInstall's two
// checkpoints (install() and applySelfUpdate), commitDestructive refusing a
// late-arriving install, and destructivePending's auto-expiry (item 1 of
// the second review round).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"emlyupdater/internal/config"
	"emlyupdater/internal/download"
	"emlyupdater/internal/manifest"
	"emlyupdater/internal/policy"
	"emlyupdater/internal/state"
	"emlyupdater/internal/wsclient"
)

// install() must refuse - before touching the setup file at all - while a
// destructive command is committed, and must not have claimed installing.
func TestInstallRefusedWhileDestructivePending(t *testing.T) {
	u := newClientTestUpdater(t)
	u.destructivePending = true

	err := u.install(context.Background(), nil, &state.Pending{Version: "9.9.9"}, config.EMLyInfo{})
	if err == nil {
		t.Fatal("install must refuse while a destructive command is pending")
	}
	if u.installing.Load() != 0 {
		t.Fatalf("installing = %d, want 0 (must not be claimed on refusal)", u.installing.Load())
	}
}

// fakeSelfSource satisfies source.Source with just enough behaviour for
// download.Manager.Ensure to succeed: FetchSetup writes a fixed payload,
// whose SHA256 the test computes and puts in the manifest Target.
type fakeSelfSource struct{}

func (fakeSelfSource) Name() string { return "fake-self-source" }
func (fakeSelfSource) FetchManifest(context.Context) (*manifest.Manifest, error) {
	return nil, nil
}
func (fakeSelfSource) ResolveTarget(*manifest.Manifest, string) (manifest.Target, error) {
	return manifest.Target{}, nil
}
func (fakeSelfSource) FetchSetup(_ context.Context, _ manifest.Target, destPath string) error {
	return os.WriteFile(destPath, []byte("fake-updater-setup-bytes"), 0o644)
}

// applySelfUpdate must refuse - via beginInstall, checked before
// SetSelfUpdate and retireCache - while a destructive command is
// committed, and must leave both the self-update attempt record and the
// remote-config cache untouched: there is nothing to undo them with on a
// refusal, since nothing failed.
func TestApplySelfUpdateRefusedWhileDestructivePending(t *testing.T) {
	u := newClientTestUpdater(t)
	u.SelfDownloads = &download.Manager{Dir: t.TempDir()}
	u.verifySelfSetupFn = func(string) error { return nil } // no real Authenticode-signed file exists in a test
	u.CachePath = filepath.Join(t.TempDir(), "remote-config.json")
	if err := os.WriteFile(u.CachePath, []byte(`{"revision":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	u.destructivePending = true

	sum := sha256.Sum256([]byte("fake-updater-setup-bytes"))
	m := &manifest.UpdaterManifest{Version: "9.9.9", Download: "fake://irrelevant", SHA256: hex.EncodeToString(sum[:])}

	if launched := u.applySelfUpdate(context.Background(), fakeSelfSource{}, m, 1); launched {
		t.Fatal("applySelfUpdate must not launch while a destructive command is pending")
	}
	if u.installing.Load() != 0 {
		t.Fatalf("installing = %d, want 0 (must not be claimed on refusal)", u.installing.Load())
	}

	st, err := u.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.SelfUpdate != nil {
		t.Fatalf("self-update attempt record must be untouched on refusal, got %+v", st.SelfUpdate)
	}
	if _, err := os.Stat(u.CachePath); err != nil {
		t.Fatalf("remote-config cache must still be at its original path: %v", err)
	}
	if _, err := os.Stat(u.cachePrevPath()); !os.IsNotExist(err) {
		t.Fatalf("remote-config cache must not have been retired (moved aside) on refusal")
	}
}

// commitDestructive must refuse even after a destructive command has
// already been admitted, if an install has since started (installing > 0)
// - the narrow gap between admitDestructive's check and runDestructive
// actually committing.
func TestCommitDestructiveRefusedWhileInstalling(t *testing.T) {
	u := newClientTestUpdater(t)
	u.installing.Add(1)

	if u.commitDestructive(u.clock().Add(time.Minute)) {
		t.Fatal("commitDestructive must refuse while installing > 0")
	}
	if u.destructivePendingNow() {
		t.Fatal("destructivePending must not be set when commitDestructive refuses")
	}
}

// destructivePending must auto-expire once its deadline passes: Cycle stops
// skipping, and a new destructive command is admitted again. Without this,
// an aborted reboot or a restart-service child that never brings the
// service back leaves the host stuck for the rest of the process's life.
func TestDestructivePendingAutoExpires(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdMachineReboot)

	now := time.Now()
	u.nowFn = func() time.Time { return now }

	if !u.commitDestructive(now.Add(time.Minute)) {
		t.Fatal("commitDestructive should have succeeded")
	}
	if !u.destructivePendingNow() {
		t.Fatal("destructivePending should be set before the deadline")
	}

	// Advance past the deadline.
	now = now.Add(2 * time.Minute)

	if u.destructivePendingNow() {
		t.Fatal("destructivePending should auto-expire once its deadline has passed")
	}

	// A new destructive command must be admitted again.
	if e := u.admitDestructive(wsclient.Command{Name: wsclient.CmdMachineReboot}); e != nil {
		t.Fatalf("a new destructive command should be admitted after expiry, got %+v", e)
	}
}

// Cycle itself must resume normal operation once destructivePending has
// expired - proven here by observing that it actually attempts a manifest
// fetch (a real network call) rather than returning early at the gate.
func TestCycleResumesAfterDestructivePendingExpires(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	cfg := internalCfg(t, config.SourceInternal)
	cfg.InternalManifestURL = srv.URL + "/v2/updates/manifest"
	cfg.ExternalManifestURL = srv.URL + "/v2/updates/manifest"
	cfg.EMLyConfigFile = filepath.Join(t.TempDir(), "missing", "config.ini")
	u := newTestUpdater(t, cfg, dcNamed("DC-RM2"), ipsOf("172.16.96.50"))
	u.Store = &state.Store{Path: filepath.Join(t.TempDir(), "state.json")}

	snap := u.Policy.Current()
	snap.Parsed.Global.Updater.Resolver = policy.ResolverSettings{Attempts: 1, BaseBackoffSeconds: 0}
	snap.Parsed.Global.Updater.SelfUpdate.Enabled = false // keep the network call scoped to EMLy's own manifest

	now := time.Now()
	u.nowFn = func() time.Time { return now }
	u.destructivePending = true
	u.destructiveDeadline = now.Add(-time.Second) // already expired

	cyc := u.beginCycle(context.Background(), true)
	_ = u.Cycle(context.Background(), cyc) // error expected (404); only whether it tried matters here

	if atomic.LoadInt32(&requests) == 0 {
		t.Fatal("Cycle did not proceed past the destructive gate: no manifest request was made")
	}
	if u.destructivePendingNow() {
		t.Fatal("destructivePending should have auto-expired")
	}
}
