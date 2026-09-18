package wsclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestURLFor(t *testing.T) {
	cases := []struct {
		base string
		want string
	}{
		{"https://api.emly.ffois.it", "wss://api.emly.ffois.it/v2/client/ws"},
		{"http://172.16.96.73:8080", "ws://172.16.96.73:8080/v2/client/ws"},
		{"https://mirror.example.test/emly", "wss://mirror.example.test/emly/v2/client/ws"},
	}
	for _, c := range cases {
		got, err := URLFor(c.base)
		if err != nil {
			t.Errorf("URLFor(%q) returned %v", c.base, err)
			continue
		}
		if got != c.want {
			t.Errorf("URLFor(%q) = %q, want %q", c.base, got, c.want)
		}
	}
}

func TestURLForRejectsWhatItCannotSpeak(t *testing.T) {
	for _, base := range []string{"", "ftp://example.test", "://nonsense"} {
		if got, err := URLFor(base); err == nil {
			t.Errorf("URLFor(%q) = %q, want an error", base, got)
		}
	}
}

// Every field of the identity payload is optional in exactly the way the
// matching X-EMLy-* header is today: absent means "not reported", never
// "empty". The API reads a missing key as unknown and keeps what it has,
// while an explicit empty string would erase it.
func TestIdentityOmitsWhatItDoesNotKnow(t *testing.T) {
	raw, err := json.Marshal(Identity{Hostname: "RM095", HWID: "HW-1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, `"hostname":"RM095"`) || !strings.Contains(got, `"hwid":"HW-1"`) {
		t.Errorf("payload dropped a field it was given: %s", got)
	}
	for _, key := range []string{"ad_domain", "logged_user", "logged_user_state",
		"logged_user_disconnected_at", "serial", "product", "os_version", "emly_version"} {
		if strings.Contains(got, key) {
			t.Errorf("payload carries %q for a value it does not have: %s", key, got)
		}
	}
}

func TestIdentityIdentified(t *testing.T) {
	if (Identity{}).Identified() {
		t.Error("an empty identity reports itself as identified")
	}
	if !(Identity{HWID: "HW-1"}).Identified() {
		t.Error("an identity with only a HWID is not identified")
	}
	if !(Identity{Hostname: "RM095"}).Identified() {
		t.Error("an identity with only a hostname is not identified")
	}
}

// A 404 on the upgrade is the documented answer from a site mirror that has
// not been updated yet, exactly like the updater's own manifest endpoint. It
// has to come back distinguishable, because the supervisor stops asking that
// server rather than retrying it forever.
func TestDialReportsAnUnimplementedEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	url, err := URLFor(srv.URL)
	if err != nil {
		t.Fatalf("URLFor: %v", err)
	}
	c := &Client{URL: url, HandshakeTimeout: 5 * time.Second}
	if _, err := c.dial(context.Background()); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("dial against a 404 returned %v, want ErrNotImplemented", err)
	}
}

func TestDialReportsARejectedKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	url, err := URLFor(srv.URL)
	if err != nil {
		t.Fatalf("URLFor: %v", err)
	}
	c := &Client{URL: url, HandshakeTimeout: 5 * time.Second}
	if _, err := c.dial(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("dial against a 401 returned %v, want ErrUnauthorized", err)
	}
}

// The API key and the User-Agent go out on the upgrade request: the key is
// what the route authenticates on, and the User-Agent is where the API reads
// updater_version and contact from - they are deliberately not in the
// identity payload.
func TestDialSendsTheApiKeyAndUserAgent(t *testing.T) {
	seen := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		http.NotFound(w, r)
	}))
	defer srv.Close()

	url, err := URLFor(srv.URL)
	if err != nil {
		t.Fatalf("URLFor: %v", err)
	}
	c := &Client{
		URL:              url,
		APIKey:           "secret-key",
		UserAgent:        "EMLy-Updater/1.6.3 (it@example.test)",
		HandshakeTimeout: 5 * time.Second,
	}
	_, _ = c.dial(context.Background())

	select {
	case h := <-seen:
		if got := h.Get("X-Api-Key"); got != "secret-key" {
			t.Errorf("X-Api-Key = %q, want %q", got, "secret-key")
		}
		if got := h.Get("User-Agent"); got != "EMLy-Updater/1.6.3 (it@example.test)" {
			t.Errorf("User-Agent = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server never saw the upgrade request")
	}
}
