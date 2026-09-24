package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/websocket"

	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/state"
	"emlyupdater/internal/wsclient"
)

func TestDestructiveRefusedOnInsecureTransport(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdMachineReboot)
	rebooted := false
	u.rebootFn = func(time.Duration) error { rebooted = true; return nil }
	s := &fakeSession{secure: false}
	msg, cmd := cmdMsg(wsclient.CmdMachineReboot, `{}`)
	u.executeCommand(context.Background(), s, msg, cmd)
	cmds, _ := u.Store.TakePendingCommands()
	if rebooted || len(cmds) != 0 || s.acks[0].Error.Code != wsclient.ErrInsecureTransport {
		t.Fatalf("rebooted=%v pending=%v acks=%+v", rebooted, cmds, s.acks)
	}
}

func TestRebootPersistsIDAndRaisesDelayForActiveUser(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdMachineReboot)
	u.loggedUserFn = func() machineinfo.UserSession {
		return machineinfo.UserSession{User: `CORP\u`, State: machineinfo.SessionActiveConsole}
	}
	var got time.Duration
	u.rebootFn = func(d time.Duration) error { got = d; return nil }
	s := &fakeSession{secure: true}
	msg, cmd := cmdMsg(wsclient.CmdMachineReboot, `{"delay_seconds":10}`)
	u.executeCommand(context.Background(), s, msg, cmd)
	cmds, _ := u.Store.TakePendingCommands()
	if got != 60*time.Second || len(cmds) != 1 || cmds[0].ID != msg.ID || len(s.results) != 0 {
		t.Fatalf("delay=%v pending=%+v results=%+v", got, cmds, s.results)
	}
}

func TestRebootSkipWithActiveUserRefused(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdMachineReboot)
	u.loggedUserFn = func() machineinfo.UserSession {
		return machineinfo.UserSession{User: `CORP\u`, State: machineinfo.SessionActiveRDP}
	}
	u.rebootFn = func(time.Duration) error { t.Fatal("rebooted"); return nil }
	s := &fakeSession{secure: true}
	msg, cmd := cmdMsg(wsclient.CmdMachineReboot, `{"when_user_active":"skip"}`)
	u.executeCommand(context.Background(), s, msg, cmd)
	if s.acks[0].Accepted || s.acks[0].Error.Code != wsclient.ErrUserActive {
		t.Fatalf("acks = %+v", s.acks)
	}
}

func TestRebootFailureUnrecordsAndReportsError(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdMachineReboot)
	u.rebootFn = func(time.Duration) error { return errors.New("privilege not held") }
	s := &fakeSession{secure: true}
	msg, cmd := cmdMsg(wsclient.CmdMachineReboot, `{}`)
	u.executeCommand(context.Background(), s, msg, cmd)
	cmds, _ := u.Store.TakePendingCommands()
	if len(cmds) != 0 || len(s.results) != 1 || s.results[0].Error.Code != wsclient.ErrInternal {
		t.Fatalf("pending=%+v results=%+v", cmds, s.results)
	}
}

func TestServiceRestartClosesThenRestarts(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdServiceRestart)
	var closedFirst bool
	s := &fakeSession{secure: true}
	u.restartFn = func() error { closedFirst = s.closed == websocket.StatusGoingAway; return nil }
	msg, cmd := cmdMsg(wsclient.CmdServiceRestart, ``)
	u.executeCommand(context.Background(), s, msg, cmd)
	cmds, _ := u.Store.TakePendingCommands()
	if !closedFirst || len(cmds) != 1 || cmds[0].Name != wsclient.CmdServiceRestart {
		t.Fatalf("closedFirst=%v pending=%+v", closedFirst, cmds)
	}
}

func TestDestructiveBusyWhileInstalling(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdServiceRestart)
	u.installing.Add(1)
	u.restartFn = func() error { t.Fatal("restarted mid-install"); return nil }
	s := &fakeSession{secure: true}
	msg, cmd := cmdMsg(wsclient.CmdServiceRestart, ``)
	u.executeCommand(context.Background(), s, msg, cmd)
	if s.acks[0].Error.Code != wsclient.ErrBusy {
		t.Fatalf("acks = %+v", s.acks)
	}
}

// A failed restartFn must not leave the pending record (and the
// destructivePending flag it set) behind: the reboot/restart never
// actually happened, so nothing here should keep blocking further
// destructive commands or installs.
func TestServiceRestartFailureRemovesPendingID(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdServiceRestart)
	u.restartFn = func() error { return errors.New("launch failed") }
	s := &fakeSession{secure: true}
	msg, cmd := cmdMsg(wsclient.CmdServiceRestart, ``)
	u.executeCommand(context.Background(), s, msg, cmd)
	cmds, _ := u.Store.TakePendingCommands()
	if len(cmds) != 0 {
		t.Fatalf("pending = %+v, want empty after a failed restart launch", cmds)
	}
	if u.destructivePendingNow() {
		t.Fatal("destructivePending must be cleared after a failed restart launch")
	}
}

// No delay_seconds and nobody logged on: the default (300s) is used
// unmodified - there is no active user to raise it for.
func TestRebootDefaultDelayNoUser(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdMachineReboot)
	u.loggedUserFn = func() machineinfo.UserSession { return machineinfo.UserSession{} }
	var got time.Duration
	u.rebootFn = func(d time.Duration) error { got = d; return nil }
	s := &fakeSession{secure: true}
	msg, cmd := cmdMsg(wsclient.CmdMachineReboot, `{}`)
	u.executeCommand(context.Background(), s, msg, cmd)
	want := time.Duration(wsclient.RebootDefaultDelaySeconds) * time.Second
	if got != want {
		t.Fatalf("delay = %v, want default %v", got, want)
	}
}

// A disconnected session is still "the logged user" for inventory purposes
// (machineinfo's convention), but it is not somebody actually at the
// machine right now - so it must not raise the requested delay the way an
// active console/RDP session does.
func TestRebootDisconnectedUserNoDelayRaise(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdMachineReboot)
	u.loggedUserFn = func() machineinfo.UserSession {
		return machineinfo.UserSession{User: `CORP\u`, State: machineinfo.SessionDisconnected}
	}
	var got time.Duration
	u.rebootFn = func(d time.Duration) error { got = d; return nil }
	s := &fakeSession{secure: true}
	msg, cmd := cmdMsg(wsclient.CmdMachineReboot, `{"delay_seconds":10}`)
	u.executeCommand(context.Background(), s, msg, cmd)
	if got != 10*time.Second {
		t.Fatalf("delay = %v, want unchanged 10s for a disconnected user", got)
	}
}

// Once one destructive command is committed (reboot scheduled), a second
// one of a different name must be refused busy too - not just a duplicate
// of the same command, which the dedupe ring already handles.
func TestSecondDestructiveCommandRefusedBusy(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdMachineReboot, wsclient.CmdServiceRestart)
	u.rebootFn = func(time.Duration) error { return nil }
	s1 := &fakeSession{secure: true}
	msg1, cmd1 := cmdMsg(wsclient.CmdMachineReboot, `{}`)
	u.executeCommand(context.Background(), s1, msg1, cmd1)
	if !u.destructivePendingNow() {
		t.Fatal("destructivePending should be set after a committed reboot")
	}

	u.restartFn = func() error { t.Fatal("a second destructive command must not run"); return nil }
	s2 := &fakeSession{secure: true}
	msg2, cmd2 := cmdMsg(wsclient.CmdServiceRestart, ``)
	u.executeCommand(context.Background(), s2, msg2, cmd2)
	if s2.acks[0].Accepted || s2.acks[0].Error.Code != wsclient.ErrBusy {
		t.Fatalf("second command acks = %+v", s2.acks)
	}
}

// Once destructivePending auto-expires (the committed reboot never actually
// completed - see destructivePendingLocked), the command's own
// pendingCommands record must go with it. Left behind, the next start's
// service.started would read it back (buildServiceStarted,
// clientevents.go) and report the aborted reboot as a completed one, under
// reason "boot".
func TestDestructivePendingExpiryRemovesPendingCommandRecord(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdMachineReboot)
	u.rebootFn = func(time.Duration) error { return nil }

	now := time.Now()
	u.nowFn = func() time.Time { return now }

	s := &fakeSession{secure: true}
	msg, cmd := cmdMsg(wsclient.CmdMachineReboot, `{}`)
	u.executeCommand(context.Background(), s, msg, cmd)

	st, err := u.Store.Load()
	if err != nil || len(st.PendingCommands) != 1 || st.PendingCommands[0].ID != msg.ID {
		t.Fatalf("pending command not recorded before expiry: %+v, err=%v", st, err)
	}

	// Advance well past the reboot's auto-expiry deadline (default delay +
	// rebootGrace).
	now = now.Add(time.Hour)

	if u.destructivePendingNow() {
		t.Fatal("destructivePending should have auto-expired")
	}
	st, err = u.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.PendingCommands) != 0 {
		t.Fatalf("expired destructive command's pending record was not removed: %+v", st.PendingCommands)
	}
}

// seedCommandRing must recognise a pending command id left in state.json by
// a previous process, and must not consume it: TakePendingCommands (used
// later by service.started's welcome burst) still has to see the record.
func TestSeedCommandRingFromState(t *testing.T) {
	u := newClientTestUpdater(t)
	rec := state.PendingCommand{ID: "abc123", Name: wsclient.CmdMachineReboot, AcceptedAt: time.Now().UTC()}
	if err := u.Store.AddPendingCommand(rec); err != nil {
		t.Fatal(err)
	}

	u.seedCommandRing()

	if _, _, seen := u.commands.lookup("abc123"); !seen {
		t.Fatal("pending command id from state.json was not seeded into the dedupe ring")
	}

	cmds, err := u.Store.TakePendingCommands()
	if err != nil || len(cmds) != 1 || cmds[0].ID != "abc123" {
		t.Fatalf("seeding must not consume the pending command record, got %+v err=%v", cmds, err)
	}
}
