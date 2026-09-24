package service

// Fix-round-1 coverage: applySelfUpdate must emit update.failed (not just
// log) when SelfDownloads.Ensure fails and when selfupdate.Launch fails
// after update.started has already gone out - otherwise a client watching
// the presence channel sees a started event with no matching outcome until
// the next cycle's reconcileSelfUpdate eventually notices (OutcomeMissed),
// several cooldown minutes later.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"emlyupdater/internal/download"
	"emlyupdater/internal/manifest"
	"emlyupdater/internal/state"
	"emlyupdater/internal/version"
	"emlyupdater/internal/wsclient"
)

// failingFetchSource satisfies source.Source but always fails FetchSetup, so
// download.Manager.Ensure fails without needing a real network.
type failingFetchSource struct{}

func (failingFetchSource) Name() string { return "failing-fetch-source" }
func (failingFetchSource) FetchManifest(context.Context) (*manifest.Manifest, error) {
	return nil, nil
}
func (failingFetchSource) ResolveTarget(*manifest.Manifest, string) (manifest.Target, error) {
	return manifest.Target{}, nil
}
func (failingFetchSource) FetchSetup(context.Context, manifest.Target, string) error {
	return errors.New("simulated network failure")
}

// TestApplySelfUpdateDownloadFailureEmitsUpdateFailed: SelfDownloads.Ensure
// failing (fetch or checksum, download.Manager gives no sentinel to tell
// them apart) must not change the existing "log and retry next cycle"
// control flow, but must also emit update.failed(code=download_failed,
// will_retry=true) so a connected client learns the attempt did not land
// without waiting on a later poll.
func TestApplySelfUpdateDownloadFailureEmitsUpdateFailed(t *testing.T) {
	u := newClientTestUpdater(t)
	u.SelfDownloads = &download.Manager{Dir: t.TempDir()}
	var got []updateEvent
	var names []string
	u.emitFn = func(name string, p any) {
		names = append(names, name)
		if ev, ok := p.(updateEvent); ok {
			got = append(got, ev)
		}
	}

	m := &manifest.UpdaterManifest{Version: "9.9.9", Download: "fake://irrelevant", SHA256: "deadbeef"}

	if launched := u.applySelfUpdate(context.Background(), failingFetchSource{}, m, 1); launched {
		t.Fatal("applySelfUpdate must report false when the download fails")
	}
	if u.installing.Load() != 0 {
		t.Fatalf("installing = %d, want 0 (never claimed on a download failure)", u.installing.Load())
	}
	if len(names) != 1 || names[0] != wsclient.EvtUpdateFailed {
		t.Fatalf("emitted events = %v, want exactly one %s", names, wsclient.EvtUpdateFailed)
	}
	ev := got[0]
	if ev.Target != "updater" || ev.ToVersion != "9.9.9" || !ev.WillRetry {
		t.Fatalf("update.failed = %+v", ev)
	}
	if ev.Error == nil || ev.Error.Code != "download_failed" {
		t.Fatalf("update.failed error = %+v, want code download_failed", ev.Error)
	}
}

// TestApplySelfUpdateLaunchFailureEmitsUpdateFailed: a failed
// selfupdate.Launch (via the launchFn seam) must still undo beginInstall and
// leave the existing control flow (best-effort cache restore, launched=
// false) unchanged, but must also emit update.failed(code=launch_failed,
// will_retry=true) - update.started was already sent for this attempt, so
// leaving it unanswered would strand the client's view of the attempt.
func TestApplySelfUpdateLaunchFailureEmitsUpdateFailed(t *testing.T) {
	u := newClientTestUpdater(t)
	u.SelfDownloads = &download.Manager{Dir: t.TempDir()}
	u.verifySelfSetupFn = func(string) error { return nil } // no real Authenticode-signed file in a test
	u.CachePath = filepath.Join(t.TempDir(), "remote-config.json")
	if err := os.WriteFile(u.CachePath, []byte(`{"revision":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	launchErr := errors.New("simulated launch failure")
	u.launchFn = func(string, string) error { return launchErr }

	var got []updateEvent
	var names []string
	u.emitFn = func(name string, p any) {
		names = append(names, name)
		if ev, ok := p.(updateEvent); ok {
			got = append(got, ev)
		}
	}

	sum := sha256.Sum256([]byte("fake-updater-setup-bytes"))
	m := &manifest.UpdaterManifest{Version: "9.9.9", Download: "fake://irrelevant", SHA256: hex.EncodeToString(sum[:])}

	if launched := u.applySelfUpdate(context.Background(), fakeSelfSource{}, m, 1); launched {
		t.Fatal("applySelfUpdate must report false when Launch fails")
	}
	if u.installing.Load() != 0 {
		t.Fatalf("installing = %d, want 0 (endInstall must run on a launch failure)", u.installing.Load())
	}
	if len(names) != 2 || names[0] != wsclient.EvtUpdateStarted || names[1] != wsclient.EvtUpdateFailed {
		t.Fatalf("emitted events = %v, want [%s %s]", names, wsclient.EvtUpdateStarted, wsclient.EvtUpdateFailed)
	}
	failed := got[1]
	if failed.Target != "updater" || failed.ToVersion != "9.9.9" || !failed.WillRetry {
		t.Fatalf("update.failed = %+v", failed)
	}
	if failed.Error == nil || failed.Error.Code != "launch_failed" || failed.Error.Message != launchErr.Error() {
		t.Fatalf("update.failed error = %+v", failed.Error)
	}
	// Cache retire/restore is best-effort and unchanged by this fix: the
	// retired file should have been moved aside then restored.
	if _, err := os.Stat(u.CachePath); err != nil {
		t.Fatalf("remote-config cache must be restored after the failed launch: %v", err)
	}
}

// TestApplySelfUpdateStartedUsesTheCycleTrigger: update.started's trigger
// must reflect what actually woke this cycle (e.g. "notify", when a
// release.published/config.published wake ran it early), not a hardcoded
// "cycle" - install() already gets this right for EMLy's own updates via
// u.cycleTrigger; applySelfUpdate must match.
func TestApplySelfUpdateStartedUsesTheCycleTrigger(t *testing.T) {
	u := newClientTestUpdater(t)
	u.SelfDownloads = &download.Manager{Dir: t.TempDir()}
	u.verifySelfSetupFn = func(string) error { return nil }
	u.CachePath = filepath.Join(t.TempDir(), "remote-config.json")
	u.launchFn = func(string, string) error { return nil }
	u.cycleTrigger = "notify"

	var got []updateEvent
	u.emitFn = func(name string, p any) {
		if ev, ok := p.(updateEvent); ok {
			got = append(got, ev)
		}
	}

	sum := sha256.Sum256([]byte("fake-updater-setup-bytes"))
	m := &manifest.UpdaterManifest{Version: "9.9.9", Download: "fake://irrelevant", SHA256: hex.EncodeToString(sum[:])}

	if launched := u.applySelfUpdate(context.Background(), fakeSelfSource{}, m, 1); !launched {
		t.Fatal("applySelfUpdate should have launched")
	}
	if len(got) != 1 || got[0].Trigger != "notify" {
		t.Fatalf("update.started events = %+v, want Trigger=notify", got)
	}
}

// TestApplySelfUpdateDownloadFailureRepeatsSuppressed: SetSelfUpdate is only
// reached after a successful download+verify, so a broken mirror never
// advances the attempt counter recorded in state.json - Decide hands back
// the same attempt number every cycle. Calling applySelfUpdate twice for
// that same (target, version, attempt, code) must only emit update.failed
// once: the second cycle is not new information.
func TestApplySelfUpdateDownloadFailureRepeatsSuppressed(t *testing.T) {
	u := newClientTestUpdater(t)
	u.SelfDownloads = &download.Manager{Dir: t.TempDir()}
	var names []string
	u.emitFn = func(name string, _ any) { names = append(names, name) }

	m := &manifest.UpdaterManifest{Version: "9.9.9", Download: "fake://irrelevant", SHA256: "deadbeef"}

	u.applySelfUpdate(context.Background(), failingFetchSource{}, m, 1)
	u.applySelfUpdate(context.Background(), failingFetchSource{}, m, 1) // a later cycle, same attempt

	if len(names) != 1 {
		t.Fatalf("emitted events across two cycles = %v, want exactly one update.failed", names)
	}
}

// TestApplySelfUpdateDownloadFailureNewAttemptEmitsAgain: once a new attempt
// number is tried (the cooldown elapsed, or Decide started a fresh count for
// a new target version), a failure at that attempt is new information and
// must be reported again.
func TestApplySelfUpdateDownloadFailureNewAttemptEmitsAgain(t *testing.T) {
	u := newClientTestUpdater(t)
	u.SelfDownloads = &download.Manager{Dir: t.TempDir()}
	var names []string
	u.emitFn = func(name string, _ any) { names = append(names, name) }

	m := &manifest.UpdaterManifest{Version: "9.9.9", Download: "fake://irrelevant", SHA256: "deadbeef"}

	u.applySelfUpdate(context.Background(), failingFetchSource{}, m, 1)
	u.applySelfUpdate(context.Background(), failingFetchSource{}, m, 2)

	if len(names) != 2 {
		t.Fatalf("emitted events across two attempts = %v, want two update.failed", names)
	}
}

// TestReconcileSelfUpdateMissedSuppressedAcrossCycles: reconcileSelfUpdate
// runs at the top of every cycle. While a launch stays pending - the
// cooldown, every retry, or forever once the target is abandoned (GaveUp,
// which Reconcile does not special-case: running stays behind rec.Version
// forever) - OutcomeMissed must only be reported to the client channel once
// per attempt, not on every cycle that re-evaluates the same record.
func TestReconcileSelfUpdateMissedSuppressedAcrossCycles(t *testing.T) {
	u := newClientTestUpdater(t)
	var names []string
	u.emitFn = func(name string, _ any) { names = append(names, name) }

	rec := &state.SelfUpdate{Version: "9.9.9", FromVersion: version.Version, Attempts: 1, LaunchedAt: time.Now()}
	if err := u.Store.SetSelfUpdate(rec); err != nil {
		t.Fatal(err)
	}

	u.reconcileSelfUpdate()
	u.reconcileSelfUpdate() // a later cycle, same unresolved record

	if len(names) != 1 || names[0] != wsclient.EvtUpdateFailed {
		t.Fatalf("emitted events across two reconciles = %v, want exactly one %s", names, wsclient.EvtUpdateFailed)
	}
}

// TestReconcileSelfUpdateMissedNewAttemptEmitsAgain: a new attempt number
// recorded against the same target (a retry after the cooldown) is new
// information even though the target version has not changed.
func TestReconcileSelfUpdateMissedNewAttemptEmitsAgain(t *testing.T) {
	u := newClientTestUpdater(t)
	var names []string
	u.emitFn = func(name string, _ any) { names = append(names, name) }

	rec := &state.SelfUpdate{Version: "9.9.9", FromVersion: version.Version, Attempts: 1, LaunchedAt: time.Now()}
	if err := u.Store.SetSelfUpdate(rec); err != nil {
		t.Fatal(err)
	}
	u.reconcileSelfUpdate()

	rec.Attempts = 2
	if err := u.Store.SetSelfUpdate(rec); err != nil {
		t.Fatal(err)
	}
	u.reconcileSelfUpdate()

	if len(names) != 2 {
		t.Fatalf("emitted events across two attempts = %v, want two %s", names, wsclient.EvtUpdateFailed)
	}
}

// TestLaunchFailureThenReconcileMissedSuppressed: a failed launch already
// reports the attempt as launch_failed. The record persisted before the
// launch (SetSelfUpdate) is not undone on failure, so the next cycle's
// reconcileSelfUpdate sees the same attempt and would otherwise restate it
// as version_mismatch - the same outcome, under a different code, that the
// client channel was already told about one message ago.
func TestLaunchFailureThenReconcileMissedSuppressed(t *testing.T) {
	u := newClientTestUpdater(t)
	u.SelfDownloads = &download.Manager{Dir: t.TempDir()}
	u.verifySelfSetupFn = func(string) error { return nil } // no real Authenticode-signed file in a test
	u.CachePath = filepath.Join(t.TempDir(), "remote-config.json")
	if err := os.WriteFile(u.CachePath, []byte(`{"revision":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	u.launchFn = func(string, string) error { return errors.New("simulated launch failure") }

	var names []string
	u.emitFn = func(name string, _ any) { names = append(names, name) }

	sum := sha256.Sum256([]byte("fake-updater-setup-bytes"))
	m := &manifest.UpdaterManifest{Version: "9.9.9", Download: "fake://irrelevant", SHA256: hex.EncodeToString(sum[:])}

	if launched := u.applySelfUpdate(context.Background(), fakeSelfSource{}, m, 1); launched {
		t.Fatal("applySelfUpdate must report false when Launch fails")
	}
	if len(names) != 2 || names[1] != wsclient.EvtUpdateFailed {
		t.Fatalf("emitted events after the failed launch = %v", names)
	}

	// A later cycle's reconcileSelfUpdate, against the same still-persisted
	// record, must find nothing new to report for this attempt.
	names = nil
	u.reconcileSelfUpdate()
	if len(names) != 0 {
		t.Fatalf("reconcileSelfUpdate after a launch_failed re-emitted = %v, want none", names)
	}
}
