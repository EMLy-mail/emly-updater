//go:build windows

package tray

// fluentCombo is the WinUI ComboBox: a button-like box showing the selected
// item and a chevron, which opens a list in a popup laid over it, the
// selected item exactly over the box. A native ComboBox cannot be restyled
// that way - its edit, button and drop-down list are drawn by the system,
// and the list is a separate window with its own (square, light) look - so
// both halves are drawn here.
//
// The popup never takes activation or focus (WS_EX_NOACTIVATE, SW_SHOWNA):
// the settings window keeps its active title bar and the combo keeps the
// keyboard, as with a native drop-down list. It holds the mouse capture
// instead, so a click anywhere else in the window closes it; a click in
// another application deactivates this one, which takes focus and capture
// away and closes it too.

import (
	"syscall"
	"unsafe"

	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
	"golang.org/x/sys/windows"
)

// Popup metrics, 96-DPI units.
const (
	comboHeight     = 32
	comboItemH      = 36
	comboPopupPad   = 4 // above the first item and below the last
	comboItemMargin = 4 // between the popup's edge and an item's fill
	comboTextPad    = 12
	comboPopupRound = 8 // Windows 11's rounding for popups (DWMWCP_ROUND)
)

type fluentCombo struct {
	*ctlBase
	items    []string
	sel      int
	onChange func()

	popup win.HWND // open list, 0 while closed
	hi    int      // item the keyboard is on while open
	hot   int      // item under the mouse while open, -1 for none
	down  bool     // left button pressed inside the popup
}

func newFluentCombo(parent ui.Parent, x, y, width int, items []string) *fluentCombo {
	cb := &fluentCombo{items: items, hot: -1}
	cb.ctlBase = newCtlBase(parent, x-focusPad, y-focusPad, width+2*focusPad, comboHeight+2*focusPad, true,
		func() argb { return fp().cardBg })
	cb.paint = cb.draw
	cb.dlgCode = co.DLGC_WANTARROWS
	cb.click = func(win.POINT) { cb.open() }
	cb.key = cb.keyDown
	cb.blur = func() { cb.close(false) }

	on := cb.ctl.On()
	on.Wm(co.WM_SYSKEYDOWN, func(p ui.Wm) uintptr {
		// Alt+Down / Alt+Up open and close the list, as on a native combo.
		if vk := co.VK(p.WParam); (vk == co.VK_DOWN || vk == co.VK_UP) && cb.enabled() {
			if cb.popup == 0 {
				cb.open()
			} else {
				cb.close(true)
			}
			return 0
		}
		return cb.hwnd().DefWindowProc(p.Msg, p.WParam, p.LParam)
	})
	// The wheel goes to the focused window. Closed, DefWindowProc hands it
	// to the parent, so it scrolls the page instead of changing the
	// selection; open, it is dropped, so the page does not move under the
	// list.
	on.Wm(wmMouseWheel, func(p ui.Wm) uintptr {
		if cb.popup != 0 {
			return 0
		}
		return cb.hwnd().DefWindowProc(p.Msg, p.WParam, p.LParam)
	})
	on.WmDestroy(func() { cb.close(false) })
	return cb
}

// Hwnd is the closed box's window, for EnableWindow and the like.
func (cb *fluentCombo) Hwnd() win.HWND { return cb.hwnd() }

func (cb *fluentCombo) SelectedIndex() int { return cb.sel }

// SelectIndex moves the selection without calling OnChange.
func (cb *fluentCombo) SelectIndex(i int) {
	if i >= 0 && i < len(cb.items) && i != cb.sel {
		cb.sel = i
		cb.redraw()
	}
}

// OnChange runs when the user picks a different item, not on SelectIndex.
func (cb *fluentCombo) OnChange(fun func()) { cb.onChange = fun }

func (cb *fluentCombo) Enable(on bool) {
	if !on {
		cb.close(false)
	}
	cb.hwnd().EnableWindow(on)
}

// pick makes i the selection, calling OnChange if it changed.
func (cb *fluentCombo) pick(i int) {
	if i < 0 || i >= len(cb.items) || i == cb.sel {
		return
	}
	cb.sel = i
	cb.redraw()
	if cb.onChange != nil {
		cb.onChange()
	}
}

func (cb *fluentCombo) keyDown(vk co.VK) bool {
	if cb.popup == 0 {
		switch vk {
		case co.VK_SPACE, co.VK_RETURN, co.VK_F4:
			cb.open()
		case co.VK_UP:
			cb.pick(max(cb.sel-1, 0))
		case co.VK_DOWN:
			cb.pick(min(cb.sel+1, len(cb.items)-1))
		case co.VK_HOME:
			cb.pick(0)
		case co.VK_END:
			cb.pick(len(cb.items) - 1)
		default:
			return false
		}
		return true
	}
	switch vk {
	case co.VK_UP:
		cb.moveHi(max(cb.hi-1, 0))
	case co.VK_DOWN:
		cb.moveHi(min(cb.hi+1, len(cb.items)-1))
	case co.VK_HOME:
		cb.moveHi(0)
	case co.VK_END:
		cb.moveHi(len(cb.items) - 1)
	case co.VK_SPACE, co.VK_RETURN, co.VK_F4, co.VK_TAB:
		cb.close(true)
	case co.VK_ESCAPE:
		cb.close(false)
	default:
		return false
	}
	return true
}

func (cb *fluentCombo) moveHi(i int) {
	if i != cb.hi {
		cb.hi = i
		cb.popup.InvalidateRect(nil, false)
	}
}

// face is the closed box's shape in client coordinates.
func (cb *fluentCombo) face(rc win.RECT) win.RECT { return inset(rc, focusPad) }

func (cb *fluentCombo) draw(c *canvas, rc win.RECT) {
	p := fp()
	face := cb.face(rc)
	radius := ui.DpiX(controlRadius)
	en := cb.enabled()

	fill, txt, chev := p.buttonBg, p.text, p.textSecondary
	switch {
	case !en:
		txt, chev = p.textDisabled, p.textDisabled
	case cb.pressed || cb.popup != 0:
		fill = p.buttonPressed
	case cb.hover:
		fill = p.buttonHover
	}
	c.fillRoundRect(face, radius, fill)
	half := int32(ui.DpiX(1)) / 2
	c.strokeRoundRect(win.RECT{Left: face.Left + half, Top: face.Top + half,
		Right: face.Right - half, Bottom: face.Bottom - half}, radius, p.buttonStroke, float32(ui.DpiX(1)))

	chevW := int32(ui.DpiX(32))
	g := win.RECT{Left: face.Right - chevW, Top: face.Top, Right: face.Right - int32(ui.DpiX(6)), Bottom: face.Bottom}
	c.glyph(glyphChevronDown, g, ui.DpiY(12), chev)
	if cb.sel >= 0 && cb.sel < len(cb.items) {
		t := win.RECT{Left: face.Left + int32(ui.DpiX(comboTextPad)), Top: face.Top, Right: g.Left, Bottom: face.Bottom}
		c.text(cb.items[cb.sel], t, fontBody, txt, co.DT_LEFT|co.DT_VCENTER|co.DT_SINGLELINE|co.DT_END_ELLIPSIS|co.DT_NOPREFIX)
	}

	if cb.showFocus() {
		paintFocus(c, face, controlRadius)
	}
}

// open shows the list over the box, the selected item where the box is.
func (cb *fluentCombo) open() {
	if cb.popup != 0 || len(cb.items) == 0 || !cb.enabled() || !registerComboPopupClass() {
		return
	}
	rc, _ := cb.hwnd().GetClientRect()
	face := cb.face(rc)
	_ = cb.hwnd().ClientToScreenRc(&face)

	itemH, pad, margin := int32(ui.DpiY(comboItemH)), int32(ui.DpiY(comboPopupPad)), int32(ui.DpiX(comboItemMargin))
	width := face.Right - face.Left + 2*margin
	for _, it := range cb.items {
		tw, _ := (*canvas)(nil).measure(it, fontBody)
		width = max(width, int32(tw+ui.DpiX(2*comboTextPad+16))+2*margin)
	}
	height := 2*pad + itemH*int32(len(cb.items))
	x := face.Left - margin
	itemTop := face.Top + (face.Bottom-face.Top-itemH)/2
	y := itemTop - pad - itemH*int32(cb.sel)

	// Inside the monitor's work area, even if the selected item then no
	// longer lines up with the box.
	if mi, err := cb.hwnd().MonitorFromWindow(co.MONITOR_DEFAULTTONEAREST).GetMonitorInfo(); err == nil {
		wa := mi.RcWork
		x = min(max(x, wa.Left), wa.Right-width)
		y = min(max(y, wa.Top), wa.Bottom-height)
	}

	owner, _ := cb.hwnd().GetAncestor(co.GA_ROOT)
	cls, _ := windows.UTF16PtrFromString(comboPopupClass)
	h, _, _ := procCreateWindowExW.Call(wsExToolWindow|wsExNoActivate, uintptr(unsafe.Pointer(cls)), 0,
		wsPopup, uintptr(x), uintptr(y), uintptr(width), uintptr(height), uintptr(owner), 0, 0, 0)
	if h == 0 {
		return
	}
	cb.popup = win.HWND(h)
	comboPopups[h] = cb
	cb.hi, cb.hot, cb.down = cb.sel, -1, false

	corner := int32(dwmwcpRound)
	procDwmSetWindowAttr.Call(h, dwmwaWindowCornerPref, uintptr(unsafe.Pointer(&corner)), unsafe.Sizeof(corner))
	border := uint32(fp().popupStroke.colorref())
	procDwmSetWindowAttr.Call(h, dwmwaBorderColor, uintptr(unsafe.Pointer(&border)), unsafe.Sizeof(border))

	// Open, the combo wants Esc, Enter and Tab too: IsDialogMessage would
	// turn them into IDCANCEL, the default button and a focus move.
	cb.dlgCode = co.DLGC_WANTARROWS | co.DLGC_WANTALLKEYS
	cb.popup.ShowWindow(co.SW_SHOWNA)
	cb.popup.SetCapture()
	cb.redraw()
}

// close takes the list down, picking the keyboard's item when commit is set.
func (cb *fluentCombo) close(commit bool) {
	h := cb.popup
	if h == 0 {
		return
	}
	cb.popup = 0 // first: ReleaseCapture below re-enters through WM_CAPTURECHANGED
	delete(comboPopups, uintptr(h))
	cb.dlgCode = co.DLGC_WANTARROWS
	_ = win.ReleaseCapture()
	_ = h.DestroyWindow()
	cb.redraw()
	if commit {
		cb.pick(cb.hi)
	}
}

// itemRect is item i's fill in the popup's client area.
func (cb *fluentCombo) itemRect(i int, rc win.RECT) win.RECT {
	top := rc.Top + int32(ui.DpiY(comboPopupPad)) + int32(i)*int32(ui.DpiY(comboItemH))
	m := int32(ui.DpiX(comboItemMargin))
	return win.RECT{Left: rc.Left + m, Top: top, Right: rc.Right - m, Bottom: top + int32(ui.DpiY(comboItemH))}
}

func (cb *fluentCombo) itemAt(x, y int32) int {
	rc, _ := cb.popup.GetClientRect()
	for i := range cb.items {
		r := cb.itemRect(i, rc)
		if x >= r.Left && x < r.Right && y >= r.Top && y < r.Bottom {
			return i
		}
	}
	return -1
}

func (cb *fluentCombo) drawPopup(c *canvas, rc win.RECT) {
	p := fp()
	c.fillRect(rc, p.popupBg)
	// The DWM border covers this on Windows 11; on Windows 10, whose
	// popups are square, it is the only edge.
	half := int32(ui.DpiX(1)) / 2
	c.strokeRoundRect(win.RECT{Left: rc.Left + half, Top: rc.Top + half, Right: rc.Right - half, Bottom: rc.Bottom - half},
		ui.DpiX(comboPopupRound), p.popupStroke, float32(ui.DpiX(1)))

	radius := ui.DpiX(controlRadius)
	for i, it := range cb.items {
		r := cb.itemRect(i, rc)
		// Hover wins over the selection, as in WinUI: the item under the
		// mouse is always the brightest.
		switch {
		case i == cb.hot:
			c.fillRoundRect(r, radius, p.popupHover)
		case i == cb.hi:
			c.fillRoundRect(r, radius, p.popupSelected)
		}
		if i == cb.hi {
			ph, pw := int32(ui.DpiY(16)), int32(ui.DpiX(3))
			cy := (r.Top + r.Bottom) / 2
			c.fillRoundRect(win.RECT{Left: r.Left, Top: cy - ph/2, Right: r.Left + pw, Bottom: cy - ph/2 + ph}, int(pw/2), p.navIndicator)
		}
		t := win.RECT{Left: r.Left + int32(ui.DpiX(comboTextPad)), Top: r.Top, Right: r.Right - int32(ui.DpiX(8)), Bottom: r.Bottom}
		c.text(it, t, fontBody, p.text, co.DT_LEFT|co.DT_VCENTER|co.DT_SINGLELINE|co.DT_END_ELLIPSIS|co.DT_NOPREFIX)
	}
}

const (
	comboPopupClass  = "AryxDComboPopup"
	wsExNoActivate   = 0x08000000
	dwmwaBorderColor = 34 // Windows 11; ignored before
	maNoActivate     = 3
)

var (
	comboPopupProc   = syscall.NewCallback(comboPopupWndProc)
	comboPopups      = map[uintptr]*fluentCombo{}
	comboPopupClassD bool
)

func registerComboPopupClass() bool {
	if comboPopupClassD {
		return true
	}
	cls, _ := windows.UTF16PtrFromString(comboPopupClass)
	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
	wc := wndClassEx{style: csDropShadow, lpfnWndProc: comboPopupProc, hCursor: cursor, lpszClass: cls}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	if r, _, _ := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return false
	}
	comboPopupClassD = true
	return true
}

// mousePos splits a mouse message's LPARAM: signed, since with the capture
// a point left of or above the popup is negative.
func mousePos(lp uintptr) (x, y int32) {
	return int32(int16(lp & 0xFFFF)), int32(int16(lp >> 16 & 0xFFFF))
}

func comboPopupWndProc(hwnd, msg, wp, lp uintptr) uintptr {
	cb := comboPopups[hwnd]
	if cb == nil {
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
		return r
	}
	switch co.WM(msg) {
	case co.WM_MOUSEACTIVATE:
		return maNoActivate
	case co.WM_ERASEBKGND:
		return 1
	case co.WM_PAINT:
		paintBuffered(win.HWND(hwnd), cb.drawPopup)
		return 0
	case co.WM_MOUSEMOVE:
		if hot := cb.itemAt(mousePos(lp)); hot != cb.hot {
			cb.hot = hot
			cb.popup.InvalidateRect(nil, false)
		}
		return 0
	case co.WM_LBUTTONDOWN, co.WM_LBUTTONDBLCLK:
		if cb.itemAt(mousePos(lp)) < 0 {
			rc, _ := cb.popup.GetClientRect()
			if x, y := mousePos(lp); x < rc.Left || x >= rc.Right || y < rc.Top || y >= rc.Bottom {
				cb.close(false) // outside: WinUI swallows the click
			}
			return 0
		}
		cb.down = true
		return 0
	case co.WM_LBUTTONUP:
		if i := cb.itemAt(mousePos(lp)); cb.down && i >= 0 {
			cb.hi = i
			cb.close(true)
		}
		cb.down = false
		return 0
	case co.WM_RBUTTONDOWN, co.WM_MBUTTONDOWN:
		rc, _ := cb.popup.GetClientRect()
		if x, y := mousePos(lp); x < rc.Left || x >= rc.Right || y < rc.Top || y >= rc.Bottom {
			cb.close(false)
		}
		return 0
	case co.WM_CAPTURECHANGED:
		// Someone else took the mouse: a menu, Alt+Tab, the window being
		// dragged by the keyboard. close is a no-op if it released it.
		cb.close(false)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}
