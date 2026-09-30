package notify

import "emlyupdater/internal/machineinfo"

// viewerSession returns the WTS session in which a user can see what this
// package shows - a toast, the progress window, a message box - and whether
// there is one.
//
// It is not WTSGetActiveConsoleSessionId: that only ever names the physical
// console, and a user connected over RDP is in a session of their own while
// the console sits at the login screen - so everything sent there was either
// dropped (no user token) or drawn where nobody was looking. The session is
// picked the way the X-EMLy-LoggedUser header picks it
// (machineinfo.InteractiveSession): the active console session when someone
// is logged on there, otherwise an active RDP session.
//
// A disconnected session is not a viewer: its user is still logged on but no
// client is attached, so nothing drawn there is seen. Callers treat "no
// viewer" as they always treated "nobody at the console" - the notification
// is skipped, and a critical update proceeds without its warning.
func viewerSession() (uint32, bool) {
	id, us, ok := machineinfo.InteractiveSession()
	if !ok || us.State == machineinfo.SessionDisconnected {
		return 0, false
	}
	return id, true
}
