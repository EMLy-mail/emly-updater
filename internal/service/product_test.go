package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"emlyupdater/internal/config"
	"emlyupdater/internal/download"
	"emlyupdater/internal/installer"
	"emlyupdater/internal/product"
	"emlyupdater/internal/state"
)

// fakeDriver stands in for a setup: Install writes version.txt into the
// product's directory (what a real setup does), unless failInstall is set.
type fakeDriver struct {
	dir         string
	failInstall bool
	installs    []string
	uninstalls  int
}

func (d *fakeDriver) Install(_ string, version string) error {
	d.installs = append(d.installs, version)
	if d.failInstall {
		return errors.New("setup exited with code 2")
	}
	return os.WriteFile(filepath.Join(d.dir, "version.txt"), []byte(version+"\r\n"), 0o644)
}

func (d *fakeDriver) Uninstall() error { d.uninstalls++; return nil }

// The built-in EMLy definition reproduces what the cycle has always done.
func TestEMLyProductMatchesTheLegacyConfiguration(t *testing.T) {
	cfg := internalCfg(t, config.SourceInternal)
	u := newTestUpdater(t, cfg, dcNamed("DC-RM2"), ipsOf("172.16.96.50"))
	p := u.emlyProduct(nil)
	if p.Slug != product.EMLySlug || !p.Legacy || !p.InstallWhenAbsent {
		t.Fatalf("emly product = %+v", p)
	}
	if p.InstallDir != cfg.EMLyInstallDir || p.ExeName != cfg.EMLyExeName || p.Name != "EMLy" {
		t.Errorf("emly paths = %+v", p)
	}
	if p.Installer.Type != product.InstallerInno || !p.Installer.CleanReinstall {
		t.Errorf("emly installer = %+v", p.Installer)
	}
	if len(p.Detect) != 1 || p.Detect[0].Path != cfg.EMLyConfigFile || p.Detect[0].Key != "GUI_SEMVER" {
		t.Errorf("emly detect = %+v", p.Detect)
	}
}

func rocketChat(dir string) *product.Product {
	return &product.Product{
		Slug: "3g-rocketchat", Name: "3g-RocketChat", InstallDir: dir, ExeName: "3g-RocketChat.exe",
		Channel: "stable", Installer: product.InstallerSpec{Type: product.InstallerNSIS},
		Detect: []product.VersionSource{{Type: product.SourceINI, Path: "config.ini", Section: "app", Key: "version"}},
	}
}

// A generic product installs through its driver, is verified by re-running
// its detection chain, and its pending entry is cleared afterwards.
func TestInstallProductVerifiesThroughDetection(t *testing.T) {
	dir := t.TempDir()
	u := newClientTestUpdater(t)
	u.Store = &state.Store{Path: filepath.Join(t.TempDir(), "state.json")}
	u.Downloads = &download.Manager{Dir: t.TempDir()}
	u.toastFn = func(string, string, string) bool { return true }

	p := rocketChat(dir)
	// Detection reads config.ini; the fake setup writes it.
	drv := &iniDriver{dir: dir}
	u.driverFn = func(*product.Product) installer.Driver { return drv }

	setup := filepath.Join(t.TempDir(), "setup.exe")
	_ = os.WriteFile(setup, []byte("setup-bytes"), 0o644)
	sum := sha256.Sum256([]byte("setup-bytes"))
	pend := &state.Pending{Version: "1.1.0", SetupPath: setup, SHA256: hex.EncodeToString(sum[:])}
	_ = u.Store.SetPendingFor(p.Slug, pend)

	if err := u.installProduct(context.Background(), nil, p, pend, productState{Installed: "1.0.0", Channel: "stable", Language: "it"}); err != nil {
		t.Fatalf("installProduct: %v", err)
	}
	if len(drv.installs) != 1 || drv.installs[0] != "1.1.0" {
		t.Errorf("installs = %v", drv.installs)
	}
	if got, _ := u.Store.PendingFor(p.Slug); got != nil {
		t.Errorf("pending not cleared: %+v", got)
	}
}

// iniDriver writes [app] version the way RocketChat's future setup will.
type iniDriver struct {
	dir      string
	installs []string
}

func (d *iniDriver) Install(_ string, version string) error {
	d.installs = append(d.installs, version)
	return os.WriteFile(filepath.Join(d.dir, "config.ini"), []byte("[app]\nversion = "+version+"\n"), 0o644)
}
func (d *iniDriver) Uninstall() error { return nil }
