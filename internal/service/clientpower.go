package service

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sys/windows"

	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/power"
	"emlyupdater/internal/state"
	"emlyupdater/internal/wsclient"
)

// minRebootNoticeWithUser is the floor machine.reboot's delay is raised to
// when somebody is actively using the console or an RDP session: Windows
// shows its own countdown, but a shorter one is not enough warning for a
// user who is actually there.
const minRebootNoticeWithUser = 60 * time.Second

func userActive(s machineinfo.UserSession) bool {
	return s.State == machineinfo.SessionActiveConsole || s.State == machineinfo.SessionActiveRDP
}

// admitDestructive is the destructive-verb half of admitCommand: busy while
// an install is running (self-update or EMLy's), and machine.reboot's
// when_user_active=skip refusing to interrupt somebody who is actually at
// the machine right now.
func (u *Updater) admitDestructive(cmd wsclient.Command) *wsclient.ErrorBody {
	if !wsclient.Destructive(cmd.Name) {
		return nil
	}
	if u.installing.Load() > 0 {
		return refuse(wsclient.ErrBusy, "an installation is in progress")
	}
	if cmd.Name == wsclient.CmdMachineReboot {
		var a wsclient.RebootArgs
		_ = wsclient.DecodeArgs(cmd.Args, &a)
		if a.Mode() == wsclient.WhenUserActiveSkip && userActive(u.loggedUser()) {
			return refuse(wsclient.ErrUserActive, "a user is logged on and when_user_active is skip")
		}
	}
	return nil
}

// runDestructive executes service.restart or machine.reboot after
// admission and Ack have already happened (executeCommand). The command's
// ID is persisted to state.json before acting - the connection this Ack
// travelled on may not survive the action itself, so the pending record is
// the only durable trace of what was asked and when.
func (u *Updater) runDestructive(ctx context.Context, s commandSession, msg wsclient.Message, cmd wsclient.Command) {
	rec := state.PendingCommand{ID: msg.ID, Name: cmd.Name, AcceptedAt: u.clock().UTC()}
	fail := func(err error) {
		u.Log.Error("client command failed", "name", cmd.Name, "id", msg.ID, "error", err.Error())
		_ = u.Store.RemovePendingCommand(msg.ID)
		_ = s.Result(ctx, msg.ID, wsclient.Result{Status: wsclient.ResultError, Error: refuse(wsclient.ErrInternal, err.Error())})
	}
	if err := u.Store.AddPendingCommand(rec); err != nil {
		fail(err)
		return
	}
	switch cmd.Name {
	case wsclient.CmdServiceRestart:
		// The connection is closed before restartFn runs: restartFn's first
		// act (via the detached restart-service subcommand) is to stop this
		// service, and the stop handler waits for RunLoop to return - by
		// which point nothing would be left to send a Result on anyway.
		s.Close(websocket.StatusGoingAway, "service restarting")
		if err := u.restart(); err != nil {
			// The connection is already closed: there is nobody left to
			// send a Result to. The server times the command out instead.
			u.Log.Error("service restart could not be launched", "id", msg.ID, "error", err.Error())
			_ = u.Store.RemovePendingCommand(msg.ID)
		}
	case wsclient.CmdMachineReboot:
		var a wsclient.RebootArgs
		_ = wsclient.DecodeArgs(cmd.Args, &a)
		delay := time.Duration(a.Delay()) * time.Second
		if userActive(u.loggedUser()) && delay < minRebootNoticeWithUser {
			delay = minRebootNoticeWithUser
		}
		if err := u.reboot(delay); err != nil {
			fail(err)
			return
		}
		// The connection stays open until Windows actually shuts the
		// service down at the end of the countdown - no Result to send yet.
		u.Log.Warn("machine reboot scheduled", "id", msg.ID, "delay", delay.String())
	}
}

func (u *Updater) reboot(d time.Duration) error {
	if u.rebootFn != nil {
		return u.rebootFn(d)
	}
	return power.Reboot(d)
}

// restart launches `<this exe> restart-service` detached and never waits on
// it, for the same reason selfupdate.Launch never does: its first act is to
// stop this service, whose stop handler waits for RunLoop to return - a
// blocking launch here would deadlock the two.
func (u *Updater) restart() error {
	if u.restartFn != nil {
		return u.restartFn()
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "restart-service")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
