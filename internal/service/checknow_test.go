package service

import (
	"testing"
	"time"
)

func TestRequestCheckWakesAndForcesConfig(t *testing.T) {
	u := newClientTestUpdater(t)
	u.wake = make(chan string, 1)
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	u.nowFn = func() time.Time { return now }

	u.RequestCheck()
	if r := <-u.wake; r != checkNowWake {
		t.Fatalf("reason = %q", r)
	}
	if !u.forceConfig.Load() {
		t.Fatal("forceConfig not set")
	}

	// Within the throttle: no wake.
	u.forceConfig.Store(false)
	now = now.Add(checkNowThrottle - time.Second)
	u.RequestCheck()
	if len(u.wake) != 0 || u.forceConfig.Load() {
		t.Fatal("throttled request still woke the loop")
	}

	// After it: wakes again.
	now = now.Add(2 * time.Second)
	u.RequestCheck()
	if len(u.wake) != 1 {
		t.Fatal("request after the throttle did not wake the loop")
	}
}

func TestRequestCheckNeverBlocks(t *testing.T) {
	u := newClientTestUpdater(t)
	u.wake = make(chan string, 1)
	u.wake <- "notify" // a wake is already pending
	u.RequestCheck()   // must return, not block on the full channel
	if r := <-u.wake; r != "notify" {
		t.Fatalf("pending wake replaced: %q", r)
	}
}
