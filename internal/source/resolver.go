package source

import (
	"context"
	"errors"
	"fmt"
	"time"

	"emlyupdater/internal/manifest"
)

// Resolver fetches the manifest from Primary with retries and exponential
// backoff.
type Resolver struct {
	Primary Source

	// Fallbacks, when set, are tried in order, once each (no retries), after
	// Primary exhausts its attempts. They exist for the case where the
	// startup source policy correctly placed the machine on a mapped internal
	// LAN but the internal manifest endpoint itself is unreachable (down,
	// misconfigured, blocked by a firewall): a site can name a backup
	// internal endpoint to try before going out to the public API, and
	// either way the cycle keeps flowing instead of failing outright. A
	// fallback is never persisted to cfg.Primary - the next cycle tries
	// Primary again first.
	Fallbacks []Source

	// Attempts and BaseBackoff control primary retries; zero values get
	// defaults (3 attempts, 5s base backoff: 5s, 10s between tries).
	Attempts    int
	BaseBackoff time.Duration

	// Document names what is being fetched, for the log. Empty means
	// "manifest". A cycle fetches two of them from the same sources, so
	// without this the log carries two identical lines per cycle and neither
	// says which one it is about.
	Document string

	// Logf receives progress lines ("primary failed, retrying", ...). Optional.
	Logf func(format string, args ...any)
}

func (r *Resolver) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

func (r *Resolver) document() string {
	if r.Document == "" {
		return "manifest"
	}
	return r.Document
}

// Resolve fetches the manifest from the primary source, retrying with
// exponential backoff.
func (r *Resolver) Resolve(ctx context.Context) (Source, *manifest.Manifest, error) {
	return resolveWith(ctx, r, func(ctx context.Context, s Source) (*manifest.Manifest, error) {
		return s.FetchManifest(ctx)
	})
}

// ResolveUpdater fetches the updater's own release manifest the same way,
// asking each source for its own updater endpoint: urlFor derives it from that
// source's manifest URL, so the internal mirror is asked for the internal one
// and the external fallback for the public one.
//
// It also returns the URL that actually answered. A source's Name only carries
// the *EMLy* manifest URL it was built from, so without this the log could
// only ever name the base endpoint - never the one this fetch really went to,
// which is the address an operator needs when checking whether a site's mirror
// is serving updater releases at all.
//
// A source that answers 404 has told us it does not implement the endpoint;
// the fallback is still tried, and when nothing serves it the ErrNotFound
// comes back for the caller to treat as "no self-update here", not a failure.
func ResolveUpdater(ctx context.Context, r *Resolver, urlFor func(Source) (string, error)) (Source, *manifest.UpdaterManifest, string, error) {
	var servedBy string
	src, m, err := resolveWith(ctx, r, func(ctx context.Context, s Source) (*manifest.UpdaterManifest, error) {
		http, ok := s.(*HTTPSource)
		if !ok {
			return nil, fmt.Errorf("source %s cannot serve an updater manifest", s.Name())
		}
		url, err := urlFor(s)
		if err != nil {
			return nil, err
		}
		m, err := http.FetchUpdaterManifest(ctx, url)
		if err != nil {
			return nil, err
		}
		servedBy = url
		return m, nil
	})
	return src, m, servedBy, err
}

// resolveWith runs one document fetch against the primary source with retries
// and exponential backoff, then once against the fallback if there is one.
//
// It is generic over the document because the retry, backoff, fallback and
// logging policy must stay identical for EMLy's manifest and the updater's
// own: a machine that can only reach its site's mirror has to behave the same
// way for both, and two copies of this loop would drift.
func resolveWith[T any](ctx context.Context, r *Resolver, fetch func(context.Context, Source) (T, error)) (Source, T, error) {
	var zero T

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
				return nil, zero, ctx.Err()
			}
		}
		doc, err := fetch(ctx, r.Primary)
		if err == nil {
			r.logf("%s served by primary source %s", r.document(), r.Primary.Name())
			return r.Primary, doc, nil
		}
		lastErr = err
		r.logf("primary source %s attempt %d/%d failed to serve the %s: %v",
			r.Primary.Name(), i+1, attempts, r.document(), err)
		if ctx.Err() != nil {
			return nil, zero, ctx.Err()
		}
		// A 404 is a definitive answer, not a hiccup: backing off and asking
		// the same question again cannot change it.
		if errors.Is(err, ErrNotFound) {
			r.logf("primary source %s does not serve the %s, skipping the remaining attempts",
				r.Primary.Name(), r.document())
			break
		}
	}

	for _, fb := range r.Fallbacks {
		if ctx.Err() != nil {
			return nil, zero, ctx.Err()
		}
		r.logf("primary source %s did not serve the %s, trying fallback source %s",
			r.Primary.Name(), r.document(), fb.Name())
		doc, err := fetch(ctx, fb)
		if err != nil {
			r.logf("fallback source %s failed: %v", fb.Name(), err)
			// Surface the fallback's 404 so a caller that treats "nobody
			// serves this" as a no-op still sees it when the primary failed
			// for some other reason.
			if errors.Is(err, ErrNotFound) {
				lastErr = err
			}
		} else {
			r.logf("%s served by fallback source %s", r.document(), fb.Name())
			return fb, doc, nil
		}
	}

	return nil, zero, lastErr
}
