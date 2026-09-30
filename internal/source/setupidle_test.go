package source

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"emlyupdater/internal/manifest"
)

// The server bounds a download's total duration itself and can raise it up to
// a day, so the client must not cap the whole download - only give up on one
// that has stopped moving.
func TestNewHTTPSourceHasNoOverallTimeout(t *testing.T) {
	if got := NewHTTPSource("x").Client.Timeout; got != 0 {
		t.Fatalf("Client.Timeout = %s, want 0: a total cap truncates slow downloads the server still allows", got)
	}
}

func fetchWithIdle(t *testing.T, idle time.Duration, ctx context.Context, h http.HandlerFunc) (string, error) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	s := NewHTTPSource(srv.URL + "/manifest")
	s.setupIdleTimeout = idle
	dest := filepath.Join(t.TempDir(), "setup.partial")
	return dest, s.FetchSetup(ctx, manifest.Target{DownloadRef: srv.URL + "/setup"}, dest)
}

// A slow line that keeps delivering bytes must finish however long the whole
// download takes - here 6x the idle timeout.
func TestFetchSetupSlowButAliveCompletes(t *testing.T) {
	const chunks = 12
	dest, err := fetchWithIdle(t, 100*time.Millisecond, context.Background(), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "12")
		w.WriteHeader(http.StatusOK)
		for range chunks {
			_, _ = w.Write([]byte("x"))
			w.(http.Flusher).Flush()
			time.Sleep(50 * time.Millisecond)
		}
	})
	if err != nil {
		t.Fatalf("slow but alive download failed: %v", err)
	}
	if data, _ := os.ReadFile(dest); len(data) != chunks {
		t.Fatalf("got %d bytes, want %d", len(data), chunks)
	}
}

func TestFetchSetupStalledMidBody(t *testing.T) {
	release := make(chan struct{})
	defer close(release) // before t.Cleanup's srv.Close, which waits for the handler
	_, err := fetchWithIdle(t, 100*time.Millisecond, context.Background(), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		<-release // then nothing more
	})
	if !errors.Is(err, ErrSetupStalled) {
		t.Fatalf("expected ErrSetupStalled, got %v", err)
	}
}

func TestFetchSetupStalledBeforeHeaders(t *testing.T) {
	release := make(chan struct{})
	defer close(release) // before t.Cleanup's srv.Close, which waits for the handler
	_, err := fetchWithIdle(t, 100*time.Millisecond, context.Background(), func(http.ResponseWriter, *http.Request) {
		<-release
	})
	if !errors.Is(err, ErrSetupStalled) {
		t.Fatalf("expected ErrSetupStalled, got %v", err)
	}
}

// Stopping the service is not a stall: the caller's cancellation comes back
// as such.
func TestFetchSetupCallerCancelIsNotAStall(t *testing.T) {
	release := make(chan struct{})
	defer close(release) // before t.Cleanup's srv.Close, which waits for the handler
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := fetchWithIdle(t, time.Minute, ctx, func(http.ResponseWriter, *http.Request) {
		<-release
	})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrSetupStalled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
