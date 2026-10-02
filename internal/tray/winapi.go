//go:build windows

package tray

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"github.com/rodrigocfd/windigo/win"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"

	"emlyupdater/internal/service"
)

var (
	shell32              = windows.NewLazySystemDLL("shell32.dll")
	procShellExecuteExW  = shell32.NewProc("ShellExecuteExW")
	procExtractIconExW   = shell32.NewProc("ExtractIconExW")
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procRegisterAppRstrt = kernel32.NewProc("RegisterApplicationRestart")
	procCreateMutexW     = kernel32.NewProc("CreateMutexW")
)

// shellExecuteInfo is SHELLEXECUTEINFOW; x/sys/windows has no ShellExecuteEx.
type shellExecuteInfo struct {
	cbSize       uint32
	fMask        uint32
	hwnd         uintptr
	lpVerb       *uint16
	lpFile       *uint16
	lpParameters *uint16
	lpDirectory  *uint16
	nShow        int32
	hInstApp     uintptr
	lpIDList     uintptr
	lpClass      *uint16
	hkeyClass    uintptr
	dwHotKey     uint32
	hIconOrMon   uintptr
	hProcess     windows.Handle
}

const (
	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoAsync        = 0x00000100
	seeMaskFlagNoUI       = 0x00000400
)

// errElevationCancelled is the user answering "No" to the UAC prompt.
var errElevationCancelled = errors.New("elevation cancelled")

// runElevated starts exe with args through the UAC prompt ("runas"), hidden,
// waits for it and returns its exit code. Blocking: call it off the UI thread.
func runElevated(owner win.HWND, exe string, args []string) (uint32, error) {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = windows.EscapeArg(a)
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	params, _ := windows.UTF16PtrFromString(strings.Join(quoted, " "))

	info := shellExecuteInfo{
		fMask:        seeMaskNoCloseProcess | seeMaskNoAsync,
		hwnd:         uintptr(owner),
		lpVerb:       verb,
		lpFile:       file,
		lpParameters: params,
		nShow:        windows.SW_HIDE,
	}
	info.cbSize = uint32(unsafe.Sizeof(info))
	if r, _, err := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info))); r == 0 {
		if errors.Is(err, windows.ERROR_CANCELLED) {
			return 0, errElevationCancelled
		}
		return 0, fmt.Errorf("ShellExecuteEx: %w", err)
	}
	if info.hProcess == 0 {
		return 0, errors.New("ShellExecuteEx returned no process handle")
	}
	defer windows.CloseHandle(info.hProcess)
	if _, err := windows.WaitForSingleObject(info.hProcess, windows.INFINITE); err != nil {
		return 0, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(info.hProcess, &code); err != nil {
		return 0, err
	}
	return code, nil
}

// exeIcons returns the small and large icon embedded in exe (the agent's own
// icon from versioninfo.json). Zero handles when it has none.
func exeIcons(exe string) (small, large win.HICON) {
	p, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return 0, 0
	}
	procExtractIconExW.Call(uintptr(unsafe.Pointer(p)), 0,
		uintptr(unsafe.Pointer(&large)), uintptr(unsafe.Pointer(&small)), 1)
	return small, large
}

// registerRestart asks Windows to relaunch the tray with "tray" after the
// Restart Manager closed it - which is what Inno Setup does to replace
// EMLyUpdater.exe while the tray is running it.
func registerRestart() {
	cmd, err := windows.UTF16PtrFromString("tray")
	if err != nil {
		return
	}
	procRegisterAppRstrt.Call(uintptr(unsafe.Pointer(cmd)), 0)
}

// singleInstance holds a per-session mutex: one tray per logged-on user.
// ok=false means another tray already runs in this session.
func singleInstance() (release func(), ok bool) {
	name, _ := windows.UTF16PtrFromString(`Local\AryxDAgentTray`)
	h, _, err := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return func() {}, true // cannot tell: better two icons than none
	}
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		windows.CloseHandle(windows.Handle(h))
		return nil, false
	}
	return func() { windows.CloseHandle(windows.Handle(h)) }, true
}

// serviceState reports the agent service's state, without admin rights:
// interactive users hold SERVICE_QUERY_STATUS in the default service DACL.
func serviceState() (svc.State, error) {
	h, err := openService(windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return 0, err
	}
	defer windows.CloseServiceHandle(h)
	var st windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(h, &st); err != nil {
		return 0, err
	}
	return svc.State(st.CurrentState), nil
}

// requestCheck sends service.CheckNowControl: interactive users hold
// SERVICE_USER_DEFINED_CONTROL in the default service DACL (IU: CR).
func requestCheck() error {
	h, err := openService(windows.SERVICE_USER_DEFINED_CONTROL)
	if err != nil {
		return err
	}
	defer windows.CloseServiceHandle(h)
	var st windows.SERVICE_STATUS
	return windows.ControlService(h, uint32(service.CheckNowControl), &st)
}

func openService(access uint32) (windows.Handle, error) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return 0, err
	}
	defer windows.CloseServiceHandle(scm)
	name, _ := windows.UTF16PtrFromString(service.Name)
	return windows.OpenService(scm, name, access)
}

func stateText(s svc.State, err error) string {
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return "non installato"
		}
		return "sconosciuto"
	}
	switch s {
	case svc.Running:
		return "in esecuzione"
	case svc.Stopped:
		return "fermo"
	case svc.StartPending:
		return "in avvio"
	case svc.StopPending:
		return "in arresto"
	default:
		return "sconosciuto"
	}
}

// openFolder opens dir in Explorer.
func openFolder(dir string) {
	_ = windows.ShellExecute(0, windows.StringToUTF16Ptr("open"), windows.StringToUTF16Ptr(dir), nil, nil, windows.SW_SHOWNORMAL)
}
