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
		u.forceConfig.Store(true)
		u.scheduleWake("notify", time.Duration(max(p.JitterSeconds, 0))*time.Second)
	default:
		u.Log.Debug("unknown notify topic ignored", "topic", n.Topic)
	}
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
		select {
		case u.wake <- reason:
		default:
		}
	})
}
