//go:build windows

package tray

import (
	"syscall"
	"unsafe"

	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Dark/light theme for plain Win32 controls, chosen by the user in the
// settings window ("Tema": Sistema, Chiaro, Scuro; see themeMode). "Sistema"
// follows Windows' own app mode (Settings > Personalization > Colors),
// switched live when it changes. The theme is applied through:
//   - the title bar through DWM (immersive dark mode, Windows 10 2004+);
//   - backgrounds and text through WM_ERASEBKGND / WM_CTLCOLOR*;
//   - the controls' visual styles through SetWindowTheme("DarkMode_*");
//   - popup menus (the tray menu) through uxtheme's undocumented
//     SetPreferredAppMode/FlushMenuThemes (ordinals 135/136, Windows 10 1903+).
//
// Every step is best-effort: on an older build an attribute or ordinal that
// does not exist leaves the light look, never an error.
//
// The settings window draws most of itself (fluent.go); this covers the
// native controls left in it (edits, the viewport's scroll bar).

type palette struct {
	bg, editBg, text   win.COLORREF
	bgBrush, editBrush win.HBRUSH // created once, kept for the process lifetime
}

var (
	gdi32                = windows.NewLazySystemDLL("gdi32.dll")
	procCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")
	uxtheme              = windows.NewLazySystemDLL("uxtheme.dll")
	procSetWindowTheme   = uxtheme.NewProc("SetWindowTheme")
	dwmapi               = windows.NewLazySystemDLL("dwmapi.dll")
	procDwmSetWindowAttr = dwmapi.NewProc("DwmSetWindowAttribute")

	lightPal = newPalette(win.RGB(240, 240, 240), win.RGB(255, 255, 255), win.RGB(0, 0, 0))
	darkPal  = newPalette(win.RGB(32, 32, 32), win.RGB(45, 45, 45), win.RGB(240, 240, 240))
	curPal   = &lightPal
)

func newPalette(bg, editBg, text win.COLORREF) palette {
	brush := func(c win.COLORREF) win.HBRUSH {
		h, _, _ := procCreateSolidBrush.Call(uintptr(c))
		return win.HBRUSH(h)
	}
	return palette{bg: bg, editBg: editBg, text: text, bgBrush: brush(bg), editBrush: brush(editBg)}
}

// themedWindow is what ui.Main and ui.Modal have in common.
type themedWindow interface {
	Hwnd() win.HWND
	On() *ui.WindowEvents
}

// installTheme registers the paint handlers on a window and applies the
// current theme once it exists. Call before the window is created.
func installTheme(w themedWindow) {
	w.On().Wm(co.WM_ERASEBKGND, func(p ui.Wm) uintptr {
		hdc := win.HDC(p.WParam)
		if rc, err := w.Hwnd().GetClientRect(); err == nil {
			hdc.FillRect(&rc, curPal.bgBrush)
		}
		return 1
	})

	// Labels and push buttons; also disabled and
	// read-only edits, which Windows paints as statics.
	paintCtl := func(p ui.Wm) uintptr {
		hdc := win.HDC(p.WParam)
		hdc.SetTextColor(curPal.text)
		hdc.SetBkColor(curPal.bg)
		return uintptr(curPal.bgBrush)
	}
	w.On().Wm(co.WM_CTLCOLORSTATIC, paintCtl)
	w.On().Wm(co.WM_CTLCOLORBTN, paintCtl)

	// Text boxes and the combo's drop-down list.
	paintEdit := func(p ui.Wm) uintptr {
		hdc := win.HDC(p.WParam)
		hdc.SetTextColor(curPal.text)
		hdc.SetBkColor(curPal.editBg)
		return uintptr(curPal.editBrush)
	}
	w.On().Wm(co.WM_CTLCOLOREDIT, paintEdit)
	w.On().Wm(co.WM_CTLCOLORLISTBOX, paintEdit)
}

// Theme modes, in the order of the settings window's radios. The zero value
// is "Sistema", so a user who never chose gets Windows' own app mode.
const (
	themeSystem = iota
	themeLight
	themeDark
)

// themeKey/themeValue hold the user's choice: per user, like the tray
// itself, and outside config.ini, which is machine-wide and admin-only. The
// key keeps the product's former name: following ProductName would forget
// every user's saved theme at each rename.
var (
	themeKey   = `Software\AryxD Agent\Tray`
	themeValue = "Theme"
)

// themeMode is the theme in use, which can be a not-yet-saved preview from
// the settings window. UI thread only.
var themeMode = loadThemeMode()

// loadThemeMode reads the user's saved choice; themeSystem when there is
// none or it is out of range.
func loadThemeMode() int {
	k, err := registry.OpenKey(registry.CURRENT_USER, themeKey, registry.QUERY_VALUE)
	if err != nil {
		return themeSystem
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue(themeValue)
	if err != nil || v > themeDark {
		return themeSystem
	}
	return int(v)
}

func saveThemeMode(mode int) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, themeKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetDWordValue(themeValue, uint32(mode))
}

// isDark reports whether themeMode currently means the dark palette.
func isDark() bool {
	switch themeMode {
	case themeLight:
		return false
	case themeDark:
		return true
	default:
		return systemDark()
	}
}

// systemDark reports whether Windows' app mode is dark (AppsUseLightTheme = 0).
func systemDark() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("AppsUseLightTheme")
	return err == nil && v == 0
}

// applyTheme switches w (frame, palette, controls) to themeMode and repaints
// it. Safe to call again whenever the theme changes.
func applyTheme(w themedWindow) {
	dark := isDark()
	if dark {
		curPal = &darkPal
	} else {
		curPal = &lightPal
	}
	setDarkMenus(dark)
	applyFrame(w.Hwnd(), dark)

	allowDark(w.Hwnd(), dark)
	for _, h := range w.Hwnd().EnumChildWindows() {
		class, _ := h.GetClassName()
		// Without it the DarkMode_* styles below stay light on some classes.
		allowDark(h, dark)
		if !dark {
			setWindowTheme(h, "")
			continue
		}
		switch class {
		case "Edit":
			setWindowTheme(h, "DarkMode_CFD")
		}
	}
	w.Hwnd().RedrawWindow(nil, 0, co.RDW_INVALIDATE|co.RDW_ERASE|co.RDW_ALLCHILDREN|co.RDW_UPDATENOW)
}

// isThemeChange reports whether a WM_SETTINGCHANGE is the app-mode switch.
func isThemeChange(p ui.Wm) bool {
	if p.LParam == 0 {
		return false
	}
	// LParam is a pointer to the setting's name; reinterpret it in place so
	// the uintptr never travels on its own (go vet's unsafeptr rule).
	name := *(**uint16)(unsafe.Pointer(&p.LParam))
	return windows.UTF16PtrToString(name) == "ImmersiveColorSet"
}

// wmSettingChange is WM_SETTINGCHANGE (= WM_WININICHANGE).
const wmSettingChange = co.WM(0x001A)

func setWindowTheme(h win.HWND, name string) {
	var p *uint16
	if name != "" {
		p, _ = windows.UTF16PtrFromString(name)
	}
	procSetWindowTheme.Call(uintptr(h), uintptr(unsafe.Pointer(p)), 0)
}

// DWM window attributes (dwmapi.h).
const (
	dwmwaUseImmersiveDarkMode = 20
	dwmwaWindowCornerPref     = 33
	dwmwcpRound               = 2
	dwmwaSystemBackdropType   = 38
	dwmsbtMainWindow          = 2 // Mica
)

func applyFrame(h win.HWND, dark bool) {
	set := func(attr uint32, v int32) {
		procDwmSetWindowAttr.Call(uintptr(h), uintptr(attr), uintptr(unsafe.Pointer(&v)), unsafe.Sizeof(v))
	}
	d := int32(0)
	if dark {
		d = 1
	}
	set(dwmwaUseImmersiveDarkMode, d)
	set(dwmwaWindowCornerPref, dwmwcpRound)        // Windows 11 rounded corners
	set(dwmwaSystemBackdropType, dwmsbtMainWindow) // Mica title bar (Windows 11 22H2+)
}

// setDarkMenus switches popup menus through uxtheme's undocumented
// SetPreferredAppMode (135) and FlushMenuThemes (136).
func setDarkMenus(dark bool) {
	if uxtheme.Load() != nil {
		return
	}
	mod := windows.Handle(uxtheme.Handle())
	setMode, err := windows.GetProcAddressByOrdinal(mod, 135)
	if err != nil {
		return
	}
	const appModeForceDark, appModeForceLight = 2, 3
	mode := uintptr(appModeForceLight)
	if dark {
		mode = appModeForceDark
	}
	syscall.SyscallN(setMode, mode)
	// RefreshImmersiveColorPolicyState (104): makes the new mode reach
	// controls themed after this point, not only menus.
	if refresh, err := windows.GetProcAddressByOrdinal(mod, 104); err == nil {
		syscall.SyscallN(refresh)
	}
	if flush, err := windows.GetProcAddressByOrdinal(mod, 136); err == nil {
		syscall.SyscallN(flush)
	}
}

// allowDark calls uxtheme's undocumented AllowDarkModeForWindow (ordinal
// 133, Windows 10 1809+), which the DarkMode_* visual styles require on some
// control classes before they actually draw dark.
func allowDark(h win.HWND, dark bool) {
	if uxtheme.Load() != nil {
		return
	}
	fn, err := windows.GetProcAddressByOrdinal(windows.Handle(uxtheme.Handle()), 133)
	if err != nil {
		return
	}
	on := uintptr(0)
	if dark {
		on = 1
	}
	syscall.SyscallN(fn, uintptr(h), on)
}
