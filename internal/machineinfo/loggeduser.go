package machineinfo

import (
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	wtsapi32                       = windows.NewLazySystemDLL("wtsapi32.dll")
	procWTSQuerySessionInformation = wtsapi32.NewProc("WTSQuerySessionInformationW")
)

const (
	wtsCurrentServerHandle = 0

	// WTS_INFO_CLASS values we query: the account occupying a session. The
	// rest of the class is not needed - a session's own State already tells
	// console from RDP apart for our purposes (see LoggedUser).
	wtsUserName   = 5
	wtsDomainName = 7

	// noActiveConsole is WTSGetActiveConsoleSessionId's failure value: the
	// machine has no console session at all (rare - a Server Core box whose
	// console is mid-reset), not merely an empty one.
	noActiveConsole = 0xFFFFFFFF
)

// LoggedUser returns the interactive user currently on this machine as
// `DOMAIN\user` (or bare `user` on a workgroup machine), and "" when nobody
// is logged on.
//
// "Currently on" covers both ways in: someone sitting at the console and
// someone attached over RDP look the same here, because WTS enumerates both
// as ordinary sessions - an RDP logon simply creates a second session and
// pushes the console one to WTSDisconnected. That is also why this cannot
// reuse notify.ConsoleUserSID: WTSGetActiveConsoleSessionId only ever names
// the physical console, so a machine being administered over RDP would
// report either nobody or the wrong person.
//
// The winner is picked by how present the user actually is:
//
//  1. the active console session - somebody is physically at the machine;
//  2. any other active session - an attached RDP client, whose session stays
//     WTSActive for as long as the viewer window is open;
//  3. a disconnected session - the user is still logged on with their
//     programs running, they just have no client attached right now (RDP
//     window closed without signing out, or the console after someone else
//     took over). Ranked last because it is the weakest claim, but reported:
//     for fleet inventory "who owns this machine" is exactly what a
//     disconnected session answers.
//
// A session with no user name is skipped at every rank, which is what keeps
// session 0 (services, this updater included) and the login screen out.
//
// Failures are silent and return "": this feeds a telemetry header, and no
// answer is a normal state for a machine sitting at the lock screen.
func LoggedUser() string {
	var sessions *windows.WTS_SESSION_INFO
	var count uint32
	if err := windows.WTSEnumerateSessions(wtsCurrentServerHandle, 0, 1, &sessions, &count); err != nil {
		return ""
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(sessions)))

	console := windows.WTSGetActiveConsoleSessionId()

	var activeConsole, active, disconnected string
	for _, s := range unsafe.Slice(sessions, count) {
		user := sessionUser(s.SessionID)
		if user == "" {
			continue
		}
		switch {
		case s.State == windows.WTSActive && console != noActiveConsole && s.SessionID == console:
			activeConsole = user
		case s.State == windows.WTSActive:
			if active == "" {
				active = user
			}
		case s.State == windows.WTSDisconnected:
			if disconnected == "" {
				disconnected = user
			}
		}
	}

	switch {
	case activeConsole != "":
		return activeConsole
	case active != "":
		return active
	default:
		return disconnected
	}
}

// sessionUser returns `DOMAIN\user` for one session, or "" when the session
// carries no logged-on account (session 0, an unattended login screen) or
// the query fails.
func sessionUser(sessionID uint32) string {
	user := sanitizeHeaderValue(querySessionString(sessionID, wtsUserName))
	if user == "" {
		return ""
	}
	domain := sanitizeHeaderValue(querySessionString(sessionID, wtsDomainName))
	if domain == "" || strings.EqualFold(domain, user) {
		// A workgroup machine reports its own hostname as the "domain";
		// keeping it would turn every local account into PC-NAME\user for no
		// gain. A domain equal to the user name is the degenerate value
		// Windows reports for some built-in accounts - drop that too.
		return user
	}
	return domain + `\` + user
}

// querySessionString reads one string-valued WTS_INFO_CLASS off a session.
// WTSQuerySessionInformationW hands back a buffer it owns, so the value is
// copied out before WTSFreeMemory releases it.
func querySessionString(sessionID uint32, infoClass uint32) string {
	var buf *uint16
	var size uint32
	ret, _, _ := procWTSQuerySessionInformation.Call(
		uintptr(wtsCurrentServerHandle),
		uintptr(sessionID),
		uintptr(infoClass),
		uintptr(unsafe.Pointer(&buf)),
		uintptr(unsafe.Pointer(&size)),
	)
	if ret == 0 || buf == nil {
		return ""
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(buf)))
	return strings.TrimSpace(windows.UTF16PtrToString(buf))
}

// sanitizeHeaderValue strips what cannot travel in an HTTP header field.
// Windows account and domain names are far more permissive than the header
// grammar (a control character is unlikely but not impossible, and Go's
// transport rejects the whole request over a single one), so anything below
// 0x20 and DEL is dropped rather than escaped: this is a telemetry value,
// and a mangled name beats a failed update check.
func sanitizeHeaderValue(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
