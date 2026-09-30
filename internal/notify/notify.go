// Package notify shows the pre-kill warning for critical updates via
// WTSSendMessageW. Called from the SYSTEM service, the message box renders
// inside the session of the user at the machine, at the console or over RDP
// (see viewerSession) - no helper process, no toast registration.
package notify

import (
	"fmt"
	"strings"
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

// Message is the language-specific message shown to users.
type Message struct {
	Title string
	Body  string
}

// warning text per language; %d is the countdown in seconds.
var messages = map[string]Message{
	"en": {
		Title: "EMLy - Critical Update",
		Body:  "EMLy will close in %d seconds to install a critical update.\n\nPlease save your work.",
	},
	"it": {
		Title: "EMLy - Aggiornamento critico",
		Body:  "EMLy verrà chiuso tra %d secondi per installare un aggiornamento critico.\n\nSi prega di salvare il proprio lavoro.",
	},
}

// formatBody applies seconds to body only when body has a verb for it.
func formatBody(body string, seconds int) string {
	if strings.Contains(body, "%") {
		return fmt.Sprintf(body, seconds)
	}
	return body
}

// sendBox shows title/body in the viewer session without waiting, the box
// auto-dismissing after seconds. False when nobody is at the machine.
func sendBox(title, body string, seconds int) bool {
	session, ok := viewerSession()
	if !ok {
		return false
	}
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
		uintptr(session),
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

// WarnCriticalUpdate shows the countdown warning in the user's session
// (viewerSession) and returns true when a box was actually displayed. It does NOT
// sleep: the box is sent with bWait=FALSE and auto-dismisses after `seconds`,
// while the caller owns the full countdown - that way the promised N seconds
// elapse even if the user clicks OK immediately.
//
// Returns false (warn skipped) when no user session is active: nobody is
// looking, so the caller may kill immediately.
func WarnCriticalUpdate(lang string, seconds int) bool {
	msg, ok := messages[lang]
	if !ok {
		msg = messages["en"]
	}
	return sendBox(msg.Title, formatBody(msg.Body, seconds), seconds)
}

func SendNotifyBox(msg Message, seconds int) bool {
	return sendBox(msg.Title, formatBody(msg.Body, seconds), seconds)
}

// CriticalUpdateProductMessage is the critical-update countdown warning for a
// product other than EMLy. Italian only, like the progress window. %d is the
// countdown in seconds. The name comes from the remote-configuration
// document, so any '%' in it is escaped: the body is a format string.
func CriticalUpdateProductMessage(name string) Message {
	return Message{
		Title: name + " - Aggiornamento critico",
		Body:  strings.ReplaceAll(name, "%", "%%") + " verrà chiuso tra %d secondi per installare un aggiornamento critico.\n\nSi prega di salvare il proprio lavoro.",
	}
}

// WarnCriticalUpdateProduct is WarnCriticalUpdate for a product other than EMLy.
func WarnCriticalUpdateProduct(name string, seconds int) bool {
	msg := CriticalUpdateProductMessage(name)
	return sendBox(msg.Title, formatBody(msg.Body, seconds), seconds)
}

// ProductWaitingMessage tells the user an update is waiting for them to close
// a product other than EMLy.
func ProductWaitingMessage(name string) Message {
	return Message{
		Title: name + " - Aggiornamento in attesa",
		Body:  "Un aggiornamento di " + name + " è pronto. Chiudere l'applicazione per completarlo.",
	}
}
