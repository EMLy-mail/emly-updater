package source

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const updaterManifestBody = `{
  "version": "1.5.0",
  "download": "https://api.example/v2/updates/download/updater/1.5.0",
  "sha256": "abcd"
}`

func TestFetchUpdaterManifest(t *testing.T) {
	var gotPath, gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("X-Api-Key")
		w.Write([]byte(updaterManifestBody))
	}))
	defer srv.Close()

	s := NewHTTPSource(srv.URL + "/v2/updates/manifest")
	s.APIKey = "secret"

	m, err := s.FetchUpdaterManifest(context.Background(), srv.URL+"/v2/updates/manifest/updater")
	if err != nil {
		t.Fatalf("FetchUpdaterManifest failed: %v", err)
	}
	if !m.Offered() || m.Version != "1.5.0" {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	if gotPath != "/v2/updates/manifest/updater" {
		t.Errorf("requested path = %q", gotPath)
	}
	// The identity headers the API uses to stage a rollout must travel on this
	// request too, not just on EMLy's manifest fetch.
	if gotAPIKey != "secret" {
		t.Errorf("X-Api-Key = %q, want it to be sent", gotAPIKey)
	}
}

// A mirror that has not been updated yet answers 404. That is an answer, not a
// failure, and the caller must be able to tell it apart.
func TestFetchUpdaterManifestNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, err := NewHTTPSource(srv.URL).FetchUpdaterManifest(context.Background(), srv.URL+"/updater")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want it to wrap ErrNotFound", err)
	}
}

// Backing off and asking an endpoint that returned 404 the same question again
// cannot change the answer: the retries must be skipped.
func TestResolveUpdaterSkipsRetriesOn404(t *testing.T) {
	var primaryCalls int
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls++
		http.NotFound(w, r)
	}))
	defer primary.Close()

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(updaterManifestBody))
	}))
	defer fallback.Close()

	r := &Resolver{
		Primary:     NewHTTPSource(primary.URL + "/manifest"),
		Fallback:    NewHTTPSource(fallback.URL + "/manifest"),
		Attempts:    3,
		BaseBackoff: time.Millisecond,
	}

	src, m, err := ResolveUpdater(context.Background(), r, updaterURLOf)
	if err != nil {
		t.Fatalf("ResolveUpdater failed: %v", err)
	}
	if primaryCalls != 1 {
		t.Errorf("primary was asked %d times, want 1 (a 404 must not be retried)", primaryCalls)
	}
	if m.Version != "1.5.0" {
		t.Errorf("unexpected manifest: %+v", m)
	}
	// The setup must be fetched from whichever source actually answered.
	if src.Name() != r.Fallback.Name() {
		t.Errorf("resolved source = %s, want the fallback", src.Name())
	}
}

// Nobody serving the endpoint is a silent no-op for the caller, so the
// ErrNotFound has to survive all the way out.
func TestResolveUpdaterReportsNotFoundWhenNoSourceServesIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	r := &Resolver{
		Primary:     NewHTTPSource(srv.URL + "/manifest"),
		Fallback:    NewHTTPSource(srv.URL + "/manifest"),
		Attempts:    2,
		BaseBackoff: time.Millisecond,
	}

	if _, _, err := ResolveUpdater(context.Background(), r, updaterURLOf); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want it to wrap ErrNotFound", err)
	}
}

// A primary that is simply unreachable keeps its retries.
func TestResolveUpdaterRetriesRealFailures(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := &Resolver{
		Primary:     NewHTTPSource(srv.URL + "/manifest"),
		Attempts:    3,
		BaseBackoff: time.Millisecond,
	}

	if _, _, err := ResolveUpdater(context.Background(), r, updaterURLOf); err == nil {
		t.Fatal("expected an error when the endpoint keeps failing")
	}
	if calls != 3 {
		t.Errorf("endpoint was asked %d times, want 3", calls)
	}
}

// updaterURLOf mirrors what config.UpdaterManifestURL does, without pulling
// the config package into these tests.
func updaterURLOf(s Source) (string, error) {
	return s.(*HTTPSource).ManifestURL + "/updater", nil
}
