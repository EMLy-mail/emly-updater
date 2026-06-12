package source

import (
	"context"
	"fmt"
	"time"

	"emlyupdater/internal/manifest"
)

// Resolver tries the primary source with retries and exponential backoff, then
// falls back to the UNC share. The winning source is returned alongside the
// manifest so the setup is fetched from the same place.
type Resolver struct {
	Primary  Source
	Fallback Source

	// Attempts and BaseBackoff control primary retries; zero values get
	// defaults (3 attempts, 5s base backoff: 5s, 10s between tries).
	Attempts    int
	BaseBackoff time.Duration

	// Logf receives progress lines ("primary failed, retrying", "fell back to
	// UNC", ...). Optional.
	Logf func(format string, args ...any)
}

func (r *Resolver) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// Resolve fetches the manifest, preferring the primary source.
func (r *Resolver) Resolve(ctx context.Context) (Source, *manifest.Manifest, error) {
	attempts := r.Attempts
	if attempts < 1 {
		attempts = 3
	}
	backoff := r.BaseBackoff
	if backoff <= 0 {
		backoff = 5 * time.Second
	}

	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-time.After(backoff):
				backoff *= 2
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		m, err := r.Primary.FetchManifest(ctx)
		if err == nil {
			r.logf("manifest served by primary source %s", r.Primary.Name())
			return r.Primary, m, nil
		}
		lastErr = err
		r.logf("primary source %s attempt %d/%d failed: %v", r.Primary.Name(), i+1, attempts, err)
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
	}

	// Fallback is a file read on the share - a single attempt is enough.
	m, err := r.Fallback.FetchManifest(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("primary source failed (%v) and UNC fallback failed: %w", lastErr, err)
	}
	r.logf("primary source exhausted (%v); manifest served by fallback %s", lastErr, r.Fallback.Name())
	return r.Fallback, m, nil
}
