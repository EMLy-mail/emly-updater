package service

import (
	"context"
	"testing"
	"time"

	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/wsclient"
)

type resolvedSession struct {
	c       machineinfo.SessionChange
	changed bool
}

func newSessionWatchUpdater(t *testing.T, user *machineinfo.UserSession) (*Updater, chan resolvedSession) {
	t.Helper()
	out := make(chan resolvedSession, 8)
	u := &Updater{
		Log:            testLogger(t),
		sessionChanges: make(chan machineinfo.SessionChange, sessionChangeBuffer),
		sessionSettle:  20 * time.Millisecond,
		resolveSessionFn: func(c machineinfo.SessionChange) machineinfo.SessionChange {
			c.LoggedUser = *user
			return c
		},
		onSessionChange: func(c machineinfo.SessionChange, changed bool) {
			out <- resolvedSession{c, changed}
		},
		emitFn: func(string, any) {},
	}
	return u, out
}

func waitResolved(t *testing.T, out chan resolvedSession) resolvedSession {
	t.Helper()
	select {
	case r := <-out:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("session change was never resolved")
		return resolvedSession{}
	}
}

// A burst (an RDP reconnect) is resolved once, from its last notification.
func TestWatchSessionsCoalescesBurst(t *testing.T) {
	user := machineinfo.UserSession{User: `TREGCC\mrossi`, State: machineinfo.SessionActiveRDP}
	u, out := newSessionWatchUpdater(t, &user)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go u.watchSessions(ctx)

	u.queueSessionChange(machineinfo.SessionChange{Kind: machineinfo.SessionConsoleDisconnect, SessionID: 1})
	u.queueSessionChange(machineinfo.SessionChange{Kind: machineinfo.SessionRemoteConnect, SessionID: 2})
	u.queueSessionChange(machineinfo.SessionChange{Kind: machineinfo.SessionUnlock, SessionID: 2})

	r := waitResolved(t, out)
	if r.c.Kind != machineinfo.SessionUnlock || r.c.SessionID != 2 || !r.changed {
		t.Fatalf("resolved %+v changed=%v, want the last event (unlock, session 2) and changed", r.c, r.changed)
	}
	select {
	case extra := <-out:
		t.Fatalf("burst resolved more than once: %+v", extra.c)
	case <-time.After(100 * time.Millisecond):
	}
}

// A second burst that leaves the same user in the same state is not a change.
func TestWatchSessionsReportsUnchanged(t *testing.T) {
	user := machineinfo.UserSession{User: `TREGCC\mrossi`, State: machineinfo.SessionActiveConsole}
	u, out := newSessionWatchUpdater(t, &user)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go u.watchSessions(ctx)

	u.queueSessionChange(machineinfo.SessionChange{Kind: machineinfo.SessionLogon, SessionID: 1})
	if r := waitResolved(t, out); !r.changed {
		t.Fatal("first resolution after a logon should be a change")
	}
	u.queueSessionChange(machineinfo.SessionChange{Kind: machineinfo.SessionLock, SessionID: 1})
	if r := waitResolved(t, out); r.changed {
		t.Fatal("lock left the same user in the same state, want changed=false")
	}
}

// A resolved session change is pushed over the client channel too, carrying
// the whole burst's kinds and the resolved logged-on user.
func TestWatchSessionsEmitsSessionChanged(t *testing.T) {
	user := machineinfo.UserSession{User: `TREGCC\mrossi`, State: machineinfo.SessionActiveConsole}
	u, out := newSessionWatchUpdater(t, &user)
	got := make(chan wsclient.SessionChanged, 1)
	u.emitFn = func(name string, p any) {
		if name == wsclient.EvtSessionChanged {
			got <- p.(wsclient.SessionChanged)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go u.watchSessions(ctx)
	u.queueSessionChange(machineinfo.SessionChange{Kind: machineinfo.SessionLogon, SessionID: 1})
	u.queueSessionChange(machineinfo.SessionChange{Kind: machineinfo.SessionUnlock, SessionID: 1})
	waitResolved(t, out)
	p := <-got
	if len(p.Events) != 2 || p.Events[1] != "unlock" || !p.Changed || p.LoggedUser.User != `TREGCC\mrossi` {
		t.Fatalf("payload = %+v", p)
	}
}

// The control loop must never block on a full queue.
func TestQueueSessionChangeNeverBlocks(t *testing.T) {
	u := &Updater{Log: testLogger(t), sessionChanges: make(chan machineinfo.SessionChange, 1)}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 5; i++ {
			u.queueSessionChange(machineinfo.SessionChange{Kind: machineinfo.SessionLock})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("queueSessionChange blocked on a full queue")
	}
}
