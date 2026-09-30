package source

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestFetchSetupReportsProgress(t *testing.T) {
	type call struct{ done, total int64 }
	var calls []call
	ctx := WithProgress(context.Background(), func(done, total int64) {
		calls = append(calls, call{done, total})
	})
	_, err := fetchWithIdle(t, time.Second, ctx, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "6")
		w.WriteHeader(http.StatusOK)
		for range 3 {
			_, _ = w.Write([]byte("xx"))
			w.(http.Flusher).Flush()
		}
	})
	if err != nil {
		t.Fatalf("FetchSetup: %v", err)
	}
	if len(calls) < 2 {
		t.Fatalf("got %d progress calls, want the initial one plus at least one read: %v", len(calls), calls)
	}
	if calls[0] != (call{0, 6}) {
		t.Errorf("first call = %v, want {0 6}: the window must learn the size before the first byte", calls[0])
	}
	if last := calls[len(calls)-1]; last != (call{6, 6}) {
		t.Errorf("last call = %v, want {6 6}", last)
	}
}

// A refused download never reports: the window must not open for a request
// the server has queued (HTTP 429) rather than served.
func TestFetchSetupNoProgressOnRefusal(t *testing.T) {
	called := false
	ctx := WithProgress(context.Background(), func(int64, int64) { called = true })
	_, err := fetchWithIdle(t, time.Second, ctx, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	if err == nil {
		t.Fatal("FetchSetup succeeded on a 429")
	}
	if called {
		t.Error("progress reported for a download the server refused")
	}
}
