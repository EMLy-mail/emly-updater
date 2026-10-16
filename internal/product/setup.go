package product

import (
	"path/filepath"

	version "github.com/hashicorp/go-version"
)

// setupChange records a product whose setup moved to another installer
// technology at a given version. The remote-configuration document names one
// installer type per product and has no way to say "from version X on", so
// the switch is pinned here: the document keeps describing the old setup,
// which is what every release before Since still is.
type setupChange struct {
	Since string // first version built with the new setup
	Type  string // its installer type
	// ForceUpgrade passes EMLy's /FORCEUPGRADE: the new setup was built from
	// EMLy's installer.iss and takes the same arguments.
	ForceUpgrade bool
}

// setupChanges, by slug.
var setupChanges = map[string]setupChange{
	// 3g-RocketChat went from NSIS to Inno Setup with 1.0.4.
	"3g-rocketchat": {Since: "1.0.4", Type: InstallerInno, ForceUpgrade: true},
}

// SetupFor returns the installer type that installs version v of p, and
// whether it takes EMLy's /FORCEUPGRADE: the document's type, unless the
// product's setup changed technology at or before v (setupChanges). EMLy's
// built-in definition always forces the upgrade. A version that does not
// parse keeps the document's type.
func (p *Product) SetupFor(v string) (typ string, forceUpgrade bool) {
	typ, forceUpgrade = p.Installer.Type, p.Legacy
	c, ok := setupChanges[p.Slug]
	if !ok {
		return typ, forceUpgrade
	}
	want, err1 := version.NewVersion(v)
	since, err2 := version.NewVersion(c.Since)
	if err1 != nil || err2 != nil || want.LessThan(since) {
		return typ, forceUpgrade
	}
	return c.Type, c.ForceUpgrade
}

// UninstallerType returns the installer type whose uninstaller is on disk
// for p: for a product whose setup changed technology (setupChanges), the
// version installed may predate the change or not, so an Inno uninstaller
// (unins*.exe) in the install directory decides; otherwise the document's
// type.
func (p *Product) UninstallerType() string {
	c, ok := setupChanges[p.Slug]
	if !ok || c.Type != InstallerInno {
		return p.Installer.Type
	}
	if m, _ := filepath.Glob(filepath.Join(p.InstallDir, "unins*.exe")); len(m) > 0 {
		return InstallerInno
	}
	return p.Installer.Type
}
