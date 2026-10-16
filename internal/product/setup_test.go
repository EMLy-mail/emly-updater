package product

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSetupFor(t *testing.T) {
	rc := &Product{Slug: "3g-rocketchat", Installer: InstallerSpec{Type: InstallerNSIS}}
	for _, tc := range []struct {
		version string
		typ     string
		force   bool
	}{
		{"1.0.3", InstallerNSIS, false},
		{"1.0.3-beta.2", InstallerNSIS, false},
		{"1.0.4", InstallerInno, true},
		{"1.0.4-beta.1", InstallerNSIS, false}, // a pre-release sorts before 1.0.4
		{"1.1.0", InstallerInno, true},
		{"2.0.0", InstallerInno, true},
		{"not-a-version", InstallerNSIS, false},
	} {
		typ, force := rc.SetupFor(tc.version)
		if typ != tc.typ || force != tc.force {
			t.Errorf("3g-rocketchat %s: got %s/%v, want %s/%v", tc.version, typ, force, tc.typ, tc.force)
		}
	}

	other := &Product{Slug: "other", Installer: InstallerSpec{Type: InstallerNSIS}}
	if typ, force := other.SetupFor("9.9.9"); typ != InstallerNSIS || force {
		t.Errorf("other product: got %s/%v, want the document's nsis without /FORCEUPGRADE", typ, force)
	}
	emly := &Product{Slug: EMLySlug, Legacy: true, Installer: InstallerSpec{Type: InstallerInno}}
	if typ, force := emly.SetupFor("1.0.0"); typ != InstallerInno || !force {
		t.Errorf("emly: got %s/%v, want inno with /FORCEUPGRADE", typ, force)
	}
}

func TestUninstallerType(t *testing.T) {
	dir := t.TempDir()
	rc := &Product{Slug: "3g-rocketchat", InstallDir: dir, Installer: InstallerSpec{Type: InstallerNSIS}}
	if got := rc.UninstallerType(); got != InstallerNSIS {
		t.Errorf("NSIS install on disk: got %s, want nsis", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "unins000.exe"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := rc.UninstallerType(); got != InstallerInno {
		t.Errorf("Inno install on disk: got %s, want inno", got)
	}
	other := &Product{Slug: "other", InstallDir: dir, Installer: InstallerSpec{Type: InstallerNSIS}}
	if got := other.UninstallerType(); got != InstallerNSIS {
		t.Errorf("product without a setup change: got %s, want the document's nsis", got)
	}
}
