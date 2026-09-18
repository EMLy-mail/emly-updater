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
func clientWSUpdater(t *testing.T, enabled bool) *Updater {
	t.Helper()
	cfg := internalCfg(t, config.SourceExternal)
	u := newTestUpdater(t, cfg, dcNamed("DC-RM2"), ipsOf("172.16.96.10"))
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

// clientWSUpdaterAt is clientWSUpdater pointed at a test server: the
// document names one server, that server is the whole chain, and the
// presence channel is on.
func clientWSUpdaterAt(t *testing.T, baseURL string) *Updater {
	t.Helper()
	u := clientWSUpdater(t, true)
	snap := u.Policy.Current()
	snap.Parsed.Global.Servers = map[string]string{"test": baseURL}
	snap.Parsed.Global.DefaultServer = "test"
	snap.Parsed.Global.DCLookupMap = map[string]policy.Site{}
	snap.Parsed.Global.ClientWS = policy.Toggle{Enabled: true}
	storeCycle(t, u, snap)
	u.Machine = machineinfo.Info{Hostname: "RM095", HWID: "HW-1"}
	u.loggedUserFn = func() machineinfo.UserSession { return machineinfo.UserSession{} }
	return u
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

	// Long enough that a supervisor which kept retrying on the default 5s
	// base backoff would have dialled again at least once.
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

	// Give it a connection, then turn the switch off the way a new revision
	// would: a fresh cycle state with the section disabled.
	time.Sleep(500 * time.Millisecond)
	disableClientWS(t, u)

	select {
	case <-closed:
	case <-time.After(3 * clientWSWatchInterval):
		cancel()
		t.Fatal("the connection was not closed after the document disabled the channel")
	}
	cancel()
	<-done
}
