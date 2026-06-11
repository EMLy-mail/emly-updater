package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"emlyupdater/internal/manifest"
)

func uncManifest() *manifest.Manifest {
	// Share manifests carry filenames as download refs and key checksums by
	// filename — the convention EMLy's in-app updater established.
	return &manifest.Manifest{
		StableVersion:  "1.7.3",
		BetaVersion:    "1.7.5",
		StableDownload: "EMLy_Installer_1.7.3.exe",
		BetaDownload:   "EMLy_Installer_1.7.5_beta.exe",
		SHA256Checksums: map[string]string{
			"EMLy_Installer_1.7.3.exe":      "aaa",
			"EMLy_Installer_1.7.5_beta.exe": "bbb",
		},
	}
}

func TestUNCResolveTargetFilenameKeyed(t *testing.T) {
	s := NewUNCSource(`\\srv\share`)
	target, err := s.ResolveTarget(uncManifest(), "beta")
	if err != nil {
		t.Fatal(err)
	}
	if target.Version != "1.7.5" || target.SHA256 != "bbb" {
		t.Fatalf("unexpected target: %+v", target)
	}
	if target.DownloadRef != filepath.Join(`\\srv\share`, "EMLy_Installer_1.7.5_beta.exe") {
		t.Fatalf("unexpected download ref: %s", target.DownloadRef)
	}
}

func TestUNCResolveTargetVersionKeyFallback(t *testing.T) {
	m := uncManifest()
	m.SHA256Checksums = map[string]string{"1.7.5": "ccc"} // API-shaped checksums on the share
	target, err := NewUNCSource(`\\srv\share`).ResolveTarget(m, "beta")
	if err != nil {
		t.Fatal(err)
	}
	if target.SHA256 != "ccc" {
		t.Fatalf("version-key fallback not used: %+v", target)
	}
}

func TestUNCResolveTargetRejectsTraversal(t *testing.T) {
	m := uncManifest()
	m.BetaDownload = `..\..\evil.exe`
	if _, err := NewUNCSource(`\\srv\share`).ResolveTarget(m, "beta"); err == nil {
		t.Fatal("expected rejection of non-bare filename")
	}
}

func TestUNCResolveTargetMissingChecksum(t *testing.T) {
	m := uncManifest()
	m.SHA256Checksums = nil
	if _, err := NewUNCSource(`\\srv\share`).ResolveTarget(m, "beta"); err == nil {
		t.Fatal("expected error when no checksum is available")
	}
}

func TestHTTPResolveTargetVersionKeyed(t *testing.T) {
	m := &manifest.Manifest{
		StableVersion:   "1.7.3",
		StableDownload:  "https://api.example/releases/1.7.3/download",
		SHA256Checksums: map[string]string{"1.7.3": "abc"},
	}
	target, err := NewHTTPSource("https://api.example/manifest").ResolveTarget(m, "stable")
	if err != nil {
		t.Fatal(err)
	}
	if target.SHA256 != "abc" || target.DownloadRef != m.StableDownload {
		t.Fatalf("unexpected target: %+v", target)
	}

	m.SHA256Checksums = nil
	if _, err := NewHTTPSource("x").ResolveTarget(m, "stable"); err == nil {
		t.Fatal("expected error when no checksum is available")
	}
}

func TestUNCFetchManifestAndSetup(t *testing.T) {
	// A local directory stands in for the share — same code path.
	root := t.TempDir()
	manifestJSON := `{"stableVersion":"1.0.0","stableDownload":"setup.exe","sha256Checksums":{"setup.exe":"x"}}`
	if err := os.WriteFile(filepath.Join(root, ManifestFileName), []byte("\xEF\xBB\xBF"+manifestJSON), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "setup.exe"), []byte("payload"), 0644); err != nil {
		t.Fatal(err)
	}

	s := NewUNCSource(root)
	m, err := s.FetchManifest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	target, err := s.ResolveTarget(m, "stable")
	if err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "out.exe")
	if err := s.FetchSetup(context.Background(), target, dest); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "payload" {
		t.Fatalf("setup copy wrong: %q %v", data, err)
	}
}

// failingSource always errors, standing in for an unreachable primary.
type failingSource struct{ calls int }

func (f *failingSource) Name() string { return "failing" }
func (f *failingSource) FetchManifest(context.Context) (*manifest.Manifest, error) {
	f.calls++
	return nil, errors.New("connection refused")
}
func (f *failingSource) ResolveTarget(*manifest.Manifest, string) (manifest.Target, error) {
	return manifest.Target{}, errors.New("unreachable")
}
func (f *failingSource) FetchSetup(context.Context, manifest.Target, string) error {
	return errors.New("unreachable")
}

func TestResolverFallsBackToUNC(t *testing.T) {
	root := t.TempDir()
	manifestJSON := `{"stableVersion":"1.0.0","stableDownload":"setup.exe","sha256Checksums":{"setup.exe":"x"}}`
	if err := os.WriteFile(filepath.Join(root, ManifestFileName), []byte(manifestJSON), 0644); err != nil {
		t.Fatal(err)
	}

	primary := &failingSource{}
	r := &Resolver{
		Primary:     primary,
		Fallback:    NewUNCSource(root),
		Attempts:    3,
		BaseBackoff: time.Millisecond,
	}

	src, m, err := r.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := src.(*UNCSource); !ok {
		t.Fatalf("expected UNC fallback to win, got %s", src.Name())
	}
	if m.StableVersion != "1.0.0" {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	if primary.calls != 3 {
		t.Fatalf("expected 3 primary attempts, got %d", primary.calls)
	}
}

func TestResolverBothFail(t *testing.T) {
	r := &Resolver{
		Primary:     &failingSource{},
		Fallback:    NewUNCSource(filepath.Join(t.TempDir(), "missing")),
		Attempts:    1,
		BaseBackoff: time.Millisecond,
	}
	if _, _, err := r.Resolve(context.Background()); err == nil {
		t.Fatal("expected error when primary and fallback both fail")
	}
}
