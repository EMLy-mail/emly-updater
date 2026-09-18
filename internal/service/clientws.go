package service

import (
	"context"
	"errors"
	"fmt"
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

// clientWSWatchInterval is how often the supervisor re-reads the current
// cycle while a connection is up, so a policy change - a different server,
// or the kill switch going off - is followed without waiting for the
// connection to fail on its own. It is also how long the supervisor idles
// when there is nothing to connect to.
//
// 15s is short enough that stopping the channel feels immediate to whoever
// published the revision and long enough to cost nothing: it is an atomic
// pointer load and three string comparisons.
const clientWSWatchInterval = 15 * time.Second

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
	backoff := wsclient.Backoff{}
	// unsupported remembers the servers that answered 404 on the upgrade.
	// Keyed by server name rather than URL because that is what the source
	// policy changes: when beginCycle moves this machine to another site,
	// the new server has never been asked and gets its chance.
	unsupported := map[string]bool{}
	running := false

	for ctx.Err() == nil {
		target := u.clientWSTarget()

		if !target.enabled {
			if running {
				u.Log.InfoEvent(logging.EventClientWSDisabled,
					"presence channel switched off by the remote configuration")
				running = false
			}
			if !u.clientWSIdle(ctx, clientWSWatchInterval) {
				return
			}
			continue
		}
		if unsupported[target.server] {
			// Nothing to do until the source policy picks another server.
			if !u.clientWSIdle(ctx, clientWSWatchInterval) {
				return
			}
			continue
		}
		running = true

		connCtx, cancel := context.WithCancel(ctx)
		go u.watchClientWSTarget(connCtx, cancel, target)

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
			// these two need no synchronisation.
			OnConnected: func() {
				connected, connectedAt = true, u.clock()
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
			unsupported[target.server] = true
			u.Log.WarnEvent(logging.EventClientWSUnsupported,
				"server does not implement the presence endpoint, not asking it again until the source policy changes server",
				"server", target.server, "url", target.url)
			continue

		case errors.Is(err, wsclient.ErrUnauthorized):
			// Unlike a 404 this is a misconfiguration, not a state to live
			// with, so it keeps retrying on the backoff and keeps saying so.
			u.Log.Warn("presence endpoint rejected the API key, retrying on the backoff",
				"server", target.server, "url", target.url)

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
	ticker := time.NewTicker(clientWSWatchInterval)
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
