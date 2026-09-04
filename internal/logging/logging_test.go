package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// logDir is not t.TempDir(): the rolling sink keeps updater.log open and
// Windows refuses to unlink an open file, so the automatic cleanup would
// fail the test for a reason unrelated to what it asserts.
func logDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "emlyupdater-logging-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func readLog(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "updater.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(raw)
}

// The remote configuration can raise or lower the level while the service
// runs; nothing below the current level may reach the file.
func TestSetLevel(t *testing.T) {
	dir := logDir(t)
	l := New(dir, "", false)

	l.Debug("hidden-at-info")
	l.Info("visible-at-info")
	if got := readLog(t, dir); strings.Contains(got, "hidden-at-info") || !strings.Contains(got, "visible-at-info") {
		t.Fatalf("at info level the log holds: %q", got)
	}

	if !l.SetLevel("debug") {
		t.Fatal("SetLevel(debug) refused")
	}
	if l.Level() != "debug" {
		t.Errorf("Level() = %q", l.Level())
	}
	l.Debug("visible-at-debug")
	if !strings.Contains(readLog(t, dir), "visible-at-debug") {
		t.Error("a debug line did not reach the log after raising the level")
	}

	if !l.SetLevel("error") {
		t.Fatal("SetLevel(error) refused")
	}
	l.Warn("hidden-at-error")
	if strings.Contains(readLog(t, dir), "hidden-at-error") {
		t.Error("a warning reached the log at error level")
	}

	// An unknown name must not silence the log: the level stays put.
	if l.SetLevel("trace") {
		t.Error("SetLevel accepted an unknown level")
	}
	if l.Level() != "error" {
		t.Errorf("Level() = %q after a refused change, want error", l.Level())
	}
}

// Reconfigure swaps the rolling sinks in place; lines written across the
// swap must all land in the file.
func TestReconfigureKeepsWriting(t *testing.T) {
	dir := logDir(t)
	l := New(dir, "", false)

	l.Info("before-reconfigure")
	l.Reconfigure(FileSettings{MaxSizeMB: 10, MaxBackups: 2, Compress: false})
	l.Info("after-reconfigure")

	got := readLog(t, dir)
	if !strings.Contains(got, "before-reconfigure") || !strings.Contains(got, "after-reconfigure") {
		t.Errorf("log lost a line across the reconfigure: %q", got)
	}

	// Idempotent: the same settings twice must not churn the sinks.
	l.Reconfigure(FileSettings{MaxSizeMB: 10, MaxBackups: 2, Compress: false})
	l.Info("after-noop-reconfigure")
	if !strings.Contains(readLog(t, dir), "after-noop-reconfigure") {
		t.Error("log stopped writing after a no-op reconfigure")
	}
}

func TestParseLevel(t *testing.T) {
	for _, name := range []string{"debug", "INFO", " warn ", "warning", "Error"} {
		if _, ok := ParseLevel(name); !ok {
			t.Errorf("ParseLevel(%q) refused a valid level", name)
		}
	}
	for _, name := range []string{"", "trace", "verbose", "off"} {
		if _, ok := ParseLevel(name); ok {
			t.Errorf("ParseLevel(%q) accepted an unknown level", name)
		}
	}
}

// The remote-configuration events must reach Event Viewer even when the
// document turns the mirror off - that is what makes a bad push diagnosable.
func TestRemoteConfigEventsBypassTheEventLogToggle(t *testing.T) {
	for _, id := range []uint32{EventRemoteConfigApplied, EventRemoteConfigUnreachable,
		EventRemoteConfigRejected, EventRemoteConfigStale, EventControlGate} {
		if !alwaysMirrored(id) {
			t.Errorf("event %d must be mirrored regardless of the toggle", id)
		}
	}
	for _, id := range []uint32{EventGeneric, EventUpdateFound, EventInstallOK, EventSelfUpdateFound} {
		if alwaysMirrored(id) {
			t.Errorf("event %d must honour the toggle", id)
		}
	}
}
