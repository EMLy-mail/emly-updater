// Package source abstracts where update information and setup binaries come
// from: an HTTP(S) endpoint (external or internal).
//
// The setup is always fetched from the same source that served the manifest,
// and each source resolves the channel target itself.
package source

import (
	"context"
	"errors"

	"emlyupdater/internal/manifest"
)

// ErrNotFound reports that an endpoint answered HTTP 404 - a deterministic
// "this host does not serve that document", as opposed to a transient failure.
// Retrying the identical request cannot change it, so the resolver skips
// straight to its fallback instead of backing off first.
var ErrNotFound = errors.New("endpoint not found")

// Source serves the update manifest and the setup executable.
type Source interface {
	// Name identifies the source in logs, e.g. "http(https://...)".
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
