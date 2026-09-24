package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

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

func TestEventBufferDropsOldest(t *testing.T) {
	u := &Updater{Log: testLogger(t)}
	for i := 0; i < eventBufferSize+5; i++ {
		u.emit(wsclient.EvtSessionChanged, i)
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
