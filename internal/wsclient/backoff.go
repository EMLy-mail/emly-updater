package wsclient

import (
	"math/rand/v2"
	"time"
)

// The shipped reconnection schedule. Deliberately conservative at the top
// end: a machine that cannot reach its server has nothing to gain from
// asking every 30 seconds for eight hours, and ~400 machines doing it is a
// load the endpoint should never have to absorb for free.
const (
	DefaultBackoffBase = 5 * time.Second
	DefaultBackoffMax  = 5 * time.Minute
	// DefaultStableFor is how long a connection must survive before the
	// schedule is considered to have proven itself and goes back to Base.
	DefaultStableFor = 60 * time.Second
)

// Backoff is the presence channel's reconnection schedule: exponential from
// Base, doubling on every consecutive attempt, capped at Max, and reset once
// a connection has stayed up for StableFor.
//
// The reset-on-stable rule is the whole point. Without it a machine whose
// connection blipped once - a Wi-Fi roam, a firewall state table expiring -
// would spend the rest of the day reconnecting at the cap, because nothing
// would ever tell the schedule the problem was over. With it, a connection
// that lived long enough to be called healthy earns a fresh schedule and one
// that died immediately does not.
//
// Full jitter: Next returns a uniformly random value in [0, ceiling] rather
// than the ceiling itself, because these reconnects are phase-locked in a way
// a manifest poll never is. Every machine builds the same schedule from the
// same inputs (the API restarted; the server rejected the upgrade), so
// without jitter every machine attached to one server retries at the exact
// same instant, in lockstep, forever. And every office not listed in
// dcLookupMap NATs to a single public address, so that instant is N
// simultaneous requests from one IP - the API's authenticated rate-limit
// tier is 100 requests/minute per IP, and tripping it 20 times in a window
// bans the IP for 5 minutes. An API deploy would otherwise ban every such
// office's shared address, taking manifest polls, self-update checks and
// EMLy's own bug-report uploads down with the presence reconnects that
// caused it. The ceiling itself (cur) still advances deterministically, so
// the schedule stays testable via that ceiling even though the value handed
// to the caller is randomised.
//
// Not safe for concurrent use - the supervisor goroutine is its only caller.
type Backoff struct {
	// Base is the first delay, and the one Reset returns to. Zero means
	// DefaultBackoffBase.
	Base time.Duration
	// Max caps the delay. Zero means DefaultBackoffMax.
	Max time.Duration
	// StableFor is how long a connection must have been up for Settle to
	// reset the schedule. Zero means DefaultStableFor.
	StableFor time.Duration

	// cur is the delay the last Next returned; zero means "not started".
	cur time.Duration
}

func (b *Backoff) base() time.Duration {
	if b.Base > 0 {
		return b.Base
	}
	return DefaultBackoffBase
}

func (b *Backoff) max() time.Duration {
	if b.Max > 0 {
		return b.Max
	}
	return DefaultBackoffMax
}

func (b *Backoff) stableFor() time.Duration {
	if b.StableFor > 0 {
		return b.StableFor
	}
	return DefaultStableFor
}

// Next advances the schedule's ceiling and returns how long to wait before
// the next connection attempt: a uniformly random duration in [0, ceiling],
// full jitter, so a fleet of machines sharing the same schedule does not
// reconnect in lockstep (see the doc comment above).
func (b *Backoff) Next() time.Duration {
	if b.cur <= 0 {
		b.cur = b.base()
	} else {
		b.cur *= 2
	}
	if m := b.max(); b.cur > m {
		b.cur = m
	}
	return jitter(b.cur)
}

// ceiling exposes the deterministic part of the schedule - the value Next
// would have returned with no jitter applied - so tests can assert on it
// without making Next's actual return value predictable.
func (b *Backoff) ceiling() time.Duration { return b.cur }

// jitter returns a uniformly random duration in [0, d], guarding against a
// zero or negative ceiling (rand.N panics on a non-positive bound).
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d) + 1))
}

// Reset returns the schedule to its start, so the next Next yields Base.
func (b *Backoff) Reset() { b.cur = 0 }

// Settle resets the schedule when a connection that has just ended lasted at
// least StableFor. A shorter one leaves the schedule where it was, so a
// connection that keeps dying on arrival keeps backing off.
func (b *Backoff) Settle(connectedFor time.Duration) {
	if connectedFor >= b.stableFor() {
		b.Reset()
	}
}
