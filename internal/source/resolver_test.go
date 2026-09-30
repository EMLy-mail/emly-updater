package source

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"emlyupdater/internal/manifest"
)

type countingSource struct {
	calls int
	err   error
}

func (s *countingSource) Name() string { return "counting" }
func (s *countingSource) FetchManifest(context.Context) (*manifest.Manifest, error) {
	s.calls++
	return nil, s.err
}
func (s *countingSource) ResolveTarget(*manifest.Manifest, string) (manifest.Target, error) {
	return manifest.Target{}, nil
}
func (s *countingSource) FetchSetup(context.Context, manifest.Target, string) error { return nil }

// "Nothing published" does not change if asked again: one request, no backoff.
func TestResolverDoesNotRetryANoRelease(t *testing.T) {
	src := &countingSource{err: fmt.Errorf("x: %w", manifest.ErrNoRelease)}
	r := &Resolver{Primary: src, Attempts: 3}
	if _, _, err := r.Resolve(context.Background()); !errorsIsNoRelease(err) {
		t.Fatalf("Resolve = %v", err)
	}
	if src.calls != 1 {
		t.Fatalf("primary asked %d times, want 1", src.calls)
	}
}

func errorsIsNoRelease(err error) bool { return errors.Is(err, manifest.ErrNoRelease) }
