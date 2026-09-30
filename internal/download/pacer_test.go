package download

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"emlyupdater/internal/manifest"
	"emlyupdater/internal/source"
)

// refusingSource answers the first refusals fetches with 429 and then serves
// payload.
type refusingSource struct {
	payload  []byte
	refusals int
	refusal  source.RetryLaterError
	fetches  int
}

func (f *refusingSource) Name() string { return "refusing" }
func (f *refusingSource) FetchManifest(context.Context) (*manifest.Manifest, error) {
	return nil, errors.New("not used")
}
func (f *refusingSource) ResolveTarget(*manifest.Manifest, string) (manifest.Target, error) {
	return manifest.Target{}, errors.New("not used")
}
func (f *refusingSource) FetchSetup(_ context.Context, _ manifest.Target, destPath string) error {
	f.fetches++
	if f.fetches <= f.refusals {
		e := f.refusal
		return &e
	}
	return os.WriteFile(destPath, f.payload, 0644)
}

// fakeClock drives a Manager's seams: sleeping advances the clock, and every
// wait is recorded.
type fakeClock struct {
	now   time.Time
	waits []time.Duration
}

func (c *fakeClock) install(m *Manager, jitter time.Duration) {
	m.now = func() time.Time { return c.now }
	m.sleep = func(_ context.Context, d time.Duration) error {
		c.waits = append(c.waits, d)
		c.now = c.now.Add(d)
		return nil
	}
	m.jitter = func(time.Duration) time.Duration { return jitter }
}

var queueFull = source.RetryLaterError{Wait: 60 * time.Second, QueueFull: true, Capacity: 50, Active: 50}

func pacedManager(t *testing.T, p *Pacer) (*Manager, *fakeClock) {
	t.Helper()
	m := &Manager{Dir: t.TempDir(), Pacer: p}
	c := &fakeClock{now: time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)}
	c.install(m, 7*time.Second)
	return m, c
}

func TestEnsureGivesUpAfterMaxRetryLater(t *testing.T) {
	payload := []byte("setup")
	src := &refusingSource{payload: payload, refusals: 100, refusal: queueFull}
	m, clock := pacedManager(t, &Pacer{})

	_, err := m.Ensure(context.Background(), src, manifest.Target{Version: "1.7.5", SHA256: sha(payload)})
	if !IsQueueFull(err) {
		t.Fatalf("expected a queue-full error, got %v", err)
	}
	if src.fetches != MaxRetryLater {
		t.Fatalf("fetches = %d, want %d", src.fetches, MaxRetryLater)
	}
	// One wait between each pair of requests, none after the last, and every
	// one Retry-After plus jitter - never an immediate retry.
	if len(clock.waits) != MaxRetryLater-1 {
		t.Fatalf("waits = %v, want %d of them", clock.waits, MaxRetryLater-1)
	}
	for _, w := range clock.waits {
		if w != 67*time.Second {
			t.Fatalf("wait %s, want Retry-After 60s + 7s jitter", w)
		}
	}
}

func TestEnsureRetriesThenSucceeds(t *testing.T) {
	payload := []byte("setup")
	src := &refusingSource{payload: payload, refusals: 2, refusal: queueFull}
	m, clock := pacedManager(t, &Pacer{})

	path, err := m.Ensure(context.Background(), src, manifest.Target{Version: "1.7.5", SHA256: sha(payload)})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyFile(path, sha(payload)); err != nil {
		t.Fatal(err)
	}
	if src.fetches != 3 || len(clock.waits) != 2 {
		t.Fatalf("fetches = %d, waits = %v", src.fetches, clock.waits)
	}
}

// A wait the server imposed carries over to the next call - the next cycle,
// or a notify's early wake - which must not ask before it has passed.
func TestEnsureHonoursPacerAcrossCalls(t *testing.T) {
	payload := []byte("setup")
	src := &refusingSource{payload: payload, refusals: 1, refusal: source.RetryLaterError{Wait: time.Hour, QueueFull: true}}
	m, clock := pacedManager(t, &Pacer{})
	target := manifest.Target{Version: "1.7.5", SHA256: sha(payload)}

	// A wait beyond MaxInCycleWait ends the call at once, without sleeping.
	if _, err := m.Ensure(context.Background(), src, target); !IsQueueFull(err) {
		t.Fatalf("expected a queue-full error, got %v", err)
	}
	if src.fetches != 1 || len(clock.waits) != 0 {
		t.Fatalf("fetches = %d, waits = %v", src.fetches, clock.waits)
	}

	// 15 minutes later - one poll cycle - the hour has not passed: no request.
	clock.now = clock.now.Add(15 * time.Minute)
	_, err := m.Ensure(context.Background(), src, target)
	if !IsQueueFull(err) {
		t.Fatalf("expected the deferral to report queue full, got %v", err)
	}
	if src.fetches != 1 {
		t.Fatalf("asked the server before Retry-After passed: fetches = %d", src.fetches)
	}

	// Once it has, the next call fetches again.
	clock.now = clock.now.Add(time.Hour)
	if _, err := m.Ensure(context.Background(), src, target); err != nil {
		t.Fatal(err)
	}
	if src.fetches != 2 {
		t.Fatalf("fetches = %d, want 2", src.fetches)
	}
}

// EMLy's setups and the updater's own share one pool of slots on the server,
// so a refusal on one holds the other back too.
func TestSharedPacerHoldsBackTheOtherManager(t *testing.T) {
	payload := []byte("setup")
	p := &Pacer{}
	self, clock := pacedManager(t, p)
	emly := &Manager{Dir: t.TempDir(), Pacer: p}
	clock.install(emly, 7*time.Second)

	selfSrc := &refusingSource{payload: payload, refusals: 1, refusal: source.RetryLaterError{Wait: time.Hour, QueueFull: true}}
	if _, err := self.Ensure(context.Background(), selfSrc, manifest.Target{Version: "1.5.0", SHA256: sha(payload)}); err == nil {
		t.Fatal("expected the self-update download to be deferred")
	}

	emlySrc := &refusingSource{payload: payload}
	if _, err := emly.Ensure(context.Background(), emlySrc, manifest.Target{Version: "1.7.5", SHA256: sha(payload)}); !IsQueueFull(err) {
		t.Fatalf("expected the EMLy download to be deferred, got %v", err)
	}
	if emlySrc.fetches != 0 {
		t.Fatalf("EMLy download asked the server right after the refusal: fetches = %d", emlySrc.fetches)
	}
}

// A 429 from a rate limiter is waited out the same way, but it is not a full
// queue: the caller treats it as a failure once the attempts run out.
func TestEnsureRateLimiter429IsNotQueueFull(t *testing.T) {
	payload := []byte("setup")
	src := &refusingSource{payload: payload, refusals: 100, refusal: source.RetryLaterError{Wait: source.DefaultRetryAfter}}
	m, _ := pacedManager(t, &Pacer{})

	_, err := m.Ensure(context.Background(), src, manifest.Target{Version: "1.7.5", SHA256: sha(payload)})
	var rle *source.RetryLaterError
	if !errors.As(err, &rle) || IsQueueFull(err) {
		t.Fatalf("expected a non-queue 429, got %v", err)
	}
	if src.fetches != MaxRetryLater {
		t.Fatalf("fetches = %d, want %d", src.fetches, MaxRetryLater)
	}
}

func TestEnsureStopsWaitingOnCancel(t *testing.T) {
	payload := []byte("setup")
	src := &refusingSource{payload: payload, refusals: 100, refusal: queueFull}
	m, _ := pacedManager(t, &Pacer{})
	ctx, cancel := context.WithCancel(context.Background())
	m.sleep = nil // the real, context-aware timer
	cancel()

	_, err := m.Ensure(ctx, src, manifest.Target{Version: "1.7.5", SHA256: sha(payload)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if src.fetches != 1 {
		t.Fatalf("fetches = %d, want 1", src.fetches)
	}
}

// Any other failure keeps its old behaviour: no retry inside the call.
func TestEnsureDoesNotRetryOtherErrors(t *testing.T) {
	src := &fakeSource{fail: true}
	m, clock := pacedManager(t, &Pacer{})

	if _, err := m.Ensure(context.Background(), src, manifest.Target{Version: "1.7.5", SHA256: "ab"}); err == nil {
		t.Fatal("expected an error")
	}
	if src.fetches != 1 || len(clock.waits) != 0 {
		t.Fatalf("fetches = %d, waits = %v", src.fetches, clock.waits)
	}
}
