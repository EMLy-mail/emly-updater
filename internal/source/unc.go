package source

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"emlyupdater/internal/manifest"
)

// ManifestFileName is the manifest's filename on the UNC share. This matches
// the convention EMLy's in-app updater already uses, so the existing share
// needs no restructuring.
const ManifestFileName = "version.json"

// UNCSource reads the manifest and the setup from a network share (or any
// directory path - handy for testing with a local folder).
//
// Share conventions, inherited from EMLy's in-app updater: the manifest's
// stableDownload/betaDownload fields hold *filenames relative to the share
// root*, and sha256Checksums is keyed by those filenames.
type UNCSource struct {
	Root string // e.g. \\dc-rm2\logo\update
}

func NewUNCSource(root string) *UNCSource {
	return &UNCSource{Root: root}
}

func (s *UNCSource) Name() string {
	return fmt.Sprintf("unc(%s)", s.Root)
}

func (s *UNCSource) FetchManifest(_ context.Context) (*manifest.Manifest, error) {
	path := filepath.Join(s.Root, ManifestFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}
	return manifest.Parse(data)
}

func (s *UNCSource) ResolveTarget(m *manifest.Manifest, channel string) (manifest.Target, error) {
	version, filename, err := m.ChannelVersion(channel)
	if err != nil {
		return manifest.Target{}, err
	}
	// Reject download refs that escape the share root (defense in depth: the
	// share is IT-controlled, but the manifest is still parsed input).
	if filepath.Base(filename) != filename {
		return manifest.Target{}, fmt.Errorf("UNC manifest download ref %q is not a bare filename", filename)
	}

	// Share manifests key checksums by filename; fall back to version keying
	// in case the share manifest is ever regenerated in the API shape.
	sha := m.SHA256Checksums[filename]
	if sha == "" {
		sha = m.SHA256Checksums[version]
	}
	if sha == "" {
		return manifest.Target{}, fmt.Errorf("manifest carries no SHA256 checksum for %s (version %s)", filename, version)
	}

	return manifest.Target{
		Version:     version,
		DownloadRef: filepath.Join(s.Root, filename),
		SHA256:      sha,
	}, nil
}

func (s *UNCSource) FetchSetup(_ context.Context, t manifest.Target, destPath string) error {
	src, err := os.Open(t.DownloadRef)
	if err != nil {
		return fmt.Errorf("failed to open %s: %w", t.DownloadRef, err)
	}
	defer src.Close()

	dest, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create %s: %w", destPath, err)
	}
	defer dest.Close()

	if _, err := io.Copy(dest, src); err != nil {
		return fmt.Errorf("copy from share interrupted: %w", err)
	}
	return dest.Close()
}
