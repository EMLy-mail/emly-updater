package source

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ConfigResponse is the outcome of one GET /v2/config attempt that the
// server answered meaningfully. Transport failures and unexpected statuses
// are errors instead, so the caller moves on to the next candidate.
type ConfigResponse struct {
	// Status is 200 (Body carries a document), 304 (the cached copy is
	// current) or 204 (the server has nothing published).
	Status int
	Body   []byte
	ETag   string
}

// ErrConfigTooLarge reports a body past the size a document may have.
var ErrConfigTooLarge = fmt.Errorf("configuration document exceeds the size limit")

// FetchConfig performs one conditional GET of the remote configuration at
// url with this source's identification headers. etag, when non-empty, is
// sent as If-None-Match so an unchanged document costs a 304. maxBody caps
// the body read; a longer one is ErrConfigTooLarge.
//
// timeout bounds the whole attempt: the fetch is best-effort and runs every
// cycle, so a hung endpoint must fail fast rather than hold the cycle.
func (s *HTTPSource) FetchConfig(ctx context.Context, url, etag string, timeout time.Duration, maxBody int64) (*ConfigResponse, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid config URL %q: %w", url, err)
	}
	s.applyHeaders(req)
	req.Header.Set("Accept", "application/json")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("config request failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified, http.StatusNoContent:
		// Drain so the connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		return &ConfigResponse{Status: resp.StatusCode, ETag: resp.Header.Get("ETag")}, nil
	case http.StatusOK:
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		if err != nil {
			return nil, fmt.Errorf("failed to read config body: %w", err)
		}
		if int64(len(body)) > maxBody {
			return nil, ErrConfigTooLarge
		}
		return &ConfigResponse{Status: resp.StatusCode, Body: body, ETag: resp.Header.Get("ETag")}, nil
	case http.StatusNotFound:
		return nil, fmt.Errorf("%s: %w", url, ErrNotFound)
	default:
		return nil, fmt.Errorf("config endpoint returned HTTP %d", resp.StatusCode)
	}
}
