package policy

import (
	"sync/atomic"
	"time"
)

// Source says where the snapshot's document came from.
type Source int

const (
	// SourceDefault is the legacy-derived policy: no document has ever been
	// accepted on this machine (or the cache was unusable).
	SourceDefault Source = iota
	// SourceCache is the last-known-good document read from disk at startup
	// and not yet confirmed by the server this run.
	SourceCache
	// SourceRemote is a document fetched (or revalidated with a 304) during
	// this run.
	SourceRemote
)

func (s Source) String() string {
	switch s {
	case SourceCache:
		return "cache"
	case SourceRemote:
		return "remote"
	default:
		return "default"
	}
}

// Snapshot is one immutable policy state. The service swaps whole snapshots
// so a poll cycle that took one at its start sees the same policy until its
// end.
type Snapshot struct {
	Parsed      *Parsed
	Source      Source
	FetchedAt   time.Time
	FetchedFrom string
	ETag        string
}

// Revision is the document's revision, 0 for the default policy.
func (s *Snapshot) Revision() int64 { return s.Parsed.Global.Revision }

// GeneratedAt is the document's own timestamp, "" for the default policy.
func (s *Snapshot) GeneratedAt() string { return s.Parsed.Global.GeneratedAt }

// Stale reports whether the snapshot is older than the document's own
// refresh.staleAfterDays, by the local clock. The default policy is never
// stale, and staleAfterDays = 0 disables the check.
func (s *Snapshot) Stale(now time.Time) bool {
	if s.Source == SourceDefault || s.FetchedAt.IsZero() {
		return false
	}
	after := s.Parsed.Global.Refresh.StaleAfter()
	if after <= 0 {
		return false
	}
	return now.Sub(s.FetchedAt) > after
}

// Store holds the current snapshot behind an atomic pointer: written by the
// poll loop, read by the IPC server on its own goroutines.
type Store struct {
	cur atomic.Pointer[Snapshot]
}

// NewStore starts with initial as the current snapshot.
func NewStore(initial *Snapshot) *Store {
	s := &Store{}
	s.cur.Store(initial)
	return s
}

// Current returns the current snapshot. Never nil once NewStore has run.
func (s *Store) Current() *Snapshot { return s.cur.Load() }

// Set replaces the current snapshot.
func (s *Store) Set(snap *Snapshot) { s.cur.Store(snap) }
