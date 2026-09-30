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

// The X-EMLy-AppVersion header reports what EMLy's own config.ini says is
// installed, and reports nothing at all when EMLy is not installed: the
// 0.0.0 the resolver substitutes there is a comparison sentinel, and an API
// that stored it would show the fleet a release that does not exist.
//
// X-EMLy-InstalledProducts comes from the same read and tells three states
// apart: installed ({emly: version}), positively absent ({} - the API drops
// the product), and unreadable (nil - no header, the API keeps what it has).
// Collapsing the last two would make every machine with a corrupt
// config.ini vanish from its owners' dashboard.
func TestInstalledProductsForHeaders(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.ini")
	if err := os.WriteFile(cfgPath, []byte("[EMLy]\nGUI_SEMVER=3.4.1\nGUI_RELEASE_CHANNEL=stable\n"), 0o644); err != nil {
		t.Fatalf("write config.ini: %v", err)
	}

	u := &Updater{Cfg: &config.Config{EMLyConfigFile: cfgPath}, Log: testLogger(t)}
	version, inv := u.installedProducts()
	if version != "3.4.1" {
		t.Errorf("installed: emly version = %q, want %q", version, "3.4.1")
	}
	if len(inv) != 1 || inv[ProductEMLy] != "3.4.1" {
		t.Errorf("installed: inventory = %v, want map[emly:3.4.1]", inv)
	}

	u.Cfg.EMLyConfigFile = filepath.Join(dir, "absent.ini")
	version, inv = u.installedProducts()
	if version != "" {
		t.Errorf("absent: emly version = %q, want empty", version)
	}
	if inv == nil || len(inv) != 0 {
		t.Errorf("absent: inventory = %#v, want a non-nil empty map", inv)
	}

	broken := filepath.Join(dir, "broken.ini")
	if err := os.WriteFile(broken, []byte("[EMLy]\nGUI_RELEASE_CHANNEL=stable\n"), 0o644); err != nil {
		t.Fatalf("write broken config.ini: %v", err)
	}
	u.Cfg.EMLyConfigFile = broken
	version, inv = u.installedProducts()
	if version != "" || inv != nil {
		t.Errorf("unreadable: got (%q, %#v), want (\"\", nil)", version, inv)
	}
}

// Every product of the document is reported - disabled ones too: enabled
// turns updates off, not dashboard visibility. One unreadable product voids
// the whole inventory, which is complete by definition.
func TestInstalledProductsCoversDocumentProducts(t *testing.T) {
	srv := newProductServer(t)
	h := newProductHarness(t, srv)
	s := h.u.Policy.Current().Parsed.Global.Products[rcSlug]
	s.Enabled = false
	h.u.Policy.Current().Parsed.Global.Products[rcSlug] = s

	_, inv := h.u.installedProducts()
	if inv["emly"] != "2.0.0" || inv[rcSlug] != "1.0.0" {
		t.Fatalf("inventory = %v, want emly 2.0.0 and %s 1.0.0", inv, rcSlug)
	}

	_ = os.WriteFile(filepath.Join(h.rcDir, "version.txt"), []byte("not-a-version"), 0o644)
	if _, inv := h.u.installedProducts(); inv != nil {
		t.Fatalf("inventory with an unknown product = %v, want nil", inv)
	}

	_ = os.Remove(filepath.Join(h.rcDir, "version.txt"))
	if _, inv := h.u.installedProducts(); inv == nil || len(inv) != 1 || inv["emly"] != "2.0.0" {
		t.Fatalf("inventory with the product absent = %v, want only emly", inv)
	}
}
