// Package toast shows an ephemeral Windows notification-area balloon
// carrying EMLy's own icon (extracted at runtime from the installed
// EMLy.exe, so it always matches whatever ships there - nothing to keep in
// sync in this repo). Unlike an AppUserModelID-registered WinRT toast (which
// needs a Start Menu shortcut and registry plumbing), a Shell_NotifyIcon
// NIIF_USER/NIIF_LARGE_ICON balloon needs no registration at all, and
// Windows 10/11 render it identically to a toast, including an Action
// Center entry.
//
// Show must run inside the target user's desktop session - session 0, where
// the SYSTEM service lives, has no desktop to draw a notification icon on.
// See internal/notify.LaunchToast for the SYSTEM -> user-session hop.
package toast

import (
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32  = windows.NewLazySystemDLL("user32.dll")
	shell32 = windows.NewLazySystemDLL("shell32.dll")

	procGetModuleHandle  = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetModuleHandleW")
	procRegisterClassEx  = user32.NewProc("RegisterClassExW")
	procUnregisterClass  = user32.NewProc("UnregisterClassW")
	procCreateWindowEx   = user32.NewProc("CreateWindowExW")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
	procDefWindowProc    = user32.NewProc("DefWindowProcW")
	procGetMessage       = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessage  = user32.NewProc("DispatchMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procSetTimer         = user32.NewProc("SetTimer")
	procDestroyIcon      = user32.NewProc("DestroyIcon")
	procShellNotifyIcon  = shell32.NewProc("Shell_NotifyIconW")
	procExtractIconEx    = shell32.NewProc("ExtractIconExW")
)

const (
	wmDestroy = 0x0002
	wmClose   = 0x0010
	wmTimer   = 0x0113

	nifIcon = 0x00000002
	nifTip  = 0x00000004
	nifInfo = 0x00000010

	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	niifUser  = 0x00000004
	niifLarge = 0x00000020

	timerID = 1

	// visibleFor bounds how long the helper process pumps messages after
	// showing the balloon; the shell keeps the Action Center entry after our
	// tray icon and window are gone.
	visibleFor = 10 * time.Second
)

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     windows.Handle
	hIcon         windows.Handle
	hCursor       windows.Handle
	hbrBackground windows.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       windows.Handle
}

type point struct{ x, y int32 }

type msgT struct {
	hwnd    windows.Handle
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

// notifyIconDataW mirrors the full (Vista+) NOTIFYICONDATAW layout, matched
// field-for-field so Go's natural struct alignment on amd64 reproduces the C
// layout - see the guidItem/hBalloonIcon tail added for the custom-icon
// balloon.
type notifyIconDataW struct {
	cbSize            uint32
	hWnd              windows.Handle
	uID               uint32
	uFlags            uint32
	uCallbackMessage  uint32
	hIcon             windows.Handle
	szTip             [128]uint16
	dwState           uint32
	dwStateMask       uint32
	szInfo            [256]uint16
	uTimeoutOrVersion uint32
	szInfoTitle       [64]uint16
	dwInfoFlags       uint32
	guidItem          windows.GUID
	hBalloonIcon      windows.Handle
}

// Show extracts iconExePath's icon and displays title/body as a
// notification-area balloon, blocking for visibleFor while pumping window
// messages. iconExePath may be unreadable or lack an icon resource - the
// balloon still shows, falling back to the system "info" icon.
func Show(iconExePath, title, body string) error {
	className, err := windows.UTF16PtrFromString("EMLyUpdaterToast")
	if err != nil {
		return err
	}

	hInstance, _, _ := procGetModuleHandle.Call(0)

	wndProc := windows.NewCallback(func(hwnd windows.Handle, msg uint32, wParam, lParam uintptr) uintptr {
		if msg == wmTimer || msg == wmClose || msg == wmDestroy {
			procPostQuitMessage.Call(0)
			return 0
		}
		ret, _, _ := procDefWindowProc.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
		return ret
	})

	var wc wndClassExW
	wc.lpfnWndProc = wndProc
	wc.hInstance = windows.Handle(hInstance)
	wc.lpszClassName = className
	wc.cbSize = uint32(unsafe.Sizeof(wc))

	if atom, _, err := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		return fmt.Errorf("RegisterClassEx failed: %w", err)
	}
	defer procUnregisterClass.Call(uintptr(unsafe.Pointer(className)), hInstance)

	hwnd, _, err := procCreateWindowEx.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(className)),
		0, // no style, in particular no WS_VISIBLE: this window is never shown
		0, 0, 0, 0,
		0, // hWndParent: top-level, so Shell_NotifyIcon's shell-side tracking works reliably
		0,
		hInstance,
		0,
	)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowEx failed: %w", err)
	}
	defer procDestroyWindow.Call(hwnd)

	smallIcon, largeIcon, iconErr := extractIcons(iconExePath)
	if smallIcon != 0 {
		defer procDestroyIcon.Call(uintptr(smallIcon))
	}
	if largeIcon != 0 {
		defer procDestroyIcon.Call(uintptr(largeIcon))
	}

	var nid notifyIconDataW
	nid.cbSize = uint32(unsafe.Sizeof(nid))
	nid.hWnd = windows.Handle(hwnd)
	nid.uID = 1
	nid.uFlags = nifTip
	if smallIcon != 0 {
		nid.uFlags |= nifIcon
		nid.hIcon = smallIcon
	}
	copyString(nid.szTip[:], title)

	if ret, _, _ := procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&nid))); ret == 0 {
		return fmt.Errorf("Shell_NotifyIcon(NIM_ADD) failed")
	}
	defer procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))

	nid.uFlags = nifInfo
	copyString(nid.szInfo[:], body)
	copyString(nid.szInfoTitle[:], title)
	// niifLarge alone (no NIIF_USER) falls back to the system "info" icon at
	// large size when icon extraction failed.
	nid.dwInfoFlags = niifLarge
	if largeIcon != 0 {
		nid.dwInfoFlags |= niifUser
		nid.hBalloonIcon = largeIcon
	}
	if ret, _, _ := procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&nid))); ret == 0 {
		return fmt.Errorf("Shell_NotifyIcon(NIM_MODIFY) failed")
	}

	procSetTimer.Call(hwnd, timerID, uintptr(visibleFor/time.Millisecond), 0)

	var m msgT
	for {
		ret, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}

	// Icon extraction failure never fails the whole call - the balloon still
	// showed, just without EMLy's icon - but it is worth surfacing to the
	// caller's logs.
	return iconErr
}

func copyString(dst []uint16, s string) {
	u, err := windows.UTF16FromString(s)
	if err != nil {
		return // embedded NUL: leave dst zeroed (empty string)
	}
	n := len(u)
	if n > len(dst) {
		n = len(dst)
		dst[n-1] = 0
	}
	copy(dst, u[:n])
}

// extractIcons returns the small (tray, ~16px) and large (balloon, ~32px)
// icons embedded in exePath's first icon resource. A non-nil error is
// informational only - callers may proceed with zero handles.
func extractIcons(exePath string) (small, large windows.Handle, err error) {
	p, err := windows.UTF16PtrFromString(exePath)
	if err != nil {
		return 0, 0, err
	}
	var largeH, smallH windows.Handle
	ret, _, callErr := procExtractIconEx.Call(
		uintptr(unsafe.Pointer(p)), 0,
		uintptr(unsafe.Pointer(&largeH)), uintptr(unsafe.Pointer(&smallH)), 1,
	)
	if ret == 0 {
		return 0, 0, fmt.Errorf("ExtractIconEx failed for %s: %w", exePath, callErr)
	}
	return smallH, largeH, nil
}
