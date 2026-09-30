package notify

import (
	"fmt"
	"strconv"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"emlyupdater/internal/progresswin"
)

// ProgressWindow is the service's end of a progress window running in the
// session of the user at the machine (console or RDP) (see internal/progresswin). Every method is safe on
// a nil *ProgressWindow and never blocks the caller: a window is a courtesy,
// and neither a download nor an install may ever wait on one.
type ProgressWindow struct {
	stdin windows.Handle

	// latest holds the one message not yet written; writer drains it. A
	// window that stops reading (hung, or its user logging off) fills the
	// pipe and blocks only the writer goroutine, never Update.
	mu      sync.Mutex
	latest  []byte
	closing bool
	wake    chan struct{}
}

// ProgressWindowOptions describe the window to open.
type ProgressWindowOptions struct {
	// SelfExe is this binary, re-launched with "show-progress".
	SelfExe  string
	Title    string
	IconPath string
	// WaitService/WaitPID: see progresswin.Options. Set for the self-update.
	WaitService string
	WaitPID     uint32
}

// OpenProgressWindow starts the window in the user's session, at the console
// or over RDP (viewerSession). It returns nil, nil when there is none.
func OpenProgressWindow(o ProgressWindowOptions) (*ProgressWindow, error) {
	session, ok := viewerSession()
	if !ok {
		return nil, nil
	}
	argv := []string{o.SelfExe, "show-progress", "--title", o.Title, "--icon", o.IconPath}
	if o.WaitService != "" {
		argv = append(argv, "--wait-service", o.WaitService, "--wait-pid", strconv.FormatUint(uint64(o.WaitPID), 10))
	}
	stdin, err := startWithStdin(session, argv)
	if err != nil {
		return nil, err
	}
	w := &ProgressWindow{stdin: stdin, wake: make(chan struct{}, 1)}
	go w.writer()
	return w, nil
}

// Update replaces what the window shows. percent is 0-100 or
// progresswin.Indeterminate.
func (w *ProgressWindow) Update(heading, detail string, percent int) {
	w.send(progresswin.Encode(progresswin.Message{Heading: heading, Detail: detail, Percent: percent}), false)
}

// Close closes the window.
func (w *ProgressWindow) Close() {
	w.send(progresswin.Encode(progresswin.Message{Close: true}), true)
}

// Detach lets go of the window without closing it: it stays up until the
// service is back under a new process (ProgressWindowOptions.WaitService) -
// the self-update, whose setup is about to stop this one.
func (w *ProgressWindow) Detach() {
	w.send(nil, true)
}

func (w *ProgressWindow) send(line []byte, last bool) {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.closing {
		w.mu.Unlock()
		return
	}
	if line != nil {
		w.latest = line
	}
	w.closing = last
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *ProgressWindow) writer() {
	defer windows.CloseHandle(w.stdin) // EOF: the window's cue if no Close came
	for range w.wake {
		w.mu.Lock()
		line, last := w.latest, w.closing
		w.latest = nil
		w.mu.Unlock()
		if line != nil {
			var n uint32
			if err := windows.WriteFile(w.stdin, line, &n, nil); err != nil {
				return // the window is gone: nothing left to tell it
			}
		}
		if last {
			return
		}
	}
}

// startWithStdin starts argv in sessionID as its user, and returns the write
// end of a pipe connected to its stdin. Its stdout and stderr go to NUL.
func startWithStdin(sessionID uint32, argv []string) (windows.Handle, error) {
	for _, priv := range []string{"SeTcbPrivilege", "SeAssignPrimaryTokenPrivilege", "SeIncreaseQuotaPrivilege"} {
		_ = enablePrivilege(priv) // best-effort, see LaunchToast
	}
	var userToken windows.Token
	if err := windows.WTSQueryUserToken(sessionID, &userToken); err != nil {
		return 0, fmt.Errorf("WTSQueryUserToken(session %d): %w", sessionID, err)
	}
	defer userToken.Close()
	var primaryToken windows.Token
	if err := windows.DuplicateTokenEx(userToken, windows.MAXIMUM_ALLOWED, nil,
		windows.SecurityImpersonation, windows.TokenPrimary, &primaryToken); err != nil {
		return 0, fmt.Errorf("DuplicateTokenEx: %w", err)
	}
	defer primaryToken.Close()
	var envBlock *uint16
	if err := windows.CreateEnvironmentBlock(&envBlock, primaryToken, false); err != nil {
		return 0, fmt.Errorf("CreateEnvironmentBlock: %w", err)
	}
	defer windows.DestroyEnvironmentBlock(envBlock)

	inherit := windows.SecurityAttributes{InheritHandle: 1}
	inherit.Length = uint32(unsafe.Sizeof(inherit))

	// The mirror image of inheritablePipe: here the child reads.
	var stdinR, stdinW windows.Handle
	if err := windows.CreatePipe(&stdinR, &stdinW, &inherit, 0); err != nil {
		return 0, fmt.Errorf("CreatePipe: %w", err)
	}
	if err := windows.SetHandleInformation(stdinW, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		windows.CloseHandle(stdinR)
		windows.CloseHandle(stdinW)
		return 0, fmt.Errorf("SetHandleInformation: %w", err)
	}
	nul, err := windows.CreateFile(windows.StringToUTF16Ptr("NUL"), windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, &inherit, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		windows.CloseHandle(stdinR)
		windows.CloseHandle(stdinW)
		return 0, fmt.Errorf("opening NUL: %w", err)
	}

	pi, err := createInSession(primaryToken, envBlock, argv, []windows.Handle{stdinR, nul, nul})
	// The child holds its own copies now; ours of its ends must go, or the
	// pipe would never report the window's exit.
	windows.CloseHandle(stdinR)
	windows.CloseHandle(nul)
	if err != nil {
		windows.CloseHandle(stdinW)
		return 0, err
	}
	windows.CloseHandle(pi.Thread)
	windows.CloseHandle(pi.Process)
	return stdinW, nil
}
