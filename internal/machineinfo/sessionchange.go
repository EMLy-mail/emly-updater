package machineinfo

import (
	"time"
	"unsafe"
)

// SessionChangeKind names one SERVICE_CONTROL_SESSIONCHANGE notification. The
// values are this package's own labels, not yet a wire contract: nothing
// sends them to the API (see Updater.watchSessions).
type SessionChangeKind string

const (
	SessionConsoleConnect    SessionChangeKind = "console-connect"
	SessionConsoleDisconnect SessionChangeKind = "console-disconnect"
	SessionRemoteConnect     SessionChangeKind = "remote-connect"
	SessionRemoteDisconnect  SessionChangeKind = "remote-disconnect"
	SessionLogon             SessionChangeKind = "logon"
	SessionLogoff            SessionChangeKind = "logoff"
	SessionLock              SessionChangeKind = "lock"
	SessionUnlock            SessionChangeKind = "unlock"
	SessionRemoteControl     SessionChangeKind = "remote-control"
	SessionCreate            SessionChangeKind = "create"
	SessionTerminate         SessionChangeKind = "terminate"
)

// sessionChangeKinds maps the WTS_* event types (winuser.h) the SCM passes as
// the dwEventType of a SERVICE_CONTROL_SESSIONCHANGE.
var sessionChangeKinds = map[uint32]SessionChangeKind{
	0x1: SessionConsoleConnect,
	0x2: SessionConsoleDisconnect,
	0x3: SessionRemoteConnect,
	0x4: SessionRemoteDisconnect,
	0x5: SessionLogon,
	0x6: SessionLogoff,
	0x7: SessionLock,
	0x8: SessionUnlock,
	0x9: SessionRemoteControl,
	0xA: SessionCreate,
	0xB: SessionTerminate,
}

// wtsSessionNotification mirrors WTSSESSION_NOTIFICATION, which x/sys/windows
// does not define: two DWORDs, so the layout is the same on every arch.
type wtsSessionNotification struct {
	Size      uint32
	SessionID uint32
}

// SessionChange is one session notification, and - once Resolve has run -
// who the machine's logged-on user is after it.
type SessionChange struct {
	Kind      SessionChangeKind
	SessionID uint32
	// At is when the service received the notification (local clock).
	At time.Time

	// Filled by Resolve. SessionUser is the account in SessionID, "" when
	// it has none (the login screen) or the session is already gone - a
	// logoff or terminate usually arrives after the account has left.
	// LoggedUser is the machine-wide answer LoggedUser gives right after the
	// change, i.e. what the X-EMLy-LoggedUser* headers would now report.
	SessionUser string
	LoggedUser  UserSession
}

// ParseSessionChange decodes the EventType/EventData of a svc.SessionChange
// request. ok is false for an event type this package does not know.
//
// Call it as soon as the request arrives, before doing anything else:
// x/sys/windows/svc forwards EventData as the raw pointer the SCM handed its
// control handler, and that handler has already returned by the time
// Execute reads the request, so the buffer is only borrowed. A nil pointer
// leaves SessionID at 0 rather than failing the whole event.
func ParseSessionChange(eventType uint32, eventData uintptr, at time.Time) (SessionChange, bool) {
	kind, ok := sessionChangeKinds[eventType]
	if !ok {
		return SessionChange{}, false
	}
	c := SessionChange{Kind: kind, At: at}
	if eventData != 0 {
		n := (*wtsSessionNotification)(unsafe.Pointer(eventData))
		c.SessionID = n.SessionID
	}
	return c, true
}

// Resolve fills SessionUser and LoggedUser with WTS queries. It is the slow
// half, kept apart from ParseSessionChange so it can run off the service
// control loop; call it after the burst of notifications an RDP reconnect
// produces has settled, or Windows may still report the pre-change state.
func (c SessionChange) Resolve() SessionChange {
	c.SessionUser = sessionUser(c.SessionID)
	c.LoggedUser = LoggedUser()
	return c
}
