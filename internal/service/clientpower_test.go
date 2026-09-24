package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/websocket"

	"emlyupdater/internal/machineinfo"
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
