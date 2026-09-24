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
