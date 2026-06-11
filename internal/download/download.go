// Package download caches setup executables under
// %ProgramData%\EMLyUpdater\downloads, verifies SHA256 digests, and cleans up
// superseded versions. A setup is never handed to the installer without a
// matching checksum.
package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"emlyupdater/internal/manifest"
	"emlyupdater/internal/source"
)

// Manager owns the downloads directory.
type Manager struct {
	Dir string
}

// SetupPath returns the cache path for a given version.
func (m *Manager) SetupPath(version string) string {
	return filepath.Join(m.Dir, fmt.Sprintf("EMLy-%s-setup.exe", version))
}

// Ensure returns a local setup path for the target whose SHA256 matches.
// A cached file with a valid checksum is reused; otherwise the setup is
// fetched from src into a .partial file, verified, and renamed into place so
// a crashed download can never be mistaken for a complete one.
func (m *Manager) Ensure(ctx context.Context, src source.Source, t manifest.Target) (string, error) {
	if err := os.MkdirAll(m.Dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create downloads dir: %w", err)
	}

	dest := m.SetupPath(t.Version)
	if err := VerifyFile(dest, t.SHA256); err == nil {
		return dest, nil
	}
	// Missing or corrupt cache entry: remove and re-fetch.
	_ = os.Remove(dest)

	partial := dest + ".partial"
	defer os.Remove(partial) // no-op after a successful rename

	if err := src.FetchSetup(ctx, t, partial); err != nil {
		return "", fmt.Errorf("fetch from %s failed: %w", src.Name(), err)
	}
	if err := VerifyFile(partial, t.SHA256); err != nil {
		return "", err
	}
	if err := os.Rename(partial, dest); err != nil {
		return "", fmt.Errorf("failed to finalize download: %w", err)
	}
	return dest, nil
}

// CleanupExcept removes cached setups and partial downloads for any version
// other than keepVersion. Pass "" to clear the whole cache.
func (m *Manager) CleanupExcept(keepVersion string) error {
	entries, err := os.ReadDir(m.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	keep := ""
	if keepVersion != "" {
		keep = filepath.Base(m.SetupPath(keepVersion))
	}

	var firstErr error
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == keep {
			continue
		}
		if !strings.HasPrefix(name, "EMLy-") {
			continue // never touch files the updater did not create
		}
		if err := os.Remove(filepath.Join(m.Dir, name)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// VerifyFile checks a file's SHA256 digest against expected (hex,
// case-insensitive). It fails on missing files and on checksum mismatch.
func VerifyFile(path, expected string) error {
	if expected == "" {
		return fmt.Errorf("no expected checksum for %s", path)
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}

	actual := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", path, expected, actual)
	}
	return nil
}
