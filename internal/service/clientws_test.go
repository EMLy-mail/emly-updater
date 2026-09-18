package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"emlyupdater/internal/config"
	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/policy"
	"emlyupdater/internal/wsclient"
)

// clientWSUpdater builds an Updater whose current cycle points at one
// server, with the presence channel enabled or not.
//
// The watch interval and the initial-dial jitter are both shortened here so
// that every test built on top of this helper runs the supervisor without
// the wall-clock sleeps (up to 15s between policy re-reads, up to 60s before
// the first dial) production uses - see clientWSWatch and
// clientWSInitialDelayFn on Updater.
func clientWSUpdater(t *testing.T, enabled bool) *Updater {
	t.Helper()
	cfg := internalCfg(t, config.SourceExternal)
	u := newTestUpdater(t, cfg, dcNamed("DC-RM2"), ipsOf("172.16.96.10"))
	u.clientWSWatch = 100 * time.Millisecond
	u.clientWSInitialDelayFn = func() time.Duration { return 0 }
	snap := u.Policy.Current()
	snap.Parsed.Global.ClientWS = policy.Toggle{Enabled: enabled}
	host := policy.Host{HWID: "HW-1", Hostname: "RM095", DC: "DC-RM2",
		IPs: []string{"172.16.96.10"}, Now: time.Now()}
	eff, err := snap.Parsed.Effective(host)
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	site, chain := eff.Chain(host)
	u.cur.Store(&cycleState{snap: snap, host: host, eff: eff, site: site, chain: chain})
	return u
}

// The channel follows the same server the rest of the cycle uses: the first
// entry of the chain beginCycle built, not a separately configured address.
func TestClientWSTargetFollowsTheCycleChain(t *testing.T) {
	u := clientWSUpdater(t, true)
	cyc := u.cur.Load()

	got := u.clientWSTarget()
	if !got.enabled {
		t.Fatal("target is disabled for a document that enables the channel")
	}
	if got.server != cyc.chain[0] {
		t.Errorf("server = %q, want the head of the chain %q", got.server, cyc.chain[0])
	}
	want := "ws://172.16.96.73:8080/v2/client/ws"
	if got.url != want {
		t.Errorf("url = %q, want %q", got.url, want)
	}
}

// Off by default: the kill switch is what opens the channel, and nothing
// else. A machine whose document never mentions clientWs connects to
// nothing, however reachable its server is.
func TestClientWSTargetIsDisabledWithoutTheKillSwitch(t *testing.T) {
	u := clientWSUpdater(t, false)
	if got := u.clientWSTarget(); got.enabled {
		t.Errorf("target = %+v, want disabled", got)
	}
}

// Before the first beginCycle there is nothing to connect to. The supervisor
// starts right after it in RunLoop, but a nil cycle must produce a disabled
// target rather than a panic.
func TestClientWSTargetBeforeTheFirstCycle(t *testing.T) {
	u := clientWSUpdater(t, true)
	u.cur.Store(nil)
	if got := u.clientWSTarget(); got.enabled {
		t.Errorf("target = %+v before any cycle, want disabled", got)
	}
}

// The identity goes through the same resolution the X-EMLy-* headers do, so
// the two paths cannot report different things about the same machine.
func TestClientWSIdentityMirrorsTheHeaders(t *testing.T) {
	u := clientWSUpdater(t, true)
	u.Machine = machineinfo.Info{
		Hostname: "RM095", HWID: "HW-1", ADDomain: "tregcc.local",
		OSVersion: "Windows 11 24H2", Serial: "SN-9", Product: "OptiPlex",
		InternalIP: "172.16.96.10",
	}
	u.loggedUserFn = func() machineinfo.UserSession {
		return machineinfo.UserSession{User: `TREGCC\mrossi`, State: "active-console"}
	}

	id := u.clientWSIdentity()
	if id.Hostname != "RM095" || id.HWID != "HW-1" || id.ADDomain != "tregcc.local" {
		t.Errorf("machine facts missing from the identity: %+v", id)
	}
	if id.OSVersion != "Windows 11 24H2" || id.Serial != "SN-9" || id.Product != "OptiPlex" {
		t.Errorf("firmware/OS facts missing from the identity: %+v", id)
	}
	if id.LoggedUser != `TREGCC\mrossi` || id.LoggedUserState != "active-console" {
		t.Errorf("logged-on user missing from the identity: %+v", id)
	}
	if id.LoggedUserDisconnectedAt != "" {
		t.Errorf("logged_user_disconnected_at = %q for a connected session, want empty", id.LoggedUserDisconnectedAt)
	}
}

// A disconnected session reports when it lost its client, RFC 3339 in UTC -
// the same format and the same meaning as the X-EMLy-LoggedUserDisconnectedAt
// header.
func TestClientWSIdentityReportsADisconnectedSession(t *testing.T) {
	u := clientWSUpdater(t, true)
	when := time.Date(2026, 9, 18, 8, 12, 0, 0, time.UTC)
	u.loggedUserFn = func() machineinfo.UserSession {
		return machineinfo.UserSession{User: `TREGCC\mrossi`, State: "disconnected", DisconnectedAt: when}
	}

	id := u.clientWSIdentity()
	if id.LoggedUserDisconnectedAt != "2026-09-18T08:12:00Z" {
		t.Errorf("logged_user_disconnected_at = %q, want %q", id.LoggedUserDisconnectedAt, "2026-09-18T08:12:00Z")
	}
}

// clientWSUpdaterWithServer is clientWSUpdater pointed at a named server: the
// document names exactly this one server as the default (no DC/subnet match
// possible, so beginCycle always falls through to it), and the presence
// channel is on.
func clientWSUpdaterWithServer(t *testing.T, name, baseURL string) *Updater {
	t.Helper()
	u := clientWSUpdater(t, true)
	snap := u.Policy.Current()
	snap.Parsed.Global.Servers = map[string]string{name: baseURL}
	snap.Parsed.Global.DefaultServer = name
	snap.Parsed.Global.DCLookupMap = map[string]policy.Site{}
	snap.Parsed.Global.ClientWS = policy.Toggle{Enabled: true}
	storeCycle(t, u, snap)
	u.Machine = machineinfo.Info{Hostname: "RM095", HWID: "HW-1"}
	u.loggedUserFn = func() machineinfo.UserSession { return machineinfo.UserSession{} }
	return u
}

// clientWSUpdaterAt is clientWSUpdaterWithServer under the fixed name "test"
// - the shape almost every supervisor test wants: one server, the whole
// chain.
func clientWSUpdaterAt(t *testing.T, baseURL string) *Updater {
	t.Helper()
	return clientWSUpdaterWithServer(t, "test", baseURL)
}

// setClientWSServer publishes a cycle whose default (and only) server is
// name→baseURL, the way beginCycle picking a different server - a site or
// subnet change - moves the chain the supervisor reads. Built the same
// aliasing-safe way disableClientWS is (see its comment): a fresh
// Document/Parsed/Snapshot rather than a mutation in place, because
// Effective() with no override applied aliases eff.Doc directly to
// Parsed.Global.
func setClientWSServer(t *testing.T, u *Updater, name, baseURL string) {
	t.Helper()
	cur := u.Policy.Current()
	doc := *cur.Parsed.Global
	doc.Servers = map[string]string{name: baseURL}
	doc.DefaultServer = name
	parsed := *cur.Parsed
	parsed.Global = &doc
	snap := *cur
	snap.Parsed = &parsed
	storeCycle(t, u, &snap)
}

// disableClientWS swaps in a cycle whose document has the channel off, the
// way accepting a new revision would.
//
// It builds a fresh Document/Parsed/Snapshot rather than mutating the
// ones u.Policy.Current() returns in place: Effective() with no override
// applied aliases eff.Doc directly to Parsed.Global (see policy/match.go),
// so an in-place write here would race with the supervisor goroutine's
// concurrent read of cyc.eff.Doc.ClientWS.Enabled in clientWSTarget - the
// same aliasing that lets Snapshot promise "one immutable policy state".
func disableClientWS(t *testing.T, u *Updater) {
	t.Helper()
	cur := u.Policy.Current()
	doc := *cur.Parsed.Global
	doc.ClientWS = policy.Toggle{Enabled: false}
	parsed := *cur.Parsed
	parsed.Global = &doc
	snap := *cur
	snap.Parsed = &parsed
	storeCycle(t, u, &snap)
}

// storeCycle re-evaluates snap for a fixed host and publishes the result as
// the current cycle, which is all the supervisor reads.
func storeCycle(t *testing.T, u *Updater, snap *policy.Snapshot) {
	t.Helper()
	host := policy.Host{HWID: "HW-1", Hostname: "RM095", Now: time.Now()}
	eff, err := snap.Parsed.Effective(host)
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	site, chain := eff.Chain(host)
	u.Policy.Set(snap)
	u.cur.Store(&cycleState{snap: snap, host: host, eff: eff, site: site, chain: chain})
}

// A whole life cycle against a real endpoint: the supervisor connects,
// identifies itself, answers a ping, and comes back when the service stops.
func TestRunClientWSConnectsAndStopsWithTheContext(t *testing.T) {
	identities := make(chan wsclient.Identity, 1)
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
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}
		var id wsclient.Identity
		if err := json.Unmarshal(msg.Data, &id); err == nil {
			select {
			case identities <- id:
			default:
			}
		}
		<-ctx.Done()
	}))
	defer srv.Close()

	u := clientWSUpdaterAt(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); u.runClientWS(ctx) }()

	select {
	case id := <-identities:
		if id.HWID != "HW-1" {
			t.Errorf("identity = %+v", id)
		}
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("the supervisor never opened a connection")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runClientWS did not return after the context was cancelled")
	}
}

// A server that answers 404 has said it does not serve this endpoint. The
// supervisor stops asking it - retrying on a backoff forever would put an
// error in every machine of that site's log until the mirror is upgraded.
func TestRunClientWSStopsAskingAServerThatAnswers404(t *testing.T) {
	var upgrades atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrades.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	u := clientWSUpdaterAt(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); u.runClientWS(ctx) }()

	// This does not prove anything about the backoff schedule - the memo
	// bypasses it entirely, and the schedule is jittered besides. What it
	// catches is the no-memo regression: without unsupported recording the
	// 404, the loop has nothing to wait on between attempts (the 404 branch
	// never reaches clientWSIdle) and would busy-loop dialling again
	// immediately, many times over, well within this window.
	time.Sleep(3 * time.Second)
	cancel()
	<-done

	if got := upgrades.Load(); got != 1 {
		t.Errorf("the endpoint was dialled %d times after a 404, want exactly 1", got)
	}
}

// Turning the kill switch off mid-flight closes the connection rather than
// waiting for the next restart: a site that decides the channel is costing
// it something must be able to stop it by publishing a revision.
func TestRunClientWSStandsDownWhenTheDocumentDisablesIt(t *testing.T) {
	connected := make(chan struct{}, 1)
	closed := make(chan struct{}, 1)
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
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}
		select {
		case connected <- struct{}{}:
		default:
		}
		// Block until the client goes away, then say so.
		var next wsclient.Message
		_ = wsjson.Read(ctx, conn, &next)
		select {
		case closed <- struct{}{}:
		default:
		}
	}))
	defer srv.Close()

	u := clientWSUpdaterAt(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); u.runClientWS(ctx) }()

	// Wait for the connection itself (not a fixed sleep) before turning the
	// switch off the way a new revision would: a fresh cycle state with the
	// section disabled.
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("the supervisor never connected")
	}
	disableClientWS(t, u)

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("the connection was not closed after the document disabled the channel")
	}
	cancel()
	<-done
}

// The supervisor reconnects after a connection that was really established
// drops: the server accepts, completes the handshake, then hangs up. This is
// the ordinary "a machine dropped and comes back" path an operator's mental
// model depends on, distinct from the 404/disabled cases above which never
// reach a real connection at all.
func TestRunClientWSReconnectsAfterADrop(t *testing.T) {
	var connects atomic.Int32
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
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}
		connects.Add(1)
		// Hang up right after the handshake: a connection that was really
		// established, then lost.
		_ = conn.Close(websocket.StatusNormalClosure, "bye")
	}))
	defer srv.Close()

	u := clientWSUpdaterAt(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); u.runClientWS(ctx) }()

	waitForAtLeast(t, &connects, 2, 15*time.Second,
		"the supervisor did not reconnect after the connection dropped")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runClientWS did not return after the context was cancelled")
	}
}

// A connection up against server A must be closed, and a new one opened
// against server B, when the source policy moves this machine's chain to a
// different server - the same way a site/subnet change moves the manifest
// poll's chain. Only the kill-switch branch of the watcher
// (TestRunClientWSStandsDownWhenTheDocumentDisablesIt) was covered before
// this.
func TestRunClientWSFollowsTheWatcherToADifferentServer(t *testing.T) {
	connectedA := make(chan struct{}, 1)
	closedA := make(chan struct{}, 1)
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}
		select {
		case connectedA <- struct{}{}:
		default:
		}
		var next wsclient.Message
		_ = wsjson.Read(ctx, conn, &next)
		select {
		case closedA <- struct{}{}:
		default:
		}
	}))
	defer srvA.Close()

	connectedB := make(chan struct{}, 1)
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}
		select {
		case connectedB <- struct{}{}:
		default:
		}
		<-ctx.Done()
	}))
	defer srvB.Close()

	u := clientWSUpdaterWithServer(t, "a", srvA.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); u.runClientWS(ctx) }()

	select {
	case <-connectedA:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("the supervisor never connected to server A")
	}

	setClientWSServer(t, u, "b", srvB.URL)

	select {
	case <-closedA:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("the connection to server A was not closed after the policy moved to server B")
	}
	select {
	case <-connectedB:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("the supervisor never connected to server B")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		// Budget must exceed coder/websocket's ~5s close handshake against
		// a server that stops reading, else the test races the library's own wait.
		t.Fatal("runClientWS did not return after the context was cancelled")
	}
}

// A server marked unsupported is retried on its own once
// clientWSUnsupportedRetryAfter elapses, without waiting for the source
// policy to move this machine elsewhere: an ingress or load balancer that
// answered 404 for a few seconds during an API deploy must not blind the
// machine until its service next restarts. Exercised through the clock seam
// rather than a real hour of wall-clock time.
func TestRunClientWSRetriesA404dServerAfterTheMarkExpires(t *testing.T) {
	var upgrades atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrades.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	u := clientWSUpdaterAt(t, srv.URL)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var now atomic.Pointer[time.Time]
	now.Store(&start)
	u.nowFn = func() time.Time { return *now.Load() }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); u.runClientWS(ctx) }()

	waitForAtLeast(t, &upgrades, 1, 10*time.Second, "the server was never dialled once")

	// A little short of the retry window: still marked, must not retry yet.
	almostExpired := start.Add(clientWSUnsupportedRetryAfter - time.Second)
	now.Store(&almostExpired)
	time.Sleep(5 * u.watchInterval())
	if got := upgrades.Load(); got != 1 {
		t.Fatalf("upgrades = %d before the mark expired, want 1 (retried too early)", got)
	}

	expired := start.Add(clientWSUnsupportedRetryAfter + time.Second)
	now.Store(&expired)
	waitForAtLeast(t, &upgrades, 2, 10*time.Second, "the server was not retried after the mark expired")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runClientWS did not return after the context was cancelled")
	}
}

// waitForAtLeast polls counter until it reaches want or timeout elapses.
func waitForAtLeast(t *testing.T, counter *atomic.Int32, want int32, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if counter.Load() >= want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("%s (got %d, want %d)", msg, counter.Load(), want)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
