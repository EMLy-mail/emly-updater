package service

import (
	"encoding/json"
	"testing"
	"time"

	"emlyupdater/internal/wsclient"
)

func notifyOf(t *testing.T, topic string, payload any) wsclient.Notify {
	t.Helper()
	b, _ := json.Marshal(payload)
	return wsclient.Notify{Topic: topic, Payload: b}
}

func newNotifyUpdater(t *testing.T) (*Updater, *[]time.Duration) {
	u := newClientTestUpdater(t)
	withPolicy(t, u)
	u.wake = make(chan string, 1)
	var delays []time.Duration
	u.jitterFn = func(max time.Duration) time.Duration { return max }
	u.afterFunc = func(d time.Duration, f func()) { delays = append(delays, d); f() }
	return u, &delays
}

func TestReleasePublishedWakesWithMinimumJitter(t *testing.T) {
	u, delays := newNotifyUpdater(t)
	u.handleNotify(notifyOf(t, wsclient.TopicReleasePublished,
		wsclient.ReleasePublished{Target: "updater", Version: "99.0.0", JitterSeconds: 10}))
	if len(*delays) != 1 || (*delays)[0] != 60*time.Second {
		t.Fatalf("delays = %v", *delays)
	}
	if r := <-u.wake; r != "notify" {
		t.Fatalf("reason = %s", r)
	}
}

func TestReleasePublishedForOlderVersionIgnored(t *testing.T) {
	u, delays := newNotifyUpdater(t)
	u.handleNotify(notifyOf(t, wsclient.TopicReleasePublished,
		wsclient.ReleasePublished{Target: "updater", Version: "0.0.1", JitterSeconds: 600}))
	if len(*delays) != 0 {
		t.Fatalf("woke for an older release: %v", *delays)
	}
}

func TestConfigPublishedForcesRefresh(t *testing.T) {
	u, delays := newNotifyUpdater(t)
	u.handleNotify(notifyOf(t, wsclient.TopicConfigPublished, wsclient.ConfigPublished{Revision: 1 << 40, JitterSeconds: 30}))
	if !u.forceConfig.Load() || len(*delays) != 1 || (*delays)[0] != 30*time.Second {
		t.Fatalf("force=%v delays=%v", u.forceConfig.Load(), *delays)
	}
}

// config.published gets the same kind of client-side floor
// release.published already has, just shorter: the document is only stale,
// not missing, so a fleet-wide push does not need release's full spread,
// but still needs one - jitterSeconds is a server suggestion, not something
// a small value (or 0) is allowed to defeat.
func TestConfigPublishedGetsAMinimumJitterFloor(t *testing.T) {
	u, delays := newNotifyUpdater(t)
	u.handleNotify(notifyOf(t, wsclient.TopicConfigPublished, wsclient.ConfigPublished{Revision: 1 << 40, JitterSeconds: 5}))
	if len(*delays) != 1 || (*delays)[0] != minConfigJitter {
		t.Fatalf("delays = %v, want the %s floor", *delays, minConfigJitter)
	}
}

// At most one notify-triggered wake is allowed per notifyWakeThrottle,
// across both topics: a second notify well within the window must not
// schedule another early wake, but forceConfig must still be set (the
// ordinary poll still forces a fetch), and a notify arriving once the
// window has fully elapsed wakes again.
func TestNotifyWakeThrottledWithinTenMinutes(t *testing.T) {
	u, delays := newNotifyUpdater(t)
	now := time.Now()
	u.nowFn = func() time.Time { return now }

	u.handleNotify(notifyOf(t, wsclient.TopicReleasePublished,
		wsclient.ReleasePublished{Target: "updater", Version: "99.0.0", JitterSeconds: 60}))
	if len(*delays) != 1 {
		t.Fatalf("first notify did not wake: %v", *delays)
	}

	now = now.Add(time.Minute)
	u.handleNotify(notifyOf(t, wsclient.TopicConfigPublished, wsclient.ConfigPublished{Revision: 1 << 40, JitterSeconds: 30}))
	if len(*delays) != 1 {
		t.Fatalf("throttled notify still scheduled a wake: %v", *delays)
	}
	if !u.forceConfig.Load() {
		t.Fatal("forceConfig must still be set even when the wake itself is throttled")
	}

	now = now.Add(notifyWakeThrottle)
	u.handleNotify(notifyOf(t, wsclient.TopicReleasePublished,
		wsclient.ReleasePublished{Target: "updater", Version: "99.0.0", JitterSeconds: 60}))
	if len(*delays) != 2 {
		t.Fatalf("delays after the window elapsed = %v, want 2", *delays)
	}
}

func TestNotifyCoalescesWakes(t *testing.T) {
	u, _ := newNotifyUpdater(t)
	n := notifyOf(t, wsclient.TopicReleasePublished, wsclient.ReleasePublished{Target: "updater", Version: "99.0.0", JitterSeconds: 60})
	u.handleNotify(n)
	u.handleNotify(n)
	u.handleNotify(n)
	if len(u.wake) != 1 {
		t.Fatalf("pending wakes = %d, want 1", len(u.wake))
	}
}
