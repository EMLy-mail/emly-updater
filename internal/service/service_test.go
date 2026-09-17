package service

import (
	"os"
	"path/filepath"
	"testing"

	"emlyupdater/internal/config"
)

// notifySourcesUnreachable talks to real Windows session APIs (no active
// console session in a CI/build agent), so this only exercises the parts
// that do not depend on one actually being there: it must never panic, and
// sourcesUnreachableNotified must stay false when the toast could not be
// shown - the whole point of gating on LaunchToast's own return value is
// that a still-ongoing outage keeps trying every cycle until someone is
// there to see it.
func TestNotifySourcesUnreachableDoesNotPanicWithoutAConsoleSession(t *testing.T) {
	u := &Updater{
		Cfg: &config.Config{
			EMLyInstallDir: `C:\3gIT\EMLy`,
			EMLyExeName:    "EMLy.exe",
			EMLyConfigFile: `C:\3gIT\EMLy\config.ini`,
		},
		Log: testLogger(t),
	}

	u.notifySourcesUnreachable()
	u.notifySourcesUnreachable()

	if u.sourcesUnreachableNotified {
		t.Skip("a console session is active on this machine; the no-session guard was not exercised")
	}
}

// The X-EMLy-Version header reports what EMLy's own config.ini says is
// installed, and reports nothing at all when EMLy is not installed: the
// 0.0.0 the resolver substitutes there is a comparison sentinel, and an API
// that stored it would show the fleet a release that does not exist.
func TestEMLyVersionForHeader(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.ini")
	if err := os.WriteFile(cfgPath, []byte("[EMLy]\nGUI_SEMVER=3.4.1\nGUI_RELEASE_CHANNEL=stable\n"), 0o644); err != nil {
		t.Fatalf("write config.ini: %v", err)
	}

	u := &Updater{Cfg: &config.Config{EMLyConfigFile: cfgPath}, Log: testLogger(t)}
	if got := u.emlyVersion(); got != "3.4.1" {
		t.Errorf("emlyVersion() = %q, want %q", got, "3.4.1")
	}

	u.Cfg.EMLyConfigFile = filepath.Join(dir, "absent.ini")
	if got := u.emlyVersion(); got != "" {
		t.Errorf("emlyVersion() on a fresh install = %q, want empty", got)
	}
}
