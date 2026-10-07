// Package crash tells the user, with a message box, that AryxD Agent hit a
// fatal error and is about to close - instead of the process silently
// disappearing (the tray icon vanishing, a window closing on its own).
//
// Report shows the box; Guard is the deferred form for panics. In a desktop
// session (tray, progress window, foreground run) the box is a plain
// MessageBoxW and Report waits until the user dismisses it. In session 0 -
// the service - nobody can see a MessageBoxW, so the box is sent to the
// viewer session through WTSSendMessageW (internal/notify) without waiting:
// it belongs to that session and stays up after the service has exited.
//
// Go cannot intercept a panic in a goroutine from outside it: only the
// goroutines that defer Guard are covered. main does, and so do the
// goroutines the service and the tray start.
package crash

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/sys/windows"

	"emlyupdater/internal/notify"
	"emlyupdater/internal/version"
)

// Headline is the box's first line, above the error itself.
const Headline = version.ProductName + " ha subito un grave errore e deve chiudersi."

// maxDetail bounds the error text in the box: a wrapped error chain or a
// panic value can be arbitrarily long, and MessageBoxW grows to fit it.
const maxDetail = 1500

// once makes a box appear at most once per process: two goroutines failing
// together would otherwise stack two boxes for one crash.
var once sync.Once

// Logger is the subset of *logging.Logger Guard uses.
type Logger interface {
	Error(msg string, kv ...any)
}

// Report shows the fatal-error box for detail. It does not exit: the caller
// does, right after. Best-effort - it never fails, and is a no-op when nobody
// could see the box.
func Report(detail string) {
	once.Do(func() { show(Text(detail)) })
}

// Text is the box's body: the headline, then the (truncated) error.
func Text(detail string) string {
	detail = strings.ReplaceAll(detail, "\x00", " ") // UTF16PtrFromString rejects NULs
	if detail == "" {
		return Headline
	}
	if len(detail) > maxDetail {
		cut := maxDetail
		for cut > 0 && !utf8.RuneStart(detail[cut]) {
			cut--
		}
		detail = detail[:cut] + "…"
	}
	return Headline + "\n\nErrore: " + detail
}

// Guard, deferred at the top of a goroutine, reports a panic in it with the
// box (and to log, when not nil) and then panics again with the same value,
// so the process still dies the way Go makes it die - exit code 2, stack on
// stderr - and the SCM's recovery actions still apply to the service.
func Guard(log Logger) {
	r := recover()
	if r == nil {
		return
	}
	if log != nil {
		log.Error("fatal panic, "+version.ProductName+" is closing", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
	}
	Report(fmt.Sprint(r))
	panic(r)
}

func show(body string) {
	title := version.ProductName + " - Errore"
	if inSessionZero() {
		notify.SendErrorBox(title, body)
		return
	}
	// On a fresh OS thread that owns no window: MessageBoxW runs a modal
	// loop that dispatches the calling thread's messages, and the thread
	// that failed may be the tray's UI thread, whose windows must not be
	// re-entered while it is panicking.
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		t, err := windows.UTF16PtrFromString(title)
		if err != nil {
			return
		}
		b, err := windows.UTF16PtrFromString(body)
		if err != nil {
			return
		}
		_, _ = windows.MessageBox(0, b, t, windows.MB_OK|windows.MB_ICONERROR|windows.MB_SETFOREGROUND|windows.MB_TOPMOST)
	}()
	<-done
}

// inSessionZero reports whether this process runs in session 0, where the
// service lives and no user sees a window.
func inSessionZero() bool {
	var session uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session); err != nil {
		return false
	}
	return session == 0
}
