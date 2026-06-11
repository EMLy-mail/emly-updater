// Package source abstracts where update information and setup binaries come
// from: an HTTP(S) endpoint (external or internal) or the UNC fallback share.
//
// The setup is always fetched from the same source that served the manifest,
// and each source resolves the channel target itself because the checksum-key
// convention differs (API keys by version, UNC share keys by filename).
package source

import (
	"context"

	"emlyupdater/internal/manifest"
)

// Source serves the update manifest and the setup executable.
type Source interface {
	// Name identifies the source in logs, e.g. "http(https://...)" or "unc(\\\\srv\\share)".
	Name() string
	// FetchManifest retrieves and parses the manifest.
	FetchManifest(ctx context.Context) (*manifest.Manifest, error)
	// ResolveTarget resolves the channel's version, download reference and
	// expected SHA256 using this source's conventions. It fails when the
	// manifest carries no checksum for the target: an unattended SYSTEM
	// service must never install an unverifiable binary.
	ResolveTarget(m *manifest.Manifest, channel string) (manifest.Target, error)
	// FetchSetup downloads/copies the setup referenced by t to destPath.
	FetchSetup(ctx context.Context, t manifest.Target, destPath string) error
}
