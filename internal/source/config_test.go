package source

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A conditional GET must carry If-None-Match when a cached ETag exists, and
// a 304 must come back as a status rather than an error: it is the common
// case once the fleet is settled, not a failure.
func TestFetchConfigConditional(t *testing.T) {
	var gotINM, gotKey, gotHWID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotINM = r.Header.Get("If-None-Match")
		gotKey = r.Header.Get("X-Api-Key")
		gotHWID = r.Header.Get("X-EMLy-HWID")
		w.Header().Set("ETag", `"v42"`)
		if gotINM == `"v42"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"schemaVersion":1,"revision":42}`))
	}))
	defer srv.Close()

	s := NewHTTPSource(srv.URL)
	s.APIKey = "key-1"
	s.HWID = "HW-1"

	resp, err := s.FetchConfig(context.Background(), srv.URL, "", time.Second*5, 1<<20)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if resp.Status != http.StatusOK || string(resp.Body) != `{"schemaVersion":1,"revision":42}` {
		t.Fatalf("first fetch = %d %q", resp.Status, resp.Body)
	}
	if resp.ETag != `"v42"` {
		t.Errorf("ETag = %q", resp.ETag)
	}
	if gotINM != "" {
		t.Errorf("If-None-Match sent on the first fetch: %q", gotINM)
	}
	// The identification headers every other updater request carries must
	// go out here too: the server keys its rollout tracking off them.
	if gotKey != "key-1" || gotHWID != "HW-1" {
		t.Errorf("identity headers = %q / %q", gotKey, gotHWID)
	}

	resp, err = s.FetchConfig(context.Background(), srv.URL, resp.ETag, time.Second*5, 1<<20)
	if err != nil {
		t.Fatalf("conditional fetch: %v", err)
	}
	if resp.Status != http.StatusNotModified {
		t.Errorf("conditional fetch = %d, want 304", resp.Status)
	}
	if len(resp.Body) != 0 {
		t.Errorf("304 carried a body: %q", resp.Body)
	}
	if gotINM != `"v42"` {
		t.Errorf("If-None-Match = %q, want the cached ETag", gotINM)
	}
}

// 204 means "reachable, nothing published": not an error, so the caller
// keeps its cache and logs no outage.
func TestFetchConfigNoContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	resp, err := NewHTTPSource(srv.URL).FetchConfig(context.Background(), srv.URL, "", time.Second*5, 1<<20)
	if err != nil {
		t.Fatalf("204 must not be an error: %v", err)
	}
	if resp.Status != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.Status)
	}
}

// A 404 is reported as ErrNotFound so a mirror that does not implement the
// route yet is distinguishable from a broken one.
func TestFetchConfigNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, err := NewHTTPSource(srv.URL).FetchConfig(context.Background(), srv.URL, "", time.Second*5, 1<<20)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// The size cap is enforced on the wire, before anything tries to parse a
// document the validator would refuse anyway.
func TestFetchConfigSizeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, 4096))
	}))
	defer srv.Close()

	_, err := NewHTTPSource(srv.URL).FetchConfig(context.Background(), srv.URL, "", time.Second*5, 1024)
	if !errors.Is(err, ErrConfigTooLarge) {
		t.Fatalf("err = %v, want ErrConfigTooLarge", err)
	}
}

// A 5xx is an error so the caller moves on to the next candidate server.
func TestFetchConfigServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	if _, err := NewHTTPSource(srv.URL).FetchConfig(context.Background(), srv.URL, "", time.Second*5, 1<<20); err == nil {
		t.Fatal("expected an error for HTTP 502")
	}
}
