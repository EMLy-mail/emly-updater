package wsclient

import (
	"testing"
	"time"
)

// The zero value is usable: a caller that sets nothing gets the shipped
// schedule rather than a busy loop on a zero delay.
func TestBackoffZeroValueUsesDefaults(t *testing.T) {
	var b Backoff
	if got := b.Next(); got != DefaultBackoffBase {
		t.Errorf("first Next() = %v, want %v", got, DefaultBackoffBase)
	}
}

func TestBackoffDoublesAndCaps(t *testing.T) {
	b := Backoff{Base: time.Second, Max: 8 * time.Second}
	want := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		8 * time.Second, // capped, not 16
		8 * time.Second,
	}
	for i, w := range want {
		if got := b.Next(); got != w {
			t.Errorf("Next() #%d = %v, want %v", i+1, got, w)
		}
	}
}

func TestBackoffResetReturnsToBase(t *testing.T) {
	b := Backoff{Base: time.Second, Max: time.Minute}
	b.Next()
	b.Next()
	b.Reset()
	if got := b.Next(); got != time.Second {
		t.Errorf("Next() after Reset() = %v, want %v", got, time.Second)
	}
}

// A connection that stayed up long enough to be called healthy earns a fresh
// schedule; one that died on arrival does not. Without this a machine that
// flapped once keeps retrying at the cap for the rest of the day.
func TestBackoffSettleOnlyResetsAfterAStableConnection(t *testing.T) {
	b := Backoff{Base: time.Second, Max: time.Minute, StableFor: 30 * time.Second}
	b.Next() // 1s
	b.Next() // 2s

	b.Settle(5 * time.Second)
	if got := b.Next(); got != 4*time.Second {
		t.Errorf("Next() after a short connection = %v, want %v (schedule must keep growing)", got, 4*time.Second)
	}

	b.Settle(45 * time.Second)
	if got := b.Next(); got != time.Second {
		t.Errorf("Next() after a stable connection = %v, want %v", got, time.Second)
	}
}

// Exactly at the threshold counts as stable: the boundary belongs to the
// healthy side, so a StableFor the caller sets to "one poll interval" is not
// missed by a nanosecond.
func TestBackoffSettleAtTheThresholdResets(t *testing.T) {
	b := Backoff{Base: time.Second, Max: time.Minute, StableFor: 30 * time.Second}
	b.Next()
	b.Settle(30 * time.Second)
	if got := b.Next(); got != time.Second {
		t.Errorf("Next() after exactly StableFor = %v, want %v", got, time.Second)
	}
}
