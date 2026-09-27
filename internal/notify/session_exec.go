package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// SessionOutput is what a process run by RunInSession left behind.
type SessionOutput struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// sessionPipeDrain bounds how long RunInSession keeps reading after the
// process has exited: a grandchild that inherited the write ends would
// otherwise hold the call open until it exits too - the same reason
// exec.Cmd.WaitDelay exists.
const sessionPipeDrain = 5 * time.Second

// RunInSession runs argv as the user logged on to WTS session sessionID and
// returns its output once it exits, the same SYSTEM -> user-session hop as
// LaunchToast (WTSQueryUserToken + CreateProcessAsUser) but waited on, with
// stdout and stderr captured through pipes. The process gets the user's own
// environment block and no window.
//
// It is for asking questions only the user's session can answer - what is
// installed for that user alone (HKCU, per-user MSIX) is invisible to
// LocalSystem. A disconnected session still has a token, so it works there
// too.
//
// Handle inheritance is limited to the three standard handles with
// PROC_THREAD_ATTRIBUTE_HANDLE_LIST, as os/exec does: a plain
// bInheritHandles=TRUE would hand the child every inheritable handle in the
// service at that instant, including another command's pipes that os/exec
// has made inheritable for its own child.
//
// Cancelling ctx terminates the process. A non-zero exit is not an error: it
// is reported in ExitCode for the caller to interpret.
func RunInSession(ctx context.Context, sessionID uint32, argv []string) (SessionOutput, error) {
	if len(argv) == 0 {
		return SessionOutput{}, errors.New("RunInSession: empty argv")
	}
	for _, priv := range []string{"SeTcbPrivilege", "SeAssignPrimaryTokenPrivilege", "SeIncreaseQuotaPrivilege"} {
		_ = enablePrivilege(priv) // best-effort, see LaunchToast
	}

	var userToken windows.Token
	if err := windows.WTSQueryUserToken(sessionID, &userToken); err != nil {
		return SessionOutput{}, fmt.Errorf("WTSQueryUserToken(session %d): %w", sessionID, err)
	}
	defer userToken.Close()

	var primaryToken windows.Token
	if err := windows.DuplicateTokenEx(userToken, windows.MAXIMUM_ALLOWED, nil,
		windows.SecurityImpersonation, windows.TokenPrimary, &primaryToken); err != nil {
		return SessionOutput{}, fmt.Errorf("DuplicateTokenEx: %w", err)
	}
	defer primaryToken.Close()

	var envBlock *uint16
	if err := windows.CreateEnvironmentBlock(&envBlock, primaryToken, false); err != nil {
		return SessionOutput{}, fmt.Errorf("CreateEnvironmentBlock: %w", err)
	}
	defer windows.DestroyEnvironmentBlock(envBlock)

	inherit := windows.SecurityAttributes{InheritHandle: 1}
	inherit.Length = uint32(unsafe.Sizeof(inherit))

	stdoutR, stdoutW, err := inheritablePipe(&inherit)
	if err != nil {
		return SessionOutput{}, err
	}
	defer windows.CloseHandle(stdoutR)
	stderrR, stderrW, err := inheritablePipe(&inherit)
	if err != nil {
		windows.CloseHandle(stdoutW)
		return SessionOutput{}, err
	}
	defer windows.CloseHandle(stderrR)
	nul, err := windows.CreateFile(windows.StringToUTF16Ptr("NUL"), windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, &inherit, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		windows.CloseHandle(stdoutW)
		windows.CloseHandle(stderrW)
		return SessionOutput{}, fmt.Errorf("opening NUL: %w", err)
	}
	// The child holds its own copies once created; the parent's must go so
	// that the read ends see EOF when the child exits.
	childEnds := []windows.Handle{nul, stdoutW, stderrW}
	closeChildEnds := func() {
		for _, h := range childEnds {
			windows.CloseHandle(h)
		}
	}

	pi, err := createInSession(primaryToken, envBlock, argv, childEnds)
	closeChildEnds()
	if err != nil {
		return SessionOutput{}, err
	}
	windows.CloseHandle(pi.Thread)
	defer windows.CloseHandle(pi.Process)

	var stdout, stderr bytes.Buffer
	var readers sync.WaitGroup
	for _, p := range []struct {
		h   windows.Handle
		dst *bytes.Buffer
	}{{stdoutR, &stdout}, {stderrR, &stderr}} {
		readers.Go(func() { readPipe(p.h, p.dst) })
	}

	exited := make(chan struct{})
	go func() {
		_, _ = windows.WaitForSingleObject(pi.Process, windows.INFINITE)
		close(exited)
	}()
	select {
	case <-exited:
	case <-ctx.Done():
		_ = windows.TerminateProcess(pi.Process, 1)
		<-exited
	}

	drained := make(chan struct{})
	go func() { readers.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(sessionPipeDrain):
		// Cancelling the pending reads is what unblocks readPipe; the
		// buffers keep whatever arrived until now.
		_ = windows.CancelIoEx(stdoutR, nil)
		_ = windows.CancelIoEx(stderrR, nil)
		select {
		case <-drained:
		case <-time.After(sessionPipeDrain):
			// The buffers are still being written: returning them would race.
			return SessionOutput{}, errors.New("process exited but its output pipes never closed")
		}
	}

	if err := ctx.Err(); err != nil {
		return SessionOutput{}, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(pi.Process, &code); err != nil {
		return SessionOutput{}, fmt.Errorf("GetExitCodeProcess: %w", err)
	}
	return SessionOutput{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: int(code)}, nil
}

// readPipe copies h into dst until the write end closes (ERROR_BROKEN_PIPE)
// or the read fails or is cancelled.
func readPipe(h windows.Handle, dst *bytes.Buffer) {
	buf := make([]byte, 32*1024)
	for {
		var n uint32
		err := windows.ReadFile(h, buf, &n, nil)
		dst.Write(buf[:n])
		if err != nil {
			return
		}
	}
}

// inheritablePipe returns an anonymous pipe whose write end the child can
// inherit and whose read end stays private to this process.
func inheritablePipe(sa *windows.SecurityAttributes) (r, w windows.Handle, err error) {
	if err := windows.CreatePipe(&r, &w, sa, 0); err != nil {
		return 0, 0, fmt.Errorf("CreatePipe: %w", err)
	}
	if err := windows.SetHandleInformation(r, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		windows.CloseHandle(r)
		windows.CloseHandle(w)
		return 0, 0, fmt.Errorf("SetHandleInformation: %w", err)
	}
	return r, w, nil
}

// createInSession starts argv with token, its stdin/stdout/stderr set to
// std[0..2], which are also the only handles it inherits.
func createInSession(token windows.Token, env *uint16, argv []string, std []windows.Handle) (windows.ProcessInformation, error) {
	var pi windows.ProcessInformation
	cmdLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(argv))
	if err != nil {
		return pi, err
	}
	desktop, err := windows.UTF16PtrFromString(`winsta0\default`)
	if err != nil {
		return pi, err
	}

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return pi, fmt.Errorf("NewProcThreadAttributeList: %w", err)
	}
	defer attrs.Delete()
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
		unsafe.Pointer(&std[0]), uintptr(len(std))*unsafe.Sizeof(std[0])); err != nil {
		return pi, fmt.Errorf("UpdateProcThreadAttribute: %w", err)
	}

	var si windows.StartupInfoEx
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Desktop = desktop
	si.Flags = windows.STARTF_USESTDHANDLES
	si.StdInput, si.StdOutput, si.StdErr = std[0], std[1], std[2]
	si.ProcThreadAttributeList = attrs.List()

	if err := windows.CreateProcessAsUser(token, nil, cmdLine, nil, nil, true,
		windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_NO_WINDOW|windows.EXTENDED_STARTUPINFO_PRESENT,
		env, nil, &si.StartupInfo, &pi); err != nil {
		return pi, fmt.Errorf("CreateProcessAsUser: %w", err)
	}
	return pi, nil
}
