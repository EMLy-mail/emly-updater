package service

import (
	"errors"
	"testing"

	"emlyupdater/internal/wsclient"
)

func TestAnnounceUpdateOncePerVersion(t *testing.T) {
	u := newClientTestUpdater(t)
	var got []string
	u.emitFn = func(name string, p any) { got = append(got, name+":"+p.(wsclient.ManifestCheck).AvailableVersion) }
	u.announceUpdate(wsclient.ManifestCheck{Target: "emly", AvailableVersion: "3.5.0"})
	u.announceUpdate(wsclient.ManifestCheck{Target: "emly", AvailableVersion: "3.5.0"})
	u.announceUpdate(wsclient.ManifestCheck{Target: "updater", AvailableVersion: "1.9.1"})
	u.announceUpdate(wsclient.ManifestCheck{Target: "emly", AvailableVersion: "3.5.1"})
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
}

func TestInstallFailureCode(t *testing.T) {
	if c := installFailureCode(errors.New("post-install verification failed: config.ini reports 3.4.1, expected 3.5.0")); c != "version_mismatch" {
		t.Errorf("verification -> %s", c)
	}
	if c := installFailureCode(errors.New("EMLy_Setup.exe exited with code 2 (see x.log)")); c != "setup_exit_code" {
		t.Errorf("exit code -> %s", c)
	}
}
