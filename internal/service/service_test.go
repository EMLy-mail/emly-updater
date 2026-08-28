package service

import (
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
