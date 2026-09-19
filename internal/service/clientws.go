package service

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"emlyupdater/internal/logging"
	"emlyupdater/internal/source"
	"emlyupdater/internal/wsclient"
)

// clientWSTarget is everything the presence supervisor needs to know from
// the current cycle: whether the remote document enables the channel at all,
// and which server to hold it open against.
//
// It is deliberately comparable, so the supervisor can watch for a change
// with a plain != while a connection is up.
type clientWSTarget struct {
	enabled bool
	server  string // the server's name in the document, for the log
	url     string // the full ws:// or wss:// endpoint
}

// clientWSTarget reads the current cycle and resolves where the presence
// channel should be pointed.
//
// The channel has no notion of its own about the right server: it reads the
// same chain beginCycle built for the rest of the cycle and takes the head
// of it - the site's base server for a machine on a mapped LAN, the default
// server for everyone else. A machine that moves site therefore moves its
// presence connection with it, with no extra configuration anywhere.
//
// A zero clientWSTarget means "do not connect", which covers every reason
// not to: the document's kill switch is off, no cycle has run yet, or the
// chosen server's base URL is not something a WebSocket can be built from.
// The last case is what a legacy-derived policy produces when config.ini
// carried a hand-edited manifest path, and it is harmless precisely because
// such a machine has no remote document and therefore no kill switch on.
//
// This deliberately pins to chain[0] only - it never falls through to
// chain[1:] the way resolveTarget's Resolver does for the manifest poll.
// That is not an oversight: this machine's telemetry rows (the updater_
// events/updater_clients history) live on whichever instance actually
// served its manifest, so its presence belongs on that same instance too.
// A mirror-site machine whose presence connection fell through to the cloud
// instance would split its state across two databases - the cloud one would
// show it "online" while carrying none of its history, and the mirror would
// show its full history but never "online". Losing presence entirely while
// the site's own server is briefly unreachable is the correct trade-off.
func (u *Updater) clientWSTarget() clientWSTarget {
	cyc := u.cur.Load()
	if cyc == nil || !cyc.eff.Doc.ClientWS.Enabled || len(cyc.chain) == 0 {
		return clientWSTarget{}
	}
	name := cyc.chain[0]
	url, err := wsclient.URLFor(cyc.eff.BaseURL(name))
	if err != nil {
		u.Log.Warn("presence channel has no usable endpoint on the current server, staying closed",
			"server", name, "baseURL", cyc.eff.BaseURL(name), "error", err.Error())
		return clientWSTarget{}
	}
	return clientWSTarget{enabled: true, server: name, url: url}
}

// clientWSIdentity resolves this machine's identity for the channel's first
// message.
//
// It goes through newHTTPSource rather than reading u.Machine directly, so
// the values - and above all the two that are re-resolved on every request,
// the logged-on user and EMLy's installed version - come from the one place
// the X-EMLy-* headers come from. A field added to the headers and not here
// still has to be added twice (see AGENTS.md), but at least the two paths
// cannot disagree about *how* a value is resolved.
//
// The manifest URL passed to newHTTPSource is empty because nothing here
// fetches anything: only the identity fields are read off the result.
//
// X-EMLy-IntIP has no counterpart in the payload on purpose (spec §4): it is
// specific to the manifest/download path, and the API reads this
// connection's address off the connection itself.
func (u *Updater) clientWSIdentity() wsclient.Identity {
	s := u.newHTTPSource("")
	return identityFromSource(s)
}

// identityFromSource maps an HTTPSource's identity fields onto the JSON
// payload. Split out from clientWSIdentity so the mapping itself - the part
// that goes stale when a field is added - is one obvious list.
func identityFromSource(s *source.HTTPSource) wsclient.Identity {
	id := wsclient.Identity{
		HWID:            s.HWID,
		Hostname:        s.Hostname,
		ADDomain:        s.ADDomain,
		LoggedUser:      s.LoggedUser,
		LoggedUserState: s.LoggedUserState,
		Serial:          s.Serial,
		Product:         s.Product,
		OSVersion:       s.OSVersion,
		EMLyVersion:     s.EMLyVersion,
	}
	if !s.LoggedUserDisconnectedAt.IsZero() {
		id.LoggedUserDisconnectedAt = s.LoggedUserDisconnectedAt.UTC().Format(time.RFC3339)
	}
	return id
}

// clientWSWatchInterval is the default for how often the supervisor
// re-reads the current cycle while a connection is up, so a policy change -
// a different server, or the kill switch going off - is followed without
// waiting for the connection to fail on its own. It is also how long the
// supervisor idles when there is nothing to connect to.
//
// 15s is short enough that stopping the channel feels immediate to whoever
// published the revision and long enough to cost nothing: it is an atomic
// pointer load and three string comparisons. Updater.watchInterval() is the
// effective value - tests shorten it via the clientWSWatch field rather than
// sleeping out a real 15s per test.
const clientWSWatchInterval = 15 * time.Second

// clientWSInitialJitterMax bounds the randomised delay runClientWS waits
// before its very first dial attempt (see the call site). 60s matches the
// order of magnitude of Backoff's own ceiling, wide enough to meaningfully
// spread a fleet-wide start without meaningfully delaying it.
const clientWSInitialJitterMax = 60 * time.Second

// clientWSUnsupportedRetryAfter is how long a server stays marked
// unsupported (see the 404 case in runClientWS) before the supervisor tries
// it again on its own, without waiting for the source policy to move this
// machine elsewhere. An hour is long enough that a mirror mid-deploy is not
// hammered, short enough that an ingress or load balancer answering 404 for
// a few seconds during an API deploy does not blind a machine until its
// service next restarts.
const clientWSUnsupportedRetryAfter = time.Hour

// watchInterval is clientWSWatchInterval, overridable by tests via
// clientWSWatch so the supervisor suite does not sleep 15s per test.
func (u *Updater) watchInterval() time.Duration {
	if u.clientWSWatch > 0 {
		return u.clientWSWatch
	}
	return clientWSWatchInterval
}

// clientWSInitialDelay is the randomised startup delay runClientWS waits
// before its first dial attempt, overridable by tests via
// clientWSInitialDelayFn so the supervisor suite does not sleep up to 60s
// per test.
func (u *Updater) clientWSInitialDelay() time.Duration {
	if u.clientWSInitialDelayFn != nil {
		return u.clientWSInitialDelayFn()
	}
	if clientWSInitialJitterMax <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(clientWSInitialJitterMax) + 1))
}

// runClientWS holds the presence channel open for the life of the service.
//
// The loop is the whole feature: resolve where to connect, connect, and on
// any ending decide whether to retry, wait, or stand down. It returns only
// when ctx ends.
//
// What it deliberately does not do is log once per attempt. Events fire on
// state changes - a connection established, a connection that was really up
// going down, a server that turned out not to serve the endpoint, the
// document switching the channel off. A machine that is simply off the
// network retries quietly on the backoff and writes nothing above Debug,
// because the alternative is ~400 machines filling their Event Log for the
// duration of every outage.
func (u *Updater) runClientWS(ctx context.Context) {
	// Spread the very first dial attempt across a window before entering the
	// loop at all: a fleet-wide first enablement (a revision turns the kill
	// switch on) or a fleet-wide reconnect (the API restarted) would
	// otherwise have every machine's *first* attempt land in the same
	// instant - Backoff's own full jitter only applies from the second
	// attempt onward, once Next has actually been called. See Backoff's doc
	// comment for why a synchronised burst matters here (the API's rate
	// limiter and the shared-NAT-per-site fact, not server load).
	if !u.clientWSIdle(ctx, u.clientWSInitialDelay()) {
		return
	}

	backoff := wsclient.Backoff{}
	// unsupported remembers the servers that answered 404 on the upgrade,
	// and when. Keyed by server name rather than URL because that is what
	// the source policy changes: when beginCycle moves this machine to
	// another site, the new server has never been asked and gets its
	// chance. A mark expires after clientWSUnsupportedRetryAfter - see its
	// doc comment - so a server is also retried on its own after an hour,
	// not only on a policy change.
	unsupported := map[string]time.Time{}
	running := false

	for ctx.Err() == nil {
		target := u.clientWSTarget()

		if !target.enabled {
			if running {
				u.Log.InfoEvent(logging.EventClientWSDisabled,
					"presence channel switched off by the remote configuration")
				running = false
			}
			if !u.clientWSIdle(ctx, u.watchInterval()) {
				return
			}
			continue
		}
		if markedAt, marked := unsupported[target.server]; marked &&
			u.clock().Sub(markedAt) < clientWSUnsupportedRetryAfter {
			// Nothing to do until the source policy picks another server, or
			// the mark expires.
			if !u.clientWSIdle(ctx, u.watchInterval()) {
				return
			}
			continue
		}

		connCtx, cancel := context.WithCancel(ctx)
		go u.watchClientWSTarget(connCtx, cancel, target)

		u.Log.Debug("presence channel attempting connection",
			"server", target.server, "url", target.url)

		var (
			connected   bool
			connectedAt time.Time
		)
		client := &wsclient.Client{
			URL:       target.url,
			APIKey:    u.Cfg.APIKey,
			UserAgent: u.Cfg.UserAgent,
			Identity:  u.clientWSIdentity(),
			Logf: func(format string, args ...any) {
				u.Log.Debug(fmt.Sprintf(format, args...))
			},
			// Called synchronously from inside Run, on this goroutine, so
			// these two need no synchronisation. running is only set true
			// here, on a genuine connection - not before the dial - so a
			// machine that never managed to connect does not log event 923
			// ("switched off") for a channel that was never really running.
			OnConnected: func() {
				connected, connectedAt = true, u.clock()
				running = true
				u.Log.InfoEvent(logging.EventClientWSConnected, "presence channel established",
					"server", target.server, "url", target.url)
			},
		}

		err := client.Run(connCtx)
		// connCtx.Err() must be read now, before cancel() below - once we
		// cancel it ourselves it is always non-nil, which would make every
		// outcome (a 404, a lost connection, ...) look like the watcher
		// fired and retry immediately with no backoff.
		watcherCancelled := connCtx.Err() != nil
		cancel()

		if ctx.Err() != nil {
			return
		}

		switch {
		case watcherCancelled:
			// The watcher cancelled us: the policy chose a different server,
			// or turned the channel off. Neither is a failure, so the next
			// attempt starts on a fresh schedule - and immediately, since
			// there is nothing to back off from.
			u.Log.Info("presence channel closing to follow a policy change", "server", target.server)
			backoff.Reset()
			continue

		case errors.Is(err, wsclient.ErrNotImplemented):
			// Log event 922 only the first time this server is marked, not
			// on every re-mark: the mark expiring after an hour and getting
			// re-set because the mirror still hasn't been upgraded is the
			// same ongoing outage, not a new one, and re-logging it would
			// defeat the point of the expiry (a long mirror lag must stay
			// quiet in the Event Log). Same "this is not a failure" reason
			// the watcherCancelled case resets on: the schedule should not
			// carry a growing ceiling into an unrelated server.
			if _, alreadyMarked := unsupported[target.server]; !alreadyMarked {
				u.Log.WarnEvent(logging.EventClientWSUnsupported,
					"server does not implement the presence endpoint, not asking it again until the source policy changes server or the mark expires",
					"server", target.server, "url", target.url)
			}
			unsupported[target.server] = u.clock()
			backoff.Reset()
			continue

		case errors.Is(err, wsclient.ErrUnauthorized):
			// Unlike a 404 this is a misconfiguration, not a state to live
			// with, so it keeps retrying on the backoff and keeps saying so.
			// The log line stays neutral about the cause - err carries the
			// actual status code (401 vs 403), because a 403 here can just
			// as well be the API's rate limiter answering a ban (see
			// Backoff's doc comment) as a wrong key, and sending an operator
			// hunting for a key misconfiguration during exactly that
			// incident is the wrong prompt.
			u.Log.Warn("presence endpoint rejected the connection, retrying on the backoff",
				"server", target.server, "url", target.url, "error", errText(err))

		case connected:
			upFor := u.clock().Sub(connectedAt)
			backoff.Settle(upFor)
			u.Log.WarnEvent(logging.EventClientWSLost, "presence channel lost",
				"server", target.server, "upFor", upFor.Round(time.Second).String(),
				"error", errText(err))

		default:
			u.Log.Debug("presence channel could not be established, retrying",
				"server", target.server, "url", target.url, "error", errText(err))
		}

		if !u.clientWSIdle(ctx, backoff.Next()) {
			return
		}
	}
}

// watchClientWSTarget cancels the running connection as soon as the cycle
// points somewhere else - a different server, or nowhere at all.
//
// It exists because the connection is a blocking read that can legitimately
// last for days: without it, a kill switch published at 09:00 would take
// effect whenever the connection happened to drop next.
func (u *Updater) watchClientWSTarget(ctx context.Context, cancel context.CancelFunc, running clientWSTarget) {
	ticker := time.NewTicker(u.watchInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if u.clientWSTarget() != running {
				cancel()
				return
			}
		}
	}
}

// clientWSIdle waits for d, or until the service is asked to stop. It
// reports whether the caller should carry on.
func (u *Updater) clientWSIdle(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		d = time.Second
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// errText renders an error for a log field, tolerating nil - a connection
// can end without one when the server closed it cleanly.
func errText(err error) string {
	if err == nil {
		return "closed by the server"
	}
	return err.Error()
}
