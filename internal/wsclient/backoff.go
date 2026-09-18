package wsclient

import "time"

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
// No jitter: at this fleet size (~350-400 machines) a synchronised retry is
// not a thundering herd against an expensive endpoint, it is a handful of
// Accept calls on a route that runs no query. Jitter would buy nothing and
// make the schedule untestable without a clock seam.
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

// Next returns how long to wait before the next connection attempt and
// advances the schedule.
func (b *Backoff) Next() time.Duration {
	if b.cur <= 0 {
		b.cur = b.base()
	} else {
		b.cur *= 2
	}
	if m := b.max(); b.cur > m {
		b.cur = m
	}
	return b.cur
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
