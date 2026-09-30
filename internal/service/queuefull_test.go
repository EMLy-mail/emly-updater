package service

// The server's installer download queue refuses with HTTP 429 when its slots
// are all taken (emly-go-api, internal/downloadqueue). That is the server
// pacing the fleet over the MPLS, not a failure: neither the EMLy cycle nor
// the self-update may report it as one. download.Manager's own tests cover the
// waiting; these cover what the service does once it gives up for the cycle.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"emlyupdater/internal/config"
	"emlyupdater/internal/download"
	"emlyupdater/internal/manifest"
	"emlyupdater/internal/policy"
	"emlyupdater/internal/source"
	"emlyupdater/internal/state"
)

// queueFullSource refuses every setup download the way the queue does.
type queueFullSource struct{ queueFull bool }

func (queueFullSource) Name() string { return "queue-full-source" }
func (queueFullSource) FetchManifest(context.Context) (*manifest.Manifest, error) {
	return nil, nil
}
func (queueFullSource) ResolveTarget(*manifest.Manifest, string) (manifest.Target, error) {
	return manifest.Target{}, nil
}
func (s queueFullSource) FetchSetup(context.Context, manifest.Target, string) error {
	return &source.RetryLaterError{Wait: time.Minute, QueueFull: s.queueFull}
}

func TestApplySelfUpdateQueueFullIsNotAFailure(t *testing.T) {
	u := newClientTestUpdater(t)
	u.SelfDownloads = &download.Manager{Dir: t.TempDir()} // no Pacer: the 429 comes straight back
	var names []string
	u.emitFn = func(name string, _ any) { names = append(names, name) }

	m := &manifest.UpdaterManifest{Version: "9.9.9", Download: "fake://irrelevant", SHA256: "deadbeef"}
	if launched := u.applySelfUpdate(context.Background(), queueFullSource{queueFull: true}, m, 1); launched {
		t.Fatal("applySelfUpdate must report false when the download is deferred")
	}
	if len(names) != 0 {
		t.Fatalf("emitted %v for a full download queue, want nothing", names)
	}
}

// A 429 from a rate limiter is not the queue: it stays a download_failed.
func TestApplySelfUpdateRateLimited429StillFails(t *testing.T) {
	u := newClientTestUpdater(t)
	u.SelfDownloads = &download.Manager{Dir: t.TempDir()}
	var got []updateEvent
	u.emitFn = func(_ string, p any) {
		if ev, ok := p.(updateEvent); ok {
			got = append(got, ev)
		}
	}

	m := &manifest.UpdaterManifest{Version: "9.9.9", Download: "fake://irrelevant", SHA256: "deadbeef"}
	u.applySelfUpdate(context.Background(), queueFullSource{queueFull: false}, m, 1)
	if len(got) != 1 || got[0].Error == nil || got[0].Error.Code != "download_failed" {
		t.Fatalf("update.failed = %+v, want one download_failed", got)
	}
}

// cycleAgainstQueue runs one EMLy cycle against a server whose manifest offers
// a new release and whose setup endpoint answers 429 with body.
func cycleAgainstQueue(t *testing.T, body string) (int32, error) {
	t.Helper()
	var setupRequests int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/updates/manifest":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"stableVersion":"9.9.9","stableDownload":"` + srv.URL +
				`/v2/updates/releases/9.9.9/download","sha256Checksums":{"9.9.9":"deadbeef"}}`))
		case "/v2/updates/releases/9.9.9/download":
			atomic.AddInt32(&setupRequests, 1)
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(body))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	cfg := internalCfg(t, config.SourceInternal)
	cfg.InternalManifestURL = srv.URL + "/v2/updates/manifest"
	cfg.ExternalManifestURL = srv.URL + "/v2/updates/manifest"
	cfg.EMLyConfigFile = filepath.Join(t.TempDir(), "missing", "config.ini") // fresh install: anything is newer
	u := newTestUpdater(t, cfg, dcNamed("DC-RM2"), ipsOf("172.16.96.50"))
	u.Store = &state.Store{Path: filepath.Join(t.TempDir(), "state.json")}
	u.Downloads = &download.Manager{Dir: t.TempDir()} // no Pacer: one request, no waiting

	snap := u.Policy.Current()
	snap.Parsed.Global.Updater.Resolver = policy.ResolverSettings{Attempts: 1, BaseBackoffSeconds: 0}
	snap.Parsed.Global.Updater.SelfUpdate.Enabled = false

	cyc := u.beginCycle(context.Background(), true)
	err := u.Cycle(context.Background(), cyc)
	return atomic.LoadInt32(&setupRequests), err
}

func TestCycleQueueFullIsNotAFailure(t *testing.T) {
	requests, err := cycleAgainstQueue(t, `{"error":"download queue full","retry_after":60,"capacity":50,"active":50}`)
	if requests != 1 {
		t.Fatalf("setup requests = %d, want 1", requests)
	}
	if err != nil {
		t.Fatalf("a full download queue failed the cycle: %v", err)
	}
}

func TestCycleRateLimited429StillFails(t *testing.T) {
	_, err := cycleAgainstQueue(t, "")
	var rle *source.RetryLaterError
	if !errors.As(err, &rle) {
		t.Fatalf("expected the cycle to fail with the 429, got %v", err)
	}
}
