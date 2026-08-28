package source

import (
	"context"
	"errors"
	"testing"
	"time"

	"emlyupdater/internal/manifest"
)

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

func TestResolverRetriesThenFails(t *testing.T) {
	primary := &failingSource{}
	r := &Resolver{
		Primary:     primary,
		Attempts:    3,
		BaseBackoff: time.Millisecond,
	}

	if _, _, err := r.Resolve(context.Background()); err == nil {
		t.Fatal("expected error when primary fails every attempt")
	}
	if primary.calls != 3 {
		t.Fatalf("expected 3 primary attempts, got %d", primary.calls)
	}
}
