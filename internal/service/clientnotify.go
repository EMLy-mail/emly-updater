package service

import (
	"encoding/json"
	"math/rand/v2"
	"time"

	"emlyupdater/internal/manifest"
	"emlyupdater/internal/version"
	"emlyupdater/internal/wsclient"
)

const minReleaseJitter = 60 * time.Second

// minConfigJitter is config.published's client-side floor, the same idea as
// minReleaseJitter but shorter: the document is already cached and merely
// stale, not missing an update, so a fleet-wide push does not need release's
// full 60s spread to avoid a stampede, but still needs one - the document
// only carries jitterSeconds as a suggestion, and a compromised or buggy
// publisher sending 0 must not be able to wake every machine of a fleet at
// once.
const minConfigJitter = 30 * time.Second

// notifyWakeThrottle bounds how often a notify may schedule an early wake of
// RunLoop, across both topics: at most once per this window. A burst of
// release.published/config.published pushes (several sites, or a publisher
// retrying) must not collapse into a stampede of early cycles - the
// ordinary poll interval keeps the machine converging regardless of whether
// a given notify's wake was allowed through.
const notifyWakeThrottle = 10 * time.Minute

// handleNotify turns a notify into a wake-up of RunLoop after a random
// delay (spec §9). It never runs a cycle itself (R-LOOP) and never trusts
// the payload beyond deciding whether waking up is worth it: the cycle
// re-reads the manifest or the document and validates it as always.
func (u *Updater) handleNotify(n wsclient.Notify) {
	switch n.Topic {
	case wsclient.TopicReleasePublished:
		var p wsclient.ReleasePublished
		if json.Unmarshal(n.Payload, &p) != nil || !u.releaseConcernsMe(p) {
			return
		}
		jitter := time.Duration(p.JitterSeconds) * time.Second
		if jitter < minReleaseJitter {
			jitter = minReleaseJitter
		}
		if !u.allowNotifyWake() {
			u.Log.Debug("notify-triggered wake throttled, the ordinary poll will still run",
				"topic", n.Topic, "target", p.Target, "version", p.Version)
			return
		}
		u.Log.Info("release announced over the client channel, waking the update loop",
			"target", p.Target, "version", p.Version, "maxDelay", jitter.String())
		u.scheduleWake("notify", jitter)
	case wsclient.TopicConfigPublished:
		var p wsclient.ConfigPublished
		if json.Unmarshal(n.Payload, &p) != nil {
			return
		}
		if cyc := u.cur.Load(); cyc != nil && p.Revision <= cyc.snap.Revision() {
			return
		}
		// forceConfig is set unconditionally, even when the wake below is
		// throttled: it is what makes the *next* poll (early or ordinary)
		// fetch unconditionally instead of waiting out the interval, and
		// that must still happen even when this particular notify does not
		// get to wake RunLoop early.
		u.forceConfig.Store(true)
		jitter := time.Duration(p.JitterSeconds) * time.Second
		if jitter < minConfigJitter {
			jitter = minConfigJitter
		}
		if !u.allowNotifyWake() {
			u.Log.Debug("notify-triggered wake throttled, the ordinary poll will still run",
				"topic", n.Topic, "revision", p.Revision)
			return
		}
		u.scheduleWake("notify", jitter)
	default:
		u.Log.Debug("unknown notify topic ignored", "topic", n.Topic)
	}
}

// allowNotifyWake reports whether a notify may schedule an early RunLoop
// wake right now, and if so records this moment as the last one allowed -
// see notifyWakeThrottle. handleNotify runs on the presence connection's own
// read goroutine (wsclient.Handler's doc comment), not the poll goroutine,
// so lastNotifyWake needs its own lock unlike the poll-goroutine-only fields
// nearby (announced, cycleTrigger).
func (u *Updater) allowNotifyWake() bool {
	now := u.clock()
	u.notifyWakeMu.Lock()
	defer u.notifyWakeMu.Unlock()
	if !u.lastNotifyWake.IsZero() && now.Sub(u.lastNotifyWake) < notifyWakeThrottle {
		return false
	}
	u.lastNotifyWake = now
	return true
}

func (u *Updater) releaseConcernsMe(p wsclient.ReleasePublished) bool {
	switch p.Target {
	case "updater":
		newer, err := manifest.Less(version.Version, p.Version)
		return err == nil && newer
	case "emly":
		cyc := u.cur.Load()
		if cyc == nil {
			return false
		}
		emly := u.Cfg.ResolveEMLyWithChannel(cyc.eff.Doc.Updater.Channel())
		if p.Channel != "" && p.Channel != emly.Channel {
			return false
		}
		newer, err := manifest.Less(emly.InstalledVersion, p.Version)
		return err == nil && newer
	}
	return false
}

// scheduleWake wakes RunLoop after a random delay in [0, maxJitter]. The
// wake channel holds one reason: extra notifies before RunLoop picks it up
// collapse into that one, so a burst never queues several cycles.
func (u *Updater) scheduleWake(reason string, maxJitter time.Duration) {
	jitter := u.jitterFn
	if jitter == nil {
		jitter = func(m time.Duration) time.Duration {
			if m <= 0 {
				return 0
			}
			return time.Duration(rand.Int64N(int64(m) + 1))
		}
	}
	after := u.afterFunc
	if after == nil {
		after = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
	}
	after(jitter(maxJitter), func() {
		defer u.recoverGoroutine("scheduleWake")
		select {
		case u.wake <- reason:
		default:
		}
	})
}
