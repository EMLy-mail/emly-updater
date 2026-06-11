// Package notify shows the pre-kill warning for critical updates via
// WTSSendMessageW. Called from the SYSTEM service, the message box renders
// inside the active console user's session — no helper process, no toast
// registration.
package notify

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	wtsapi32           = windows.NewLazySystemDLL("wtsapi32.dll")
	kernel32           = windows.NewLazySystemDLL("kernel32.dll")
	procWTSSendMessage = wtsapi32.NewProc("WTSSendMessageW")
	procActiveConsole  = kernel32.NewProc("WTSGetActiveConsoleSessionId")
)

const (
	wtsCurrentServerHandle = 0

	mbOK            = 0x00000000
	mbIconWarning   = 0x00000030
	mbSetForeground = 0x00010000
	mbTopMost       = 0x00040000

	// noConsoleSession is WTSGetActiveConsoleSessionId's failure value: no
	// user is at the console (e.g. logged off), so there is nobody to warn.
	noConsoleSession = 0xFFFFFFFF
)

// warning text per language; %d is the countdown in seconds.
var messages = map[string]struct{ title, body string }{
	"en": {
		title: "EMLy — Critical Update",
		body:  "EMLy will close in %d seconds to install a critical update.\n\nPlease save your work.",
	},
	"it": {
		title: "EMLy — Aggiornamento critico",
		body:  "EMLy verrà chiuso tra %d secondi per installare un aggiornamento critico.\n\nSi prega di salvare il proprio lavoro.",
	},
}

// WarnCriticalUpdate shows the countdown warning in the active console
// session and returns true when a box was actually displayed. It does NOT
// sleep: the box is sent with bWait=FALSE and auto-dismisses after `seconds`,
// while the caller owns the full countdown — that way the promised N seconds
// elapse even if the user clicks OK immediately.
//
// Returns false (warn skipped) when no console session is active: nobody is
// looking, so the caller may kill immediately.
func WarnCriticalUpdate(lang string, seconds int) bool {
	session, _, _ := procActiveConsole.Call()
	if uint32(session) == noConsoleSession {
		return false
	}

	msg, ok := messages[lang]
	if !ok {
		msg = messages["en"]
	}
	title := msg.title
	body := fmt.Sprintf(msg.body, seconds)

	titleU16, err := windows.UTF16FromString(title)
	if err != nil {
		return false
	}
	bodyU16, err := windows.UTF16FromString(body)
	if err != nil {
		return false
	}

	var response uint32
	// Title/message lengths are in BYTES, excluding the NUL terminator.
	// bWait=FALSE: return immediately; Timeout still auto-dismisses the box.
	ret, _, _ := procWTSSendMessage.Call(
		wtsCurrentServerHandle,
		session,
		uintptr(unsafe.Pointer(&titleU16[0])),
		uintptr((len(titleU16)-1)*2),
		uintptr(unsafe.Pointer(&bodyU16[0])),
		uintptr((len(bodyU16)-1)*2),
		uintptr(mbOK|mbIconWarning|mbSetForeground|mbTopMost),
		uintptr(seconds),
		uintptr(unsafe.Pointer(&response)),
		0, // bWait = FALSE
	)
	return ret != 0
}
