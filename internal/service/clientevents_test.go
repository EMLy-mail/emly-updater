package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/state"
	"emlyupdater/internal/wsclient"
)

func TestCapabilitiesDeclareEverythingThisBuildImplements(t *testing.T) {
	u := &Updater{}
	got := map[string]bool{}
	for _, c := range u.capabilities() {
		got[c] = true
	}
	for _, list := range [][]string{wsclient.CommandNames, wsclient.EventNames, wsclient.TopicNames} {
		for _, n := range list {
			if !got[n] {
				t.Errorf("capability %s missing", n)
			}
		}
	}
}

// machine.info names both a command and an event; capabilities must not
// repeat it.
func TestCapabilitiesHasNoDuplicates(t *testing.T) {
	u := &Updater{}
	seen := map[string]int{}
	for _, c := range u.capabilities() {
		seen[c]++
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("capability %q listed %d times, want 1", name, n)
		}
	}
}

// Welcome runs on the connection's own read goroutine, which also answers
// pings, so it must hand the burst of work off to its own goroutine and
// return immediately - never block on it, however slow the burst turns out
// to be.
func TestWelcomeReturnsWithoutBlockingOnASlowBurst(t *testing.T) {
	u := &Updater{Log: testLogger(t)}
	block := make(chan struct{})
	called := make(chan struct{})
	u.welcomeBurstFn = func(gen uint64, s *wsclient.Session) {
		close(called)
		<-block
	}
	defer close(block)

	returned := make(chan struct{})
	go func() {
		clientHandler{u: u}.Welcome(nil, wsclient.Welcome{})
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("Welcome did not return promptly while the burst was blocked")
	}
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("the burst function was never invoked")
	}
}

// The service.started payload is built at most once per process: a failed
// send must not lose the pending command ids (TakePendingCommands is
// destructive) or the reason it computed - the retry has to report exactly
// what the first attempt would have.
func TestServiceStartedRetryReusesTheCachedPayload(t *testing.T) {
	u := newClientTestUpdater(t)
	if err := u.Store.AddPendingCommand(state.PendingCommand{ID: "cmd-1", Name: wsclient.CmdServiceRestart}); err != nil {
		t.Fatalf("AddPendingCommand: %v", err)
	}

	attempt := 0
	var gotPayload wsclient.ServiceStarted
	u.wsSendFn = func(name string, payload any) error {
		if name != wsclient.EvtServiceStarted {
			return nil
		}
		attempt++
		if attempt == 1 {
			return errors.New("connection reset")
		}
		gotPayload = payload.(wsclient.ServiceStarted)
		return nil
	}

	u.handleWelcomeBurst(0, nil) // first attempt: the send fails
	if u.startedSent.Load() {
		t.Fatal("startedSent should have been reset after a failed send")
	}
	u.handleWelcomeBurst(0, nil) // retry: must reuse the cached payload

	if attempt != 2 {
		t.Fatalf("service.started send attempts = %d, want 2", attempt)
	}
	if len(gotPayload.CompletedCommands) != 1 || gotPayload.CompletedCommands[0] != "cmd-1" {
		t.Fatalf("retry payload = %+v, want the original pending command id preserved", gotPayload)
	}
	st, err := u.Store.Load()
	if err != nil {
		t.Fatalf("Store.Load: %v", err)
	}
	if len(st.PendingCommands) != 0 {
		t.Fatalf("pending commands = %v, want them consumed exactly once", st.PendingCommands)
	}
}

func TestEventsBufferedUntilWelcome(t *testing.T) {
	u := &Updater{Log: testLogger(t)}
	u.emit(wsclient.EvtUpdateApplied, map[string]string{"target": "updater"})
	u.emit(wsclient.EvtUpdateStarted, map[string]string{"target": "emly"})
	var sent []string
	u.flushEvents(func(name string, _ any) error { sent = append(sent, name); return nil })
	if len(sent) != 2 || sent[0] != wsclient.EvtUpdateApplied {
		t.Fatalf("flushed %v", sent)
	}
	u.flushEvents(func(name string, _ any) error { t.Fatal("buffer not emptied"); return nil })
}

// session.changed is the one event that is never buffered (see
// TestSessionChangedNotBufferedWithoutSession) - this uses update.applied
// instead so the buffer-eviction behaviour it actually tests is unaffected.
func TestEventBufferDropsOldest(t *testing.T) {
	u := &Updater{Log: testLogger(t)}
	for i := 0; i < eventBufferSize+5; i++ {
		u.emit(wsclient.EvtUpdateApplied, i)
	}
	var first any
	n := 0
	u.flushEvents(func(_ string, p any) error {
		if n == 0 {
			first = p
		}
		n++
		return nil
	})
	if n != eventBufferSize || first != 5 {
		t.Fatalf("n=%d first=%v", n, first)
	}
}

func TestSessionChangedPayload(t *testing.T) {
	at := time.Date(2026, 9, 23, 8, 20, 29, 450e6, time.UTC)
	c := machineinfo.SessionChange{Kind: machineinfo.SessionUnlock, SessionID: 2, At: at, SessionUser: `CORP\m.rossi`,
		LoggedUser: machineinfo.UserSession{User: `CORP\m.rossi`, State: machineinfo.SessionActiveRDP}}
	p := sessionChangedPayload([]machineinfo.SessionChangeKind{machineinfo.SessionRemoteConnect, machineinfo.SessionUnlock}, c, true)
	if len(p.Events) != 2 || p.Events[0] != "remote-connect" || p.SessionID != 2 || !p.Changed ||
		p.LoggedUser == nil || p.LoggedUser.State != "active-rdp" || p.At != "2026-09-23T08:20:29.450Z" {
		t.Fatalf("payload = %+v", p)
	}
	nobody := sessionChangedPayload(nil, machineinfo.SessionChange{Kind: machineinfo.SessionLogoff}, true)
	if nobody.LoggedUser != nil {
		t.Fatalf("nobody logged on must omit logged_user: %+v", nobody)
	}
}

func TestServiceStartedReason(t *testing.T) {
	boot := time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		cmds   []state.PendingCommand
		landed *state.SelfUpdate
		start  time.Time
		want   string
	}{
		{"restart command", []state.PendingCommand{{Name: wsclient.CmdServiceRestart}}, nil, boot.Add(time.Hour), "command"},
		{"reboot command", []state.PendingCommand{{Name: wsclient.CmdMachineReboot}}, nil, boot.Add(time.Minute), "boot"},
		{"self update", nil, &state.SelfUpdate{FromVersion: "1.8.2"}, boot.Add(time.Hour), "self_update"},
		{"fresh boot", nil, nil, boot.Add(3 * time.Minute), "boot"},
		{"plain restart", nil, nil, boot.Add(3 * time.Hour), "unknown"},
	}
	for _, c := range cases {
		if got := serviceStartedReason(c.cmds, c.landed, boot, c.start); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestMachineInfoSectionsFilter(t *testing.T) {
	u := newClientTestUpdater(t)
	p := u.machineInfo([]string{"network"})
	if p.Network == nil || p.Hardware != nil || p.HWID != "" || p.EMLy != nil {
		t.Fatalf("payload = %+v", p)
	}
}

// session.changed only anticipates what the next poll's
// X-EMLy-LoggedUser* headers would carry (spec §8.1) - stale by the time a
// channel reconnects, so it must be dropped rather than buffered while no
// session is up. Other events keep buffering.
func TestSessionChangedNotBufferedWithoutSession(t *testing.T) {
	u := &Updater{Log: testLogger(t)}
	u.emit(wsclient.EvtSessionChanged, map[string]string{"session_id": "1"})
	u.emit(wsclient.EvtUpdateApplied, map[string]string{"target": "emly"})

	u.eventsMu.Lock()
	buf := u.eventBuf
	u.eventsMu.Unlock()
	if len(buf) != 1 || buf[0].name != wsclient.EvtUpdateApplied {
		t.Fatalf("buffer = %+v, want only the non-session.changed event", buf)
	}
}

// flushEvents must drop an event that a live session refused as too large
// rather than keep it at the head of the buffer: retrying it can only ever
// reproduce the same failure, blocking every event queued behind it.
func TestFlushEventsDropsOversizedEvent(t *testing.T) {
	u := &Updater{Log: testLogger(t)}
	u.eventBuf = []bufferedEvent{
		{name: "too.big", payload: 1},
		{name: wsclient.EvtUpdateApplied, payload: 2},
	}
	var sent []string
	u.flushEvents(func(name string, _ any) error {
		if name == "too.big" {
			return wsclient.ErrTooLarge
		}
		sent = append(sent, name)
		return nil
	})
	if len(sent) != 1 || sent[0] != wsclient.EvtUpdateApplied {
		t.Fatalf("sent = %v, want only the event after the oversized one", sent)
	}
	u.eventsMu.Lock()
	n := len(u.eventBuf)
	u.eventsMu.Unlock()
	if n != 0 {
		t.Fatalf("buffer = %d entries, want the oversized event dropped rather than re-queued", n)
	}
}

// emit's live-session path must drop an oversized event the same way,
// rather than fall into the generic "connection going away" buffering path -
// otherwise a single event too large for the negotiated limit sits at the
// head of the buffer forever, blocking everything behind it.
func TestEmitDropsOversizedEventOnALiveSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		if err := wsjson.Write(ctx, conn, wsclient.Message{Type: wsclient.TypeHello}); err != nil {
			return
		}
		var msg wsclient.Message
		if err := wsjson.Read(ctx, conn, &msg); err != nil { // identity
			return
		}
		welcome, _ := json.Marshal(wsclient.Welcome{
			Protocol:             wsclient.ProtocolV2,
			AcceptedCapabilities: wsclient.EventNames,
			Limits:               wsclient.Limits{MaxMessageBytes: 200, AckTimeoutSeconds: 5},
		})
		if err := wsjson.Write(ctx, conn, wsclient.Message{Type: wsclient.TypeWelcome, Data: welcome}); err != nil {
			return
		}
		<-ctx.Done()
	}))
	defer srv.Close()

	u := clientWSUpdaterAt(t, srv.URL)
	u.Store = &state.Store{Path: filepath.Join(t.TempDir(), "state.json")}
	u.systemFactsFn = func(time.Time) machineinfo.SystemFacts { return machineinfo.SystemFacts{Cores: 4} }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); u.runClientWS(ctx) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(5 * time.Second)
	for u.wsSession.Load() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the presence channel never became live")
		}
		time.Sleep(10 * time.Millisecond)
	}

	u.emit(wsclient.EvtUpdateApplied, map[string]string{"data": strings.Repeat("x", 1000)})

	u.eventsMu.Lock()
	n := len(u.eventBuf)
	u.eventsMu.Unlock()
	if n != 0 {
		t.Fatalf("oversized event was buffered instead of dropped: %d entries", n)
	}
}

// welcomeBurst - like executeCommand and handleNotify's delayed wake - runs
// on its own goroutine for remote-triggered work: a panic there must be
// recovered and logged, not crash the process.
func TestWelcomeBurstRecoversFromPanic(t *testing.T) {
	u := &Updater{Log: testLogger(t)}
	u.welcomeBurstFn = func(uint64, *wsclient.Session) { panic("boom") }

	done := make(chan struct{})
	go func() {
		u.welcomeBurst(0, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("welcomeBurst did not return after a panicking seam - the panic was not recovered")
	}
}

// commandRefusalEventID is the pure predicate logCommandRefused
// (clientcmd.go) uses to decide whether a refusal mirrors to the Event Log:
// only a destructive command's refusal is consequential enough for Event
// Viewer (925) - a read-only refusal (policy, a busy winget call) stays in
// the file only.
func TestCommandRefusalEventID(t *testing.T) {
	for _, name := range []string{wsclient.CmdServiceRestart, wsclient.CmdMachineReboot} {
		if got := commandRefusalEventID(name); got != 925 {
			t.Errorf("commandRefusalEventID(%s) = %d, want 925", name, got)
		}
	}
	for _, name := range []string{wsclient.CmdMachineInfo, wsclient.CmdAppsListUpgradable, "machine.format_disk"} {
		if got := commandRefusalEventID(name); got != 0 {
			t.Errorf("commandRefusalEventID(%s) = %d, want 0 (file only)", name, got)
		}
	}
}

// newClientTestUpdater builds an Updater on the legacy policy (remote
// config off, see newTestUpdater), with every Windows-touching seam stubbed,
// a temp state file, EMLy not installed, and one cycle already run so
// u.cur is set.
func newClientTestUpdater(t *testing.T) *Updater {
	t.Helper()
	cfg := internalCfg(t, "internal")
	cfg.EMLyConfigFile = filepath.Join(t.TempDir(), "missing", "config.ini")
	u := newTestUpdater(t, cfg, dcNamed("DC-RM2"), ipsOf("172.16.96.50"))
	u.Store = &state.Store{Path: filepath.Join(t.TempDir(), "state.json")}
	u.loggedUserFn = func() machineinfo.UserSession { return machineinfo.UserSession{} }
	u.systemFactsFn = func(time.Time) machineinfo.SystemFacts { return machineinfo.SystemFacts{Cores: 4} }
	u.netInterfacesFn = func() []machineinfo.NetInterface { return []machineinfo.NetInterface{{Name: "Ethernet"}} }
	u.emlyRunningFn = func() bool { return false }
	u.startedAt = time.Now()
	u.cur.Store(u.beginCycle(context.Background(), false))
	return u
}
