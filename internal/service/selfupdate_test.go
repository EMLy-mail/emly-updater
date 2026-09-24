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

	"emlyupdater/internal/download"
	"emlyupdater/internal/manifest"
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
