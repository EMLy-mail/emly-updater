package source

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"emlyupdater/internal/manifest"
)

// fetch429 runs FetchSetup against a server answering 429 with the given
// Retry-After header (omitted when empty) and body.
func fetch429(t *testing.T, retryAfterHeader, body string) error {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if retryAfterHeader != "" {
			w.Header().Set("Retry-After", retryAfterHeader)
		}
		if body != "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	s := NewHTTPSource(srv.URL + "/manifest")
	dest := filepath.Join(t.TempDir(), "setup.partial")
	return s.FetchSetup(context.Background(), manifest.Target{DownloadRef: srv.URL + "/setup"}, dest)
}

const queueFullBody = `{"error":"download queue full","message":"No free download slots are available right now. Retry in 45 seconds.","retry_after":45,"capacity":50,"active":50}`

func TestFetchSetup429(t *testing.T) {
	cases := []struct {
		name      string
		header    string
		body      string
		wait      time.Duration
		queueFull bool
	}{
		{"queue full, header wins over body", "120", queueFullBody, 120 * time.Second, true},
		{"queue full, body only", "", queueFullBody, 45 * time.Second, true},
		{"rate limiter, no body no header", "", "", DefaultRetryAfter, false},
		{"rate limiter, plain-text body", "", "Too Many Requests", DefaultRetryAfter, false},
		{"non-numeric header falls back to body", "abc", queueFullBody, 45 * time.Second, true},
		{"HTTP-date header falls back to default", "Wed, 21 Oct 2026 07:28:00 GMT", "", DefaultRetryAfter, false},
		{"zero header falls back to body", "0", queueFullBody, 45 * time.Second, true},
		{"negative header and body fall back to default", "-5", `{"error":"download queue full","retry_after":-1}`, DefaultRetryAfter, true},
		{"other JSON error is not the queue", "10", `{"error":"rate limited"}`, 10 * time.Second, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := fetch429(t, tc.header, tc.body)
			var rle *RetryLaterError
			if !errors.As(err, &rle) {
				t.Fatalf("expected *RetryLaterError, got %v", err)
			}
			if rle.Wait != tc.wait {
				t.Errorf("Wait = %s, want %s", rle.Wait, tc.wait)
			}
			if rle.QueueFull != tc.queueFull {
				t.Errorf("QueueFull = %v, want %v", rle.QueueFull, tc.queueFull)
			}
		})
	}
}

func TestFetchSetup429CarriesQueueDetails(t *testing.T) {
	err := fetch429(t, "60", queueFullBody)
	var rle *RetryLaterError
	if !errors.As(err, &rle) {
		t.Fatalf("expected *RetryLaterError, got %v", err)
	}
	if rle.Capacity != 50 || rle.Active != 50 || !strings.Contains(rle.Message, "No free download slots") {
		t.Fatalf("queue details not carried: %+v", rle)
	}
}

// An admin aborting a download from the dashboard closes the connection after
// the 200 and Content-Length, before the end of the body. That must fail the
// fetch - never hand back a short file as if it were complete.
func TestFetchSetupTruncatedAfter200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("only a part of the setup"))
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()

	s := NewHTTPSource(srv.URL + "/manifest")
	dest := filepath.Join(t.TempDir(), "setup.partial")
	err := s.FetchSetup(context.Background(), manifest.Target{DownloadRef: srv.URL + "/setup"}, dest)
	if err == nil {
		t.Fatal("truncated download accepted")
	}
	var rle *RetryLaterError
	if errors.As(err, &rle) {
		t.Fatalf("a truncated download is not a 429: %v", err)
	}
}
