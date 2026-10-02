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

	"emlyupdater/internal/version"
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
// A themed checkbox draws its own text in black whatever WM_CTLCOLORBTN says,
// which is unreadable on the dark background: that is why the settings
// window draws each checkbox as a bare glyph plus a Static (checkRow), and
// each theme radio the same way (themeRow).

type palette struct {
	bg, editBg, text   win.COLORREF
	dim                win.COLORREF // text of a greyed-out label
	bgBrush, editBrush win.HBRUSH   // created once, kept for the process lifetime
}

var (
	gdi32                = windows.NewLazySystemDLL("gdi32.dll")
	procCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")
	uxtheme              = windows.NewLazySystemDLL("uxtheme.dll")
	procSetWindowTheme   = uxtheme.NewProc("SetWindowTheme")
	dwmapi               = windows.NewLazySystemDLL("dwmapi.dll")
	procDwmSetWindowAttr = dwmapi.NewProc("DwmSetWindowAttribute")

	lightPal = withDim(newPalette(win.RGB(240, 240, 240), win.RGB(255, 255, 255), win.RGB(0, 0, 0)), win.RGB(109, 109, 109))
	darkPal  = withDim(newPalette(win.RGB(32, 32, 32), win.RGB(45, 45, 45), win.RGB(240, 240, 240)), win.RGB(125, 125, 125))
	curPal   = &lightPal
)

func newPalette(bg, editBg, text win.COLORREF) palette {
	brush := func(c win.COLORREF) win.HBRUSH {
		h, _, _ := procCreateSolidBrush.Call(uintptr(c))
		return win.HBRUSH(h)
	}
	return palette{bg: bg, editBg: editBg, text: text, bgBrush: brush(bg), editBrush: brush(editBg)}
}

func withDim(p palette, dim win.COLORREF) palette {
	p.dim = dim
	return p
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

	// Labels, glyph-only checkboxes, push buttons; also disabled and
	// read-only edits, which Windows paints as statics.
	paintCtl := func(p ui.Wm) uintptr {
		hdc := win.HDC(p.WParam)
		if dimmed[win.HWND(p.LParam)] {
			hdc.SetTextColor(curPal.dim)
		} else {
			hdc.SetTextColor(curPal.text)
		}
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
// itself, and outside config.ini, which is machine-wide and admin-only.
var (
	themeKey   = `Software\` + version.ProductName + `\Tray`
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
		// Without it the DarkMode_* styles below stay light on push buttons
		// and list view headers.
		allowDark(h, dark)
		if !dark {
			setWindowTheme(h, "")
			if class == "SysListView32" {
				setListColors(h, lightPal.editBg, lightPal.text)
			}
			continue
		}
		switch class {
		case "Edit", "ComboBox":
			setWindowTheme(h, "DarkMode_CFD")
		case "Button":
			// Checkbox glyphs; push buttons ignore it and are drawn by
			// darkButton instead.
			setWindowTheme(h, "DarkMode_Explorer")
		case "SysListView32":
			setWindowTheme(h, "DarkMode_Explorer")
			setListColors(h, darkPal.editBg, darkPal.text)
		case "SysHeader32":
			setWindowTheme(h, "ItemsView") // with allowDark: dark header, light text
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

func setListColors(h win.HWND, bg, text win.COLORREF) {
	h.SendMessage(co.LVM_SETBKCOLOR, 0, win.LPARAM(bg))
	h.SendMessage(co.LVM_SETTEXTBKCOLOR, 0, win.LPARAM(bg))
	h.SendMessage(co.LVM_SETTEXTCOLOR, 0, win.LPARAM(text))
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

// Push button colours in dark mode.
var (
	btnFace    = newPalette(win.RGB(51, 51, 51), win.RGB(69, 69, 69), win.RGB(240, 240, 240))
	btnPressed = newPalette(win.RGB(40, 40, 40), win.RGB(40, 40, 40), win.RGB(240, 240, 240))
	btnBorder  = newPalette(win.RGB(110, 110, 110), win.RGB(150, 150, 150), win.RGB(0, 0, 0))
)

// darkButton paints a push button itself while the dark palette is active:
// no visual style draws a dark push button on every Windows build (neither
// "DarkMode_Explorer" nor "Explorer" + AllowDarkModeForWindow did on Windows
// 11 26100), so the face, border and caption come from NM_CUSTOMDRAW. In the
// light theme the system draws it. Register before the button is created.
func darkButton(b *ui.Button) {
	b.On().NmCustomDraw(func(p *win.NMCUSTOMDRAW) co.CDRF {
		if curPal != &darkPal || p.DwDrawStage != co.CDDS_PREPAINT {
			return co.CDRF_DODEFAULT
		}
		hdc, rc := p.Hdc, p.Rc
		hot := p.UItemState&co.CDIS_HOT != 0
		face, border := btnFace.bgBrush, btnBorder.bgBrush
		switch {
		case p.UItemState&co.CDIS_SELECTED != 0:
			face = btnPressed.bgBrush
		case hot:
			face, border = btnFace.editBrush, btnBorder.editBrush
		}
		hdc.FillRect(&rc, border) // 1px border: outer rect, then the face inset
		in := win.RECT{Left: rc.Left + 1, Top: rc.Top + 1, Right: rc.Right - 1, Bottom: rc.Bottom - 1}
		hdc.FillRect(&in, face)

		text, _ := b.Hwnd().GetWindowText()
		hdc.SetBkMode(co.BKMODE_TRANSPARENT)
		color := darkPal.text
		if p.UItemState&co.CDIS_DISABLED != 0 {
			color = win.RGB(120, 120, 120)
		}
		hdc.SetTextColor(color)
		hdc.DrawText(text, &in, co.DT_CENTER|co.DT_VCENTER|co.DT_SINGLELINE) // '&' still underlines
		return co.CDRF_SKIPDEFAULT
	})
}

// darkListHeader makes a list view's column titles readable in the dark
// theme. The dark header styles keep a dark-grey caption on the dark face,
// and they draw it with DrawThemeText, which ignores SetTextColor: so in the
// header's NM_CUSTOMDRAW - sent to the list view, its parent - each item is
// drawn here instead. windigo routes WM_NOTIFY by (ID, code) only
// (WmNotify), never through a plain Wm(WM_NOTIFY) handler, and the header's
// ID is whatever comctl32 gave it: fixHeaderID pins it to headerID once the
// list view exists. Register before the list view is created.
func darkListHeader(lv *ui.ListView) {
	lv.OnSubclass().WmNotify(headerID, co.NM_CUSTOMDRAW, func(ptr unsafe.Pointer) uintptr {
		cd := (*win.NMCUSTOMDRAW)(ptr)
		if curPal != &darkPal {
			return lv.Hwnd().DefSubclassProc(co.WM_NOTIFY, headerID, win.LPARAM(ptr))
		}
		switch cd.DwDrawStage {
		case co.CDDS_PREPAINT:
			return uintptr(co.CDRF_NOTIFYITEMDRAW)
		case co.CDDS_ITEMPREPAINT:
			hdc, rc := cd.Hdc, cd.Rc
			hdc.FillRect(&rc, darkPal.editBrush)
			sep := win.RECT{Left: rc.Right - 1, Top: rc.Top + 4, Right: rc.Right, Bottom: rc.Bottom - 4}
			hdc.FillRect(&sep, btnBorder.bgBrush)
			title := ""
			if i := int(cd.DwItemSpec); i < lv.ColCount() {
				title = lv.Col(i).Title()
			}
			hdc.SetBkMode(co.BKMODE_TRANSPARENT)
			hdc.SetTextColor(darkPal.text)
			text := win.RECT{Left: rc.Left + 6, Top: rc.Top, Right: rc.Right - 6, Bottom: rc.Bottom}
			hdc.DrawText(title, &text, co.DT_LEFT|co.DT_VCENTER|co.DT_SINGLELINE|co.DT_END_ELLIPSIS|co.DT_NOPREFIX)
			return uintptr(co.CDRF_SKIPDEFAULT)
		}
		return uintptr(co.CDRF_DODEFAULT)
	})
}

// headerID is the control ID darkListHeader expects on a list view's header.
const headerID = 0x7F01

// fixHeaderID gives every list view header under w the ID darkListHeader
// listens on. Call once the controls exist (WM_CREATE).
func fixHeaderID(w themedWindow) {
	for _, h := range w.Hwnd().EnumChildWindows() {
		if class, _ := h.GetClassName(); class == "SysHeader32" {
			h.SetWindowLongPtr(co.GWLP_ID, headerID)
		}
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

// checkRow is a checkbox drawn as a bare glyph plus a Static for its text
// (see the package comment above on why). Clicking the text toggles the box
// exactly as clicking the glyph does, BN_CLICKED handlers included.
type checkRow struct {
	box     *ui.CheckBox
	lbl     *ui.Static
	enabled bool
}

func newCheckRow(parent ui.Parent, text string, x, y, width int) *checkRow {
	c := &checkRow{enabled: true}
	c.box = ui.NewCheckBox(parent, ui.OptsCheckBox().Position(ui.Dpi(x, y)).Size(ui.Dpi(18, 20)))
	c.lbl = ui.NewStatic(parent, ui.OptsStatic().Text(text).Position(ui.Dpi(x+20, y+3)).Size(ui.Dpi(width-20, 16)))
	c.lbl.On().StnClicked(func() {
		if c.enabled {
			c.box.SetCheckAndTrigger(!c.box.IsChecked())
		}
	})
	return c
}

func (c *checkRow) IsChecked() bool    { return c.box.IsChecked() }
func (c *checkRow) SetCheck(on bool)   { c.box.SetCheck(on) }
func (c *checkRow) OnClick(fun func()) { c.box.On().BnClicked(fun) }

// Enable greys the row out. The label is not disabled - a disabled Static
// is drawn etched, unreadable on the dark background - but painted in the
// palette's dim colour (see dimmed), and its click is ignored.
func (c *checkRow) Enable(on bool) {
	c.enabled = on
	c.box.Hwnd().EnableWindow(on)
	dimmed[c.lbl.Hwnd()] = !on
	c.lbl.Hwnd().InvalidateRect(nil, true)
}

// dimmed are the Statics painted in the dim colour: checkRow labels of
// disabled rows. UI thread only.
var dimmed = map[win.HWND]bool{}

// themeRow is the settings window's "Tema" choice: one radio per theme mode,
// each a bare glyph plus a Static like checkRow. Clicking either part
// previews the theme at once (onPick); the window saves or reverts it.
type themeRow struct {
	radios []*ui.RadioButton
}

func newThemeRow(parent ui.Parent, x, y int, onPick func(mode int)) *themeRow {
	names := []string{"Sistema", "Chiaro", "Scuro"} // themeSystem, themeLight, themeDark
	opts := make([]*ui.VarOptsRadioButton, len(names))
	for i := range names {
		opts[i] = ui.OptsRadioButton().Position(ui.Dpi(x+i*90, y)).Size(ui.Dpi(18, 20))
	}
	g := ui.NewRadioGroup(parent, opts...)
	t := &themeRow{}
	for i, name := range names {
		mode := i
		t.radios = append(t.radios, g.Get(i))
		lbl := ui.NewStatic(parent, ui.OptsStatic().Text(name).Position(ui.Dpi(x+i*90+20, y+3)).Size(ui.Dpi(65, 16)))
		lbl.On().StnClicked(func() {
			t.Select(mode)
			onPick(mode)
		})
	}
	g.On().BnClicked(func(r *ui.RadioButton) { onPick(r.Index()) })
	return t
}

// Select checks one radio and unchecks the others: BM_SETCHECK does not do
// the mutual exclusion a mouse click on a radio does.
func (t *themeRow) Select(mode int) {
	for i, r := range t.radios {
		state := co.BST_UNCHECKED
		if i == mode {
			state = co.BST_CHECKED
		}
		r.Hwnd().SendMessage(co.BM_SETCHECK, win.WPARAM(state), 0)
	}
}
