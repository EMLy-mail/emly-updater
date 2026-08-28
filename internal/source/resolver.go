package source

import (
	"context"
	"time"

	"emlyupdater/internal/manifest"
)

// Resolver fetches the manifest from Primary with retries and exponential
// backoff.
type Resolver struct {
	Primary Source

	// Fallback, when set, is tried once (no retries) after Primary exhausts
	// its attempts. It exists for the case where the startup source policy
	// correctly placed the machine on a mapped internal LAN but the internal
	// manifest endpoint itself is unreachable (down, misconfigured, blocked
	// by a firewall): falling back keeps updates flowing for this cycle
	// instead of failing it outright. The fallback is never persisted to
	// cfg.Primary - the next cycle tries Primary again first.
	Fallback Source

	// Attempts and BaseBackoff control primary retries; zero values get
	// defaults (3 attempts, 5s base backoff: 5s, 10s between tries).
	Attempts    int
	BaseBackoff time.Duration

	// Logf receives progress lines ("primary failed, retrying", ...). Optional.
	Logf func(format string, args ...any)
}

func (r *Resolver) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// Resolve fetches the manifest from the primary source, retrying with
// exponential backoff.
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

	if r.Fallback != nil {
		r.logf("primary source %s exhausted after %d attempts, trying fallback source %s", r.Primary.Name(), attempts, r.Fallback.Name())
		m, err := r.Fallback.FetchManifest(ctx)
		if err != nil {
			r.logf("fallback source %s failed: %v", r.Fallback.Name(), err)
		} else {
			r.logf("manifest served by fallback source %s", r.Fallback.Name())
			return r.Fallback, m, nil
		}
	}

	return nil, nil, lastErr
}
