package download

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"emlyupdater/internal/manifest"
	"emlyupdater/internal/source"
)

// The server caps concurrent installer downloads with one pool of slots
// shared by EMLy and the updater, and refuses the rest with HTTP 429 plus a
// Retry-After (emly-go-api, internal/downloadqueue). The constants below are
// this client's side of that contract.
const (
	// MaxRetryLater is how many consecutive 429s one Ensure call takes
	// before giving up until the next poll cycle.
	MaxRetryLater = 5
	// RetryLaterJitter is the most added on top of Retry-After. Without it,
	// every machine refused in the same second comes back in the same second
	// and fills the queue again.
	RetryLaterJitter = 30 * time.Second
	// MaxInCycleWait is the longest wait fetch sleeps through inside a
	// cycle. Retry-After can be set as high as a day on the server; waiting
	// that out would stall the whole poll loop, so a longer wait ends the
	// cycle instead and the Pacer keeps later cycles from asking early.
	MaxInCycleWait = 5 * time.Minute
)

// Logger is the subset of logging.Logger a Manager writes to.
type Logger interface {
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
}

// Pacer remembers, in memory only, the earliest moment the server may be
// asked for a setup again after a 429. A restart forgets it - the server
// simply answers 429 again if it is still full.
type Pacer struct {
	mu        sync.Mutex
	notBefore time.Time
	last      *source.RetryLaterError
}

// hold records a refusal and the wait (jitter included) it imposes.
func (p *Pacer) hold(until time.Time, e *source.RetryLaterError) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if until.After(p.notBefore) {
		p.notBefore = until
	}
	p.last = e
}

// deferred returns the refusal still in force at now, or nil.
func (p *Pacer) deferred(now time.Time) *source.RetryLaterError {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !now.Before(p.notBefore) {
		return nil
	}
	e := source.RetryLaterError{Wait: p.notBefore.Sub(now)}
	if p.last != nil {
		e.QueueFull, e.Message = p.last.QueueFull, p.last.Message
	}
	return &e
}

// IsQueueFull reports whether err is the server's download queue refusing
// with "download queue full" - a "come back later" to log at info level and
// never count as a failure. A 429 from a rate limiter is not one.
func IsQueueFull(err error) bool {
	var rle *source.RetryLaterError
	return errors.As(err, &rle) && rle.QueueFull
}

func (m *Manager) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *Manager) wait(ctx context.Context, d time.Duration) error {
	if m.sleep != nil {
		return m.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) randJitter(max time.Duration) time.Duration {
	if m.jitter != nil {
		return m.jitter(max)
	}
	return rand.N(max + 1)
}

func (m *Manager) logf(queueFull bool, msg string, kv ...any) {
	if m.Log == nil {
		return
	}
	// A full queue is the server working as intended, not a failure;
	// any other 429 (a rate limiter) is worth a warning.
	if queueFull {
		m.Log.Info(msg, kv...)
	} else {
		m.Log.Warn(msg, kv...)
	}
}

// fetch downloads t into partial, honouring the server's 429s: never ask
// again before Retry-After (plus jitter) has passed, at most MaxRetryLater
// requests per call. A refusal is returned as (a wrap of) a
// *source.RetryLaterError, so the caller can tell "come back later" apart
// from a real failure. Any other error is returned at once - it is retried by
// the next cycle, exactly as before.
func (m *Manager) fetch(ctx context.Context, src source.Source, t manifest.Target, partial string) error {
	if m.Pacer == nil {
		return src.FetchSetup(ctx, t, partial)
	}
	if e := m.Pacer.deferred(m.clock()); e != nil {
		m.logf(e.QueueFull, "setup download deferred: the server asked to wait",
			"target", t.Version, "remaining", e.Wait.Round(time.Second).String())
		return fmt.Errorf("download deferred: %w", e)
	}

	for attempt := 1; ; attempt++ {
		err := src.FetchSetup(ctx, t, partial)
		var rle *source.RetryLaterError
		if !errors.As(err, &rle) {
			return err
		}

		wait := rle.Wait + m.randJitter(RetryLaterJitter)
		m.Pacer.hold(m.clock().Add(wait), rle)
		kv := []any{"target", t.Version, "attempt", attempt, "maxAttempts", MaxRetryLater,
			"retryAfter", rle.Wait.String(), "wait", wait.Round(time.Second).String()}
		if rle.QueueFull {
			kv = append(kv, "capacity", rle.Capacity, "active", rle.Active)
		}

		if attempt >= MaxRetryLater || wait > MaxInCycleWait {
			m.logf(rle.QueueFull, "setup download refused with 429, retrying next cycle", kv...)
			return err
		}
		m.logf(rle.QueueFull, "setup download refused with 429, waiting before retrying", kv...)
		if err := m.wait(ctx, wait); err != nil {
			return err
		}
	}
}
