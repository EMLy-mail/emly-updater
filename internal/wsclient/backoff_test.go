package wsclient

import (
	"testing"
	"time"
)

// assertInRange fails unless got is in [0, ceiling] - the full-jitter
// contract: Next never returns more than the deterministic ceiling, but it
// is not required to return the ceiling itself.
func assertInRange(t *testing.T, got, ceiling time.Duration) {
	t.Helper()
	if got < 0 || got > ceiling {
		t.Errorf("Next() = %v, want in [0, %v]", got, ceiling)
	}
}

// The zero value is usable: a caller that sets nothing gets the shipped
// schedule rather than a busy loop on a zero delay.
func TestBackoffZeroValueUsesDefaults(t *testing.T) {
	var b Backoff
	got := b.Next()
	assertInRange(t, got, DefaultBackoffBase)
	if b.ceiling() != DefaultBackoffBase {
		t.Errorf("ceiling after first Next() = %v, want %v", b.ceiling(), DefaultBackoffBase)
	}
}

// The ceiling doubles on every call and caps at Max. Jitter randomises what
// Next returns, but the ceiling it draws from stays exactly this sequence.
func TestBackoffCeilingDoublesAndCaps(t *testing.T) {
	b := Backoff{Base: time.Second, Max: 8 * time.Second}
	wantCeilings := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		8 * time.Second, // capped, not 16
		8 * time.Second,
	}
	for i, want := range wantCeilings {
		got := b.Next()
		assertInRange(t, got, want)
		if b.ceiling() != want {
			t.Errorf("ceiling() #%d = %v, want %v", i+1, b.ceiling(), want)
		}
	}
}

func TestBackoffResetReturnsCeilingToBase(t *testing.T) {
	b := Backoff{Base: time.Second, Max: time.Minute}
	b.Next()
	b.Next()
	b.Reset()
	got := b.Next()
	assertInRange(t, got, time.Second)
	if b.ceiling() != time.Second {
		t.Errorf("ceiling() after Reset() = %v, want %v", b.ceiling(), time.Second)
	}
}

// A connection that stayed up long enough to be called healthy earns a fresh
// schedule; one that died on arrival does not. Without this a machine that
// flapped once keeps retrying at the cap for the rest of the day.
func TestBackoffSettleOnlyResetsAfterAStableConnection(t *testing.T) {
	b := Backoff{Base: time.Second, Max: time.Minute, StableFor: 30 * time.Second}
	b.Next() // ceiling 1s
	b.Next() // ceiling 2s

	b.Settle(5 * time.Second)
	got := b.Next() // ceiling should still grow to 4s
	assertInRange(t, got, 4*time.Second)
	if b.ceiling() != 4*time.Second {
		t.Errorf("ceiling() after a short connection = %v, want %v (schedule must keep growing)", b.ceiling(), 4*time.Second)
	}

	b.Settle(45 * time.Second)
	got = b.Next()
	assertInRange(t, got, time.Second)
	if b.ceiling() != time.Second {
		t.Errorf("ceiling() after a stable connection = %v, want %v", b.ceiling(), time.Second)
	}
}

// Exactly at the threshold counts as stable: the boundary belongs to the
// healthy side, so a StableFor the caller sets to "one poll interval" is not
// missed by a nanosecond.
func TestBackoffSettleAtTheThresholdResets(t *testing.T) {
	b := Backoff{Base: time.Second, Max: time.Minute, StableFor: 30 * time.Second}
	b.Next()
	b.Settle(30 * time.Second)
	got := b.Next()
	assertInRange(t, got, time.Second)
	if b.ceiling() != time.Second {
		t.Errorf("ceiling() after exactly StableFor = %v, want %v", b.ceiling(), time.Second)
	}
}

// Next never returns a value above its ceiling, exercised over many draws so
// a jitter implementation that occasionally exceeds the bound (an off-by-one
// in the random range) would be caught rather than passing by luck.
func TestBackoffNextStaysWithinCeilingAcrossManyDraws(t *testing.T) {
	b := Backoff{Base: 100 * time.Millisecond, Max: time.Second}
	for i := 0; i < 500; i++ {
		got := b.Next()
		if got < 0 || got > b.ceiling() {
			t.Fatalf("draw #%d: Next() = %v, ceiling() = %v", i, got, b.ceiling())
		}
	}
}
