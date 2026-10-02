package service

import (
	"time"

	"golang.org/x/sys/windows/svc"
)

// CheckNowControl is the user-defined service control code (128-255) the
// tray sends to ask for an update cycle right now. A control code and not an
// IPC request: the pipe only admits EMLy.exe, and the service's default
// security descriptor already grants SERVICE_USER_DEFINED_CONTROL to
// interactive users (IU: CR), so a non-elevated tray can send it with no
// change to the pipe, its proto or the service's DACL. It carries no data -
// which is the point: there is nothing in it to validate.
const CheckNowControl = svc.Cmd(200)

// checkNowWake is the wake reason of a cycle the tray asked for. Like
// productExitWake it is reported to the server as an ordinary "cycle".
const checkNowWake = "check-now"

// checkNowThrottle bounds how often the tray may wake the loop. One machine,
// one user: this is not about the fleet (the server paces downloads with its
// 429s regardless) but about a script or a stuck button turning the poll loop
// into a busy loop.
const checkNowThrottle = time.Minute

// RequestCheck wakes RunLoop for an immediate cycle, which also fetches the
// remote configuration unconditionally. Called from the SCM control handler,
// so it never blocks: a wake already pending absorbs this one.
func (u *Updater) RequestCheck() {
	now := u.clock()
	u.notifyWakeMu.Lock()
	if !u.lastCheckNow.IsZero() && now.Sub(u.lastCheckNow) < checkNowThrottle {
		u.notifyWakeMu.Unlock()
		u.Log.Info("update check requested from the tray, throttled", "throttle", checkNowThrottle.String())
		return
	}
	u.lastCheckNow = now
	u.notifyWakeMu.Unlock()

	u.forceConfig.Store(true)
	u.Log.Info("update check requested from the tray, waking the update loop")
	select {
	case u.wake <- checkNowWake:
	default: // a wake is already pending: that cycle is the check
	}
}
