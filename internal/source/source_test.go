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

// succeedingSource always returns m, standing in for a reachable fallback.
type succeedingSource struct {
	m     *manifest.Manifest
	calls int
}

func (s *succeedingSource) Name() string { return "succeeding" }
func (s *succeedingSource) FetchManifest(context.Context) (*manifest.Manifest, error) {
	s.calls++
	return s.m, nil
}
func (s *succeedingSource) ResolveTarget(m *manifest.Manifest, channel string) (manifest.Target, error) {
	return manifest.Target{}, nil
}
func (s *succeedingSource) FetchSetup(context.Context, manifest.Target, string) error {
	return nil
}

// A primary that never comes back must not fail the cycle when a fallback is
// reachable: the internal DC/subnet check can be right while the internal
// manifest endpoint itself is down.
func TestResolverFallsBackWhenPrimaryExhausted(t *testing.T) {
	primary := &failingSource{}
	fallback := &succeedingSource{m: &manifest.Manifest{StableVersion: "1.0.0"}}
	r := &Resolver{
		Primary:     primary,
		Fallback:    fallback,
		Attempts:    2,
		BaseBackoff: time.Millisecond,
	}

	src, m, err := r.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil", err)
	}
	if src != fallback {
		t.Errorf("Resolve() source = %v, want the fallback", src)
	}
	if m.StableVersion != "1.0.0" {
		t.Errorf("Resolve() manifest = %+v, want the fallback's", m)
	}
	if primary.calls != 2 {
		t.Errorf("expected primary to exhaust its 2 attempts, got %d", primary.calls)
	}
	if fallback.calls != 1 {
		t.Errorf("expected fallback to be tried once, got %d", fallback.calls)
	}
}

// When both primary and fallback fail, the original primary error is what
// callers see - it is the one that actually explains why updates stopped.
func TestResolverReturnsPrimaryErrorWhenFallbackAlsoFails(t *testing.T) {
	primary := &failingSource{}
	fallback := &failingSource{}
	r := &Resolver{
		Primary:     primary,
		Fallback:    fallback,
		Attempts:    1,
		BaseBackoff: time.Millisecond,
	}

	_, _, err := r.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected an error when both primary and fallback fail")
	}
	if fallback.calls != 1 {
		t.Errorf("expected fallback to be tried once, got %d", fallback.calls)
	}
}

// No fallback configured must behave exactly as before: no extra call, and
// the primary's own error is returned.
func TestResolverNoFallbackConfigured(t *testing.T) {
	primary := &failingSource{}
	r := &Resolver{
		Primary:     primary,
		Attempts:    1,
		BaseBackoff: time.Millisecond,
	}

	if _, _, err := r.Resolve(context.Background()); err == nil {
		t.Fatal("expected an error when primary fails and there is no fallback")
	}
}
