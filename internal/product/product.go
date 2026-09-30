// Package product describes what the agent updates: EMLy, which is built in,
// and every product the remote-configuration document lists under
// "products". It knows how to tell which version of a product is installed;
// it does no network I/O and runs nothing.
package product

// EMLySlug is EMLy's product slug, the one the API uses in the inventory and
// in /v2/updates/{slug}/.... EMLy itself keeps the historical, slug-less
// manifest route (see ManifestPath).
const EMLySlug = "emly"

// Version source types. Closed on purpose: the document picks among readers
// implemented here, it cannot name an arbitrary command.
const (
	SourceINI  = "ini"
	SourceFile = "file"
	SourceExe  = "exe"
)

// Installer driver types, see internal/installer.
const (
	InstallerInno = "inno"
	InstallerNSIS = "nsis"
)

// VersionSource is one link of a product's detection chain. Path is relative
// to Product.InstallDir; an absolute Path is used as-is (only the built-in
// EMLy definition uses one - the document's validator forbids it).
type VersionSource struct {
	Type    string
	Path    string
	Section string // ini only
	Key     string // ini only
}

// InstallerSpec selects the setup driver.
type InstallerSpec struct {
	Type string
	// CleanReinstall runs the product's uninstaller before the retry that
	// follows a failed install. Off by default for products: an NSIS
	// uninstaller typically removes the whole install directory, user data
	// and IT-managed configuration included.
	CleanReinstall bool
}

// Product is one updatable product.
type Product struct {
	Slug       string
	Name       string // user-facing text and logs
	InstallDir string
	ExeName    string // process waited for / closed before an install
	Channel    string // "stable" | "beta"
	Detect     []VersionSource
	Installer  InstallerSpec
	// InstallWhenAbsent installs the product on a machine that does not have
	// it. EMLy: always (its historical fresh-install mode). Others: off
	// unless the document turns it on.
	InstallWhenAbsent bool
	// Legacy marks the built-in EMLy definition, which keeps behaviour no
	// other product has: file associations, the localized critical warning,
	// the blocking wait for the app to close, update.* events, unlimited
	// retries. See the spec, section 3.4.
	Legacy bool
}

// ManifestPath is the manifest route for this product, relative to a
// server's base URL.
func (p *Product) ManifestPath() string {
	if p.Slug == EMLySlug {
		return "/v2/updates/manifest"
	}
	return "/v2/updates/" + p.Slug + "/manifest"
}
