package notify

import (
	"errors"
	"fmt"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TrayMutexName is the per-session mutex the tray (internal/tray) holds, as
// Local\<name> in its own session. EnsureTray looks it up from session 0 as
// Session\<id>\<name>, which is the same object.
const TrayMutexName = "AryxDAgentTray"

// TrayLaunch is what EnsureTray did in one session.
type TrayLaunch struct {
	SessionID uint32
	Err       error // nil: started
}

// EnsureTray starts `exe tray` as the logged-on user in every active WTS
// session (console or RDP) whose tray is not running, and reports each
// session it tried. A session where the tray already runs is not reported.
//
// The HKLM Run value starts the tray at logon; this covers a tray that was
// closed, crashed, or never came back after an install - restored as soon as
// the service starts, without waiting for the next logon. Starting one where
// it already runs would be harmless anyway (the tray exits at once when its
// mutex is taken), the check only saves the process.
//
// A disconnected session is skipped: nobody sees an icon there, and its tray,
// if any, is still running.
func EnsureTray(exe string) []TrayLaunch {
	var sessions *windows.WTS_SESSION_INFO
	var count uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &sessions, &count); err != nil {
		return []TrayLaunch{{SessionID: 0xFFFFFFFF, Err: fmt.Errorf("WTSEnumerateSessions: %w", err)}}
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(sessions)))

	var out []TrayLaunch
	for _, s := range unsafe.Slice(sessions, count) {
		if s.SessionID == 0 || s.State != windows.WTSActive || trayRunning(s.SessionID) {
			continue
		}
		out = append(out, TrayLaunch{SessionID: s.SessionID, Err: startDetached(s.SessionID, []string{exe, "tray"})})
	}
	return out
}

// trayRunning reports whether session id holds the tray's mutex. Only "no
// such object" means it does not: access denied is a mutex that exists, and
// any other failure is treated as running, since a duplicate tray would only
// exit again.
func trayRunning(id uint32) bool {
	name, err := windows.UTF16PtrFromString(`Session\` + strconv.FormatUint(uint64(id), 10) + `\` + TrayMutexName)
	if err != nil {
		return true
	}
	h, err := windows.OpenMutex(windows.SYNCHRONIZE, false, name)
	if err == nil {
		windows.CloseHandle(h)
		return true
	}
	return !errors.Is(err, windows.ERROR_FILE_NOT_FOUND)
}

// startDetached starts argv as the user of session id and lets it run: the
// same hop as LaunchToast, with the error kept for the log. CREATE_NO_WINDOW
// gives the console binary no console, which is what the Run value's
// `conhost.exe --headless` achieves for a process started by Explorer.
func startDetached(id uint32, argv []string) error {
	for _, priv := range []string{"SeTcbPrivilege", "SeAssignPrimaryTokenPrivilege", "SeIncreaseQuotaPrivilege"} {
		_ = enablePrivilege(priv) // best-effort, see LaunchToast
	}

	var userToken windows.Token
	if err := windows.WTSQueryUserToken(id, &userToken); err != nil {
		return fmt.Errorf("WTSQueryUserToken: %w", err)
	}
	defer userToken.Close()

	var primaryToken windows.Token
	if err := windows.DuplicateTokenEx(userToken, windows.MAXIMUM_ALLOWED, nil,
		windows.SecurityImpersonation, windows.TokenPrimary, &primaryToken); err != nil {
		return fmt.Errorf("DuplicateTokenEx: %w", err)
	}
	defer primaryToken.Close()

	var envBlock *uint16
	if err := windows.CreateEnvironmentBlock(&envBlock, primaryToken, false); err != nil {
		return fmt.Errorf("CreateEnvironmentBlock: %w", err)
	}
	defer windows.DestroyEnvironmentBlock(envBlock)

	cmdLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(argv))
	if err != nil {
		return err
	}
	desktop, err := windows.UTF16PtrFromString(`winsta0\default`)
	if err != nil {
		return err
	}
	var si windows.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Desktop = desktop

	var pi windows.ProcessInformation
	if err := windows.CreateProcessAsUser(primaryToken, nil, cmdLine, nil, nil, false,
		windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_NO_WINDOW, envBlock, nil, &si, &pi); err != nil {
		return fmt.Errorf("CreateProcessAsUser: %w", err)
	}
	windows.CloseHandle(pi.Thread)
	windows.CloseHandle(pi.Process)
	return nil
}
