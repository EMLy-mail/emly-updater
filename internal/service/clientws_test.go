package service

import (
	"testing"
	"time"

	"emlyupdater/internal/config"
	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/policy"
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
