package machineinfo

import (
	"encoding/binary"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	wtsapi32                       = windows.NewLazySystemDLL("wtsapi32.dll")
	procWTSQuerySessionInformation = wtsapi32.NewProc("WTSQuerySessionInformationW")
)

const (
	wtsCurrentServerHandle = 0

	// WTS_INFO_CLASS values we query: the account occupying a session, and the
	// extended record that carries its timestamps. A session's own State
	// already tells console from RDP apart for our purposes (see LoggedUser).
	wtsUserName      = 5
	wtsDomainName    = 7
	wtsSessionInfoEx = 25

	// noActiveConsole is WTSGetActiveConsoleSessionId's failure value: the
	// machine has no console session at all (rare - a Server Core box whose
	// console is mid-reset), not merely an empty one.
	noActiveConsole = 0xFFFFFFFF
)

// SessionState is how present the logged-on user is, sent as
// X-EMLy-LoggedUserState. The values are part of the wire contract with the
// API: rename one and the server stops recognising it.
type SessionState string

const (
	// SessionActiveConsole: somebody is physically at the machine.
	SessionActiveConsole SessionState = "active-console"
	// SessionActiveRDP: a remote client is attached right now.
	SessionActiveRDP SessionState = "active-rdp"
	// SessionDisconnected: the user is still logged on with their programs
	// running but no client is attached - an RDP window closed without
	// signing out, or the console after someone else took over.
	SessionDisconnected SessionState = "disconnected"
)

// UserSession is the interactive user on this machine and the state of the
// session they occupy. The zero value means nobody is logged on.
type UserSession struct {
	// User is `DOMAIN\user`, or bare `user` on a workgroup machine.
	User string
	// State is empty exactly when User is.
	State SessionState
	// DisconnectedAt is when the session lost its client, in UTC. Only set
	// for SessionDisconnected, and even then left zero when Windows does not
	// report it: an active session keeps the timestamp of its last
	// disconnection, which would be misleading to pass on.
	DisconnectedAt time.Time
}

// LoggedUser returns the interactive user currently on this machine and how
// they are attached to it. The zero UserSession means nobody is logged on.
//
// "Currently on" covers both ways in: someone sitting at the console and
// someone attached over RDP look the same here, because WTS enumerates both
// as ordinary sessions - an RDP logon simply creates a second session and
// pushes the console one to WTSDisconnected. That is also why this cannot
// reuse notify.ConsoleUserSID: WTSGetActiveConsoleSessionId only ever names
// the physical console, so a machine being administered over RDP would
// report either nobody or the wrong person. See pickSession for which session
// wins when there is more than one.
//
// Failures are silent and return the zero value: this feeds telemetry
// headers, and no answer is a normal state for a machine sitting at the lock
// screen.
func LoggedUser() UserSession {
	var sessions *windows.WTS_SESSION_INFO
	var count uint32
	if err := windows.WTSEnumerateSessions(wtsCurrentServerHandle, 0, 1, &sessions, &count); err != nil {
		return UserSession{}
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(sessions)))

	var entries []sessionEntry
	for _, s := range unsafe.Slice(sessions, count) {
		entries = append(entries, sessionEntry{
			ID:    s.SessionID,
			State: s.State,
			User:  sessionUser(s.SessionID),
		})
	}

	picked, state, ok := pickSession(entries, windows.WTSGetActiveConsoleSessionId())
	if !ok {
		return UserSession{}
	}
	us := UserSession{User: picked.User, State: state}
	if state == SessionDisconnected {
		us.DisconnectedAt = sessionDisconnectTime(picked.ID)
	}
	return us
}

// sessionEntry is the part of a WTS session pickSession decides on.
type sessionEntry struct {
	ID    uint32
	State uint32 // a windows.WTS* connect state
	User  string // "" when the session carries no logged-on account
}

// pickSession chooses the session that best answers "who is at this machine",
// ranked by how present the user actually is:
//
//  1. the active console session - somebody is physically at the machine;
//  2. any other active session - an attached RDP client, whose session stays
//     WTSActive for as long as the viewer window is open. A session that is
//     not the console is remote by definition, so this needs no protocol
//     query;
//  3. a disconnected session - the user is still logged on, they just have
//     no client attached right now. Ranked last because it is the weakest
//     claim, but reported: for fleet inventory "who owns this machine" is
//     exactly what a disconnected session answers, and the state tells the
//     server it is not a live presence.
//
// A session with no user name is skipped at every rank, which is what keeps
// session 0 (services, this updater included), the login screen on a
// connected-but-empty console and the RDP listener out. Within a rank the
// first session enumerated wins.
func pickSession(entries []sessionEntry, console uint32) (sessionEntry, SessionState, bool) {
	var activeConsole, active, disconnected *sessionEntry
	for i := range entries {
		e := &entries[i]
		if e.User == "" {
			continue
		}
		switch {
		case e.State == windows.WTSActive && console != noActiveConsole && e.ID == console:
			activeConsole = e
		case e.State == windows.WTSActive:
			if active == nil {
				active = e
			}
		case e.State == windows.WTSDisconnected:
			if disconnected == nil {
				disconnected = e
			}
		}
	}

	switch {
	case activeConsole != nil:
		return *activeConsole, SessionActiveConsole, true
	case active != nil:
		return *active, SessionActiveRDP, true
	case disconnected != nil:
		return *disconnected, SessionDisconnected, true
	default:
		return sessionEntry{}, "", false
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
	buf, release, ok := querySession(sessionID, infoClass)
	if !ok {
		return ""
	}
	defer release()
	return strings.TrimSpace(windows.UTF16PtrToString((*uint16)(unsafe.Pointer(&buf[0]))))
}

// querySession runs WTSQuerySessionInformationW and returns the buffer it
// allocated as a byte slice, with the function that frees it. The slice must
// not be used after release.
func querySession(sessionID uint32, infoClass uint32) (buf []byte, release func(), ok bool) {
	var p *byte
	var size uint32
	ret, _, _ := procWTSQuerySessionInformation.Call(
		uintptr(wtsCurrentServerHandle),
		uintptr(sessionID),
		uintptr(infoClass),
		uintptr(unsafe.Pointer(&p)),
		uintptr(unsafe.Pointer(&size)),
	)
	if ret == 0 || p == nil {
		return nil, nil, false
	}
	release = func() { windows.WTSFreeMemory(uintptr(unsafe.Pointer(p))) }
	if size == 0 {
		release()
		return nil, nil, false
	}
	return unsafe.Slice(p, size), release, true
}

// Byte offsets into the WTSINFOEXW buffer WTSSessionInfoEx returns. The
// record is a DWORD Level followed by a union holding WTSINFOEX_LEVEL1_W,
// which starts at 8 because the union contains LARGE_INTEGERs:
//
//	+0   SessionId, +4 SessionState, +8 SessionFlags   (3 x DWORD/LONG)
//	+12  WinStationName  WCHAR[33]
//	+78  UserName        WCHAR[21]
//	+120 DomainName      WCHAR[18]
//	+160 LogonTime, +168 ConnectTime, +176 DisconnectTime,
//	+184 LastInputTime, +192 CurrentTime               (LARGE_INTEGER, 8-aligned)
//
// The layout is identical on x86 and x64 (no pointers, and MSVC aligns
// LARGE_INTEGER to 8 on both), so the offsets are read straight off the
// bytes rather than through a Go struct, whose int64 alignment on 386 would
// not match.
const (
	wtsInfoExLevel1        = 1
	wtsInfoExDataOffset    = 8
	wtsInfoExDisconnectOff = wtsInfoExDataOffset + 176
	wtsInfoExCurrentOff    = wtsInfoExDataOffset + 192
	wtsInfoExMinSize       = wtsInfoExCurrentOff + 8
)

// sessionTimes is the pair of WTSINFOEX_LEVEL1 timestamps this package reads.
type sessionTimes struct {
	Disconnect time.Time // zero when the session has never been disconnected
	Current    time.Time // the server's clock at query time
}

// querySessionTimes reads the timestamps off one session's WTSSessionInfoEx
// record.
func querySessionTimes(sessionID uint32) (sessionTimes, bool) {
	buf, release, ok := querySession(sessionID, wtsSessionInfoEx)
	if !ok {
		return sessionTimes{}, false
	}
	defer release()
	return parseSessionInfoEx(buf)
}

// parseSessionInfoEx decodes the timestamps from a raw WTSINFOEXW buffer.
func parseSessionInfoEx(buf []byte) (sessionTimes, bool) {
	if len(buf) < wtsInfoExMinSize || binary.LittleEndian.Uint32(buf) != wtsInfoExLevel1 {
		return sessionTimes{}, false
	}
	return sessionTimes{
		Disconnect: fileTimeToTime(int64(binary.LittleEndian.Uint64(buf[wtsInfoExDisconnectOff:]))),
		Current:    fileTimeToTime(int64(binary.LittleEndian.Uint64(buf[wtsInfoExCurrentOff:]))),
	}, true
}

// sessionDisconnectTime returns when a session lost its client, or the zero
// time when Windows does not say.
func sessionDisconnectTime(sessionID uint32) time.Time {
	t, ok := querySessionTimes(sessionID)
	if !ok {
		return time.Time{}
	}
	return t.Disconnect
}

// fileTimeToTime converts a FILETIME carried as a LARGE_INTEGER (100 ns
// ticks since 1601-01-01 UTC) to a UTC time.Time. Zero and negative values
// are Windows' "never" and come back as the zero time.
func fileTimeToTime(ticks int64) time.Time {
	if ticks <= 0 {
		return time.Time{}
	}
	ft := windows.Filetime{
		LowDateTime:  uint32(ticks),
		HighDateTime: uint32(ticks >> 32),
	}
	return time.Unix(0, ft.Nanoseconds()).UTC()
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
