package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Every identity header the server keys its fleet inventory off must go out
// on every request shape - the manifest fetch, the config fetch and the
// setup download all run through applyHeaders, and a header wired into only
// one of them leaves the API with a partial picture of the machine.
func TestApplyHeadersCarriesIdentity(t *testing.T) {
	want := map[string]string{
		"X-Api-Key":         "key-1",
		"X-EMLy-Hostname":   "PC-01",
		"X-EMLy-HWID":       "36CC511A-F0DE-EA11-8106-842AFDCE34D0",
		"X-EMLy-ADDomain":   "contoso.local",
		"X-EMLy-IntIP":      "10.0.0.5",
		"X-EMLy-Serial":     "CND0342SLW",
		"X-EMLy-Product":    "1F3N0EA#ABZ",
		"X-EMLy-LoggedUser": `CONTOSO\mario.rossi`,
	}

	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"schemaVersion":1,"revision":1}`))
	}))
	defer srv.Close()

	s := NewHTTPSource(srv.URL)
	s.APIKey = want["X-Api-Key"]
	s.Hostname = want["X-EMLy-Hostname"]
	s.HWID = want["X-EMLy-HWID"]
	s.ADDomain = want["X-EMLy-ADDomain"]
	s.InternalIP = want["X-EMLy-IntIP"]
	s.Serial = want["X-EMLy-Serial"]
	s.Product = want["X-EMLy-Product"]
	s.LoggedUser = want["X-EMLy-LoggedUser"]

	if _, err := s.FetchConfig(context.Background(), srv.URL, "", 5*time.Second, 1<<20); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	for k, v := range want {
		if got.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, got.Get(k), v)
		}
	}
}

// An unset field sends no header at all rather than an empty one: nobody
// logged on, or firmware with no usable serial, must be indistinguishable
// from a client too old to report it - the API treats a missing header as
// "unknown" and leaves the stored value alone, which an empty string would
// instead overwrite.
func TestApplyHeadersOmitsEmpty(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"schemaVersion":1,"revision":1}`))
	}))
	defer srv.Close()

	s := NewHTTPSource(srv.URL)
	s.HWID = "HW-1"

	if _, err := s.FetchConfig(context.Background(), srv.URL, "", 5*time.Second, 1<<20); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	for _, h := range []string{"X-EMLy-LoggedUser", "X-EMLy-Serial", "X-EMLy-Product", "X-EMLy-Hostname"} {
		if _, present := got[http.CanonicalHeaderKey(h)]; present {
			t.Errorf("%s was sent despite being unset", h)
		}
	}
}
