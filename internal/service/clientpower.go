package service

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sys/windows"

	"emlyupdater/internal/logging"
	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/power"
	"emlyupdater/internal/state"
	"emlyupdater/internal/wsclient"
)

const (
	// minRebootNoticeWithUser is the floor machine.reboot's delay is raised
	// to when somebody is actively using the console or an RDP session:
	// Windows shows its own countdown, but a shorter one is not enough
	// warning for a user who is actually there.
	minRebootNoticeWithUser = 60 * time.Second

	// rebootGrace is added to the reboot's own requested delay when
	// computing destructivePending's auto-expiry deadline: slack, beyond
	// InitiateSystemShutdownEx's own countdown, for the shutdown to
	// actually happen. If it is aborted (`shutdown /a`) or hangs, this is
	// what lets destructivePending - and the busy state it holds the
	// machine in - heal on its own instead of staying stuck for the rest
	// of the process's life.
	rebootGrace = 5 * time.Minute
	// serviceRestartGrace is destructivePending's auto-expiry deadline for
	// service.restart: cmdStop's own wait (up to 60s), the restart-service
	// child's retried cmdStart attempts, and a margin for process/SCM
	// scheduling. If the detached child dies, or the service never comes
	// back, this is what lets the flag heal the same way.
	serviceRestartGrace = 3 * time.Minute
)

func userActive(s machineinfo.UserSession) bool {
	return s.State == machineinfo.SessionActiveConsole || s.State == machineinfo.SessionActiveRDP
}

// admitDestructive is the destructive-verb half of admitCommand: busy while
// an install is running (self-update or EMLy's) or a previous destructive
// command has already been committed (a reboot/restart already scheduled -
// this also stops a second one arriving mid-countdown), and
// machine.reboot's when_user_active=skip refusing to interrupt somebody who
// is actually at the machine right now.
func (u *Updater) admitDestructive(cmd wsclient.Command) *wsclient.ErrorBody {
	if !wsclient.Destructive(cmd.Name) {
		return nil
	}
	if u.installing.Load() > 0 {
		return refuse(wsclient.ErrBusy, "an installation is in progress")
	}
	if u.destructivePendingNow() {
		return refuse(wsclient.ErrBusy, "a service.restart or machine.reboot is already pending")
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

// beginInstall claims installing for a setup that is about to run - EMLy's
// own (install, service.go) or this updater's own (applySelfUpdate,
// selfupdate.go) - refusing (false) when a destructive client command has
// already been committed. It shares destructiveMu with commitDestructive
// below, so the two cannot interleave: an install cannot start in the gap
// between admitDestructive's busy check and runDestructive actually
// committing to the reboot/restart. reason is only used for the Info log
// explaining a skip.
func (u *Updater) beginInstall(reason string) bool {
	u.destructiveMu.Lock()
	defer u.destructiveMu.Unlock()
	if u.destructivePendingLocked() {
		u.Log.Info("skipping "+reason+": a destructive client command (service.restart/machine.reboot) is pending")
		return false
	}
	u.installing.Add(1)
	return true
}

// endInstall releases what beginInstall claimed. Not called on
// applySelfUpdate's success path - the service is about to stop, so there
// is nothing left to un-mark; see installing's doc comment on Updater.
func (u *Updater) endInstall() {
	u.installing.Add(-1)
}

// commitDestructive is runDestructive's last check before actually
// restarting the service or rebooting the machine: under the same lock
// beginInstall uses, it re-verifies that no install has started since this
// command was admitted and, if none has, sets destructivePending (with the
// given auto-expiry deadline - see rebootGrace/serviceRestartGrace) so
// beginInstall (and a second destructive command reaching
// admitDestructive) both see it immediately. False means an install
// slipped into the gap between admission and here; the caller must treat
// that exactly like restartFn/rebootFn itself failing.
func (u *Updater) commitDestructive(deadline time.Time) bool {
	u.destructiveMu.Lock()
	defer u.destructiveMu.Unlock()
	if u.installing.Load() > 0 {
		return false
	}
	u.destructivePending = true
	u.destructiveDeadline = deadline
	u.destructiveSkipLogged = false
	return true
}

// clearDestructivePending undoes commitDestructive after restartFn/rebootFn
// itself (or the late installing race commitDestructive itself catches)
// failed - the reboot/restart never actually happened, so nothing should
// keep refusing further commands or installs as busy.
func (u *Updater) clearDestructivePending() {
	u.destructiveMu.Lock()
	u.clearDestructivePendingLocked()
	u.destructiveMu.Unlock()
}

// clearDestructivePendingLocked must be called with destructiveMu held.
// Shared by clearDestructivePending (an explicit failure) and
// destructivePendingLocked (an expired deadline) so both reset the same
// three fields together - in particular destructiveSkipLogged, so the next
// episode logs its own "skipping this cycle" line instead of staying
// silent because a previous, now-irrelevant episode already logged once.
func (u *Updater) clearDestructivePendingLocked() {
	u.destructivePending = false
	u.destructiveDeadline = time.Time{}
	u.destructiveSkipLogged = false
}

// destructivePendingLocked must be called with destructiveMu held. It is
// the single place that enforces destructiveDeadline: once the deadline has
// passed, the committed reboot/restart is assumed to have failed silently
// (an aborted shutdown, a restart-service child that died or a service that
// never came back up) and the flag is released so the machine is not stuck
// refusing every cycle and every new destructive command forever. This is
// checked lazily, on whichever caller happens to read the flag next -
// there is no background timer - and logged (Warn + Event Log) exactly
// once for the transition, not on every read after it.
func (u *Updater) destructivePendingLocked() bool {
	if u.destructivePending && !u.destructiveDeadline.IsZero() && !u.clock().Before(u.destructiveDeadline) {
		deadline := u.destructiveDeadline
		u.clearDestructivePendingLocked()
		u.Log.WarnEvent(logging.EventClientCommandExpired,
			"a committed service.restart/machine.reboot did not complete by its deadline, resuming normal operation",
			"deadline", deadline.UTC().Format(time.RFC3339))
	}
	return u.destructivePending
}

// destructivePendingNow reports whether a service.restart or machine.reboot
// has been committed for this process (see commitDestructive), applying
// destructiveDeadline's auto-expiry. Cycle uses it to skip self-update and
// the EMLy update path entirely while a countdown or the detached
// restart-service launch is in flight.
func (u *Updater) destructivePendingNow() bool {
	u.destructiveMu.Lock()
	defer u.destructiveMu.Unlock()
	return u.destructivePendingLocked()
}

// logDestructiveSkipOnce logs Cycle's "skipping this cycle" line at most
// once per destructivePending episode (reset by clearDestructivePendingLocked
// whenever the flag clears, explicitly or by expiry) - a reboot's countdown
// can span several poll intervals, and the point of the message is to
// explain the first skip, not to repeat on every one of them.
func (u *Updater) logDestructiveSkipOnce() {
	u.destructiveMu.Lock()
	already := u.destructiveSkipLogged
	u.destructiveSkipLogged = true
	u.destructiveMu.Unlock()
	if !already {
		u.Log.Info("skipping cycles: a destructive client command (service.restart/machine.reboot) is pending")
	}
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
		if !u.commitDestructive(u.clock().Add(serviceRestartGrace)) {
			// An install started in the narrow gap between admission and
			// here. Same handling as restartFn itself failing below: the
			// connection is already closed, nobody is left to send a
			// Result to.
			u.Log.Error("service restart aborted: an install started after admission", "id", msg.ID)
			_ = u.Store.RemovePendingCommand(msg.ID)
			return
		}
		if err := u.restart(); err != nil {
			u.clearDestructivePending()
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
		if !u.commitDestructive(u.clock().Add(delay + rebootGrace)) {
			fail(errors.New("an install started after admission"))
			return
		}
		if err := u.reboot(delay); err != nil {
			u.clearDestructivePending()
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
