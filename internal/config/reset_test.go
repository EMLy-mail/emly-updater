package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withDataDir points DataDir at a temp directory so BackupPath writes there.
func withDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ProgramData", dir)
	return dir
}

func TestResetOverwritesWithDefaults(t *testing.T) {
	withDataDir(t)
	if err := EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	path := ConfigPath()
	old := "[updater]\npollIntervalMinutes = 5\nchannelOverride = test-marker-zz\n"
	if err := os.WriteFile(path, []byte(old), 0644); err != nil {
		t.Fatal(err)
	}

	changed, err := Reset(path)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("Reset reported no change over a customised config")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(defaultINI) {
		t.Error("config.ini is not this build's embedded defaults after Reset")
	}
	if strings.Contains(string(got), "test-marker-zz") {
		t.Error("a per-machine value survived the reset")
	}

	backup, err := os.ReadFile(BackupPath())
	if err != nil {
		t.Fatalf("no backup written: %v", err)
	}
	if string(backup) != old {
		t.Error("config.prev.ini does not hold the pre-reset config")
	}
}

func TestResetMissingFileWritesDefaults(t *testing.T) {
	withDataDir(t)
	path := filepath.Join(t.TempDir(), "config.ini")

	changed, err := Reset(path)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("Reset reported no change on a fresh install")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(defaultINI) {
		t.Error("fresh install did not get the embedded defaults")
	}
}

func TestResetPristineConfigIsNoOp(t *testing.T) {
	dataDir := withDataDir(t)
	path := filepath.Join(t.TempDir(), "config.ini")
	if err := os.WriteFile(path, defaultINI, 0644); err != nil {
		t.Fatal(err)
	}

	changed, err := Reset(path)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("Reset rewrote a config that already matches the defaults")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "EMLyUpdater", "config.prev.ini")); !os.IsNotExist(err) {
		t.Error("a backup was written for a pristine config")
	}
}

// The config Reset writes must load with this build's version resolved into
// the User-Agent.
func TestResetConfigLoads(t *testing.T) {
	withDataDir(t)
	path := filepath.Join(t.TempDir(), "config.ini")
	if _, err := Reset(path); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("reset config does not load: %v", err)
	}
	if strings.Contains(cfg.UserAgent, "{{VERSION}}") {
		t.Errorf("User-Agent placeholder not resolved: %q", cfg.UserAgent)
	}
}
