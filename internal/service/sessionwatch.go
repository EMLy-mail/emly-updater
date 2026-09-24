package service

import (
	"context"
	"time"

	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/wsclient"
)

// sessionSettle is how long watchSessions waits for the notifications to go
// quiet before it resolves who is logged on. An RDP reconnect arrives as a
// burst (remote-disconnect of the console, remote-connect, sometimes logon
// and unlock) and WTS can still report the pre-change state for a moment
// after the first of them.
const sessionSettle = 1500 * time.Millisecond

// sessionChangeBuffer bounds the queue between the service control loop and
// watchSessions. The control loop never blocks on it (see
// queueSessionChange): a full buffer drops the event, which is harmless,
// since the burst it belongs to is resolved as a whole anyway.
const sessionChangeBuffer = 16

// queueSessionChange hands a notification from the service control loop to
// watchSessions without ever blocking: the control loop also carries Stop
// and Shutdown, and must not wait on this.
func (u *Updater) queueSessionChange(c machineinfo.SessionChange) {
	if u.sessionChanges == nil {
		return
	}
	select {
	case u.sessionChanges <- c:
	default:
		u.Log.Debug("session change dropped, queue full", "kind", c.Kind, "sessionID", c.SessionID)
	}
}

// resolveSession runs the slow WTS half of a session change, through the
// seam the tests pin.
func (u *Updater) resolveSession(c machineinfo.SessionChange) machineinfo.SessionChange {
	if u.resolveSessionFn != nil {
		return u.resolveSessionFn(c)
	}
	return c.Resolve()
}

// watchSessions turns the SCM's session notifications into "who is at this
// machine now", in real time instead of once per poll cycle. It coalesces a
// burst (see sessionSettle), resolves once, and logs the result along with
// every kind of notification the burst carried.
//
// It is a trigger, not a source of truth: the poll cycle keeps resolving
// the logged-on user on its own (newHTTPSource), so a notification lost to
// a full queue or a service restart costs latency, never correctness. The
// foreground `run` mode gets no notifications at all and relies on that
// path alone.
func (u *Updater) watchSessions(ctx context.Context) {
	if u.sessionChanges == nil {
		return
	}
	settle := sessionSettle
	if u.sessionSettle > 0 {
		settle = u.sessionSettle
	}

	var (
		last  machineinfo.SessionChange
		kinds []machineinfo.SessionChangeKind
		timer *time.Timer
		fire  <-chan time.Time
		prev  machineinfo.UserSession
	)
	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case c := <-u.sessionChanges:
			last = c
			kinds = append(kinds, c.Kind)
			if timer == nil {
				timer = time.NewTimer(settle)
			} else {
				timer.Reset(settle)
			}
			fire = timer.C
		case <-fire:
			fire = nil
			got := u.resolveSession(last)
			changed := got.LoggedUser != prev
			prev = got.LoggedUser
			u.Log.Info("session change",
				"events", kinds,
				"sessionID", got.SessionID,
				"sessionUser", got.SessionUser,
				"loggedUser", got.LoggedUser.User,
				"loggedUserState", string(got.LoggedUser.State),
				"changed", changed,
			)
			// Push the result over the client channel (CLIENT_WS_PROTOCOL.md
			// §8.1). Not a source of truth: with the channel down, emit drops
			// this one instead of buffering it - it only anticipates what the
			// next poll's X-EMLy-LoggedUser* headers would carry anyway, and
			// would be stale by the time a channel reconnects.
			u.emit(wsclient.EvtSessionChanged, sessionChangedPayload(kinds, got, changed))

			kinds = nil

			if u.onSessionChange != nil {
				u.onSessionChange(got, changed)
			}
		}
	}
}
