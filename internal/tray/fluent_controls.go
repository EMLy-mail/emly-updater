//go:build windows

package tray

// Custom child controls for the Fluent settings window.
//
// Every control is a ui.Control child window painted with paintBuffered,
// with WS_TABSTOP where it takes input, keyboard support (Space/Enter,
// arrows for navList), a focus rectangle when focused from the keyboard,
// hover/pressed states, and fp() colours read at paint time so applyTheme's
// redraw is enough to switch theme. Positions and sizes are 96-DPI units.
// Constructors must be called before the parent window is created (windigo).
//
// Focusable controls (toggleSwitch, fluentButton) are focusPad larger than
// their shape on every side, so the focus rectangle can sit outside the
// shape as in WinUI; x, y still place the shape itself. The margin is
// painted in the backdrop colour (setBackdrop), which must match what the
// control sits on.

import (
	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
)

// focusPad is the margin around a focusable control's shape, 96-DPI units.
const focusPad = 3

// uisfHideFocus is UISF_HIDEFOCUS in WM_QUERYUISTATE's result: set while the
// window was last used with the mouse, cleared by IsDialogMessage on Tab.
const uisfHideFocus = 0x1

// hideFocusCues starts a window with keyboard cues hidden, as a dialog
// opened with the mouse does: without it the focused control (the nav list)
// shows its focus rectangle from the first frame, which WinUI shows only
// after the keyboard is used. Tab or an arrow key clears it again
// (IsDialogMessage sends WM_CHANGEUISTATE).
func hideFocusCues(h win.HWND) {
	const uisSet, uisfHideAccel = 1, 0x2
	h.SendMessage(co.WM_CHANGEUISTATE, win.MAKEWPARAM(uisSet, uisfHideFocus|uisfHideAccel), 0)
}

// ctlBase is what every control shares: the window, the backdrop colour and
// the mouse/keyboard plumbing. The concrete control supplies the hooks.
type ctlBase struct {
	ctl      *ui.Control
	backdrop func() argb
	hover    bool
	pressed  bool
	tracking bool // TrackMouseEvent armed for WM_MOUSELEAVE

	paint   func(c *canvas, rc win.RECT)
	click   func(pt win.POINT) // left button released inside, mouse coords
	press   func(pt win.POINT) // optional: left button down (fluentScrollBar)
	key     func(vk co.VK) bool
	move    func(pt win.POINT) // optional: hover position (navList)
	focus   func()             // optional: after WM_SETFOCUS (fluentCombo)
	blur    func()             // optional: after WM_KILLFOCUS (fluentCombo)
	dlgCode co.DLGC
}

func newCtlBase(parent ui.Parent, x, y, width, height int, focusable bool, backdrop func() argb) *ctlBase {
	style := co.WS_CHILD | co.WS_VISIBLE | co.WS_CLIPSIBLINGS
	if focusable {
		style |= co.WS_TABSTOP
	}
	b := &ctlBase{backdrop: backdrop}
	b.ctl = ui.NewControl(parent, ui.OptsControl().
		Position(ui.Dpi(x, y)).
		Size(ui.Dpi(width, height)).
		Style(style).
		ExStyle(0))
	b.events(focusable)
	return b
}

func (b *ctlBase) hwnd() win.HWND { return b.ctl.Hwnd() }

func (b *ctlBase) redraw() {
	if h := b.hwnd(); h != 0 {
		h.InvalidateRect(nil, false)
	}
}

// enabled reads WS_DISABLED rather than keeping a flag, so EnableWindow
// from anywhere (setAllEnabled) is reflected at once.
func (b *ctlBase) enabled() bool {
	st, _ := b.hwnd().GetWindowLongPtr(co.GWLP_STYLE)
	return co.WS(st)&co.WS_DISABLED == 0
}

// showFocus reports whether to draw the focus rectangle: focused, and the
// window's UI state says the keyboard is in use.
func (b *ctlBase) showFocus() bool {
	h := b.hwnd()
	if win.GetFocus() != h {
		return false
	}
	state, _ := h.SendMessage(co.WM_QUERYUISTATE, 0, 0)
	return state&uisfHideFocus == 0
}

func (b *ctlBase) events(focusable bool) {
	on := b.ctl.On()
	on.WmEraseBkgnd(func(ui.WmEraseBkgnd) int { return 1 })
	on.WmPaint(func() {
		paintBuffered(b.hwnd(), func(c *canvas, rc win.RECT) {
			c.fillRect(rc, b.backdrop())
			if b.paint != nil {
				b.paint(c, rc)
			}
		})
	})
	on.WmMouseMove(func(p ui.WmMouse) {
		if !b.tracking {
			tme := win.TRACKMOUSEEVENT{DwFlags: co.TME_LEAVE, HwndTrack: b.hwnd()}
			tme.SetCbSize()
			if win.TrackMouseEvent(&tme) == nil {
				b.tracking = true
			}
		}
		if b.move != nil {
			b.move(p.Pos())
		}
		if !b.hover {
			b.hover = true
			b.redraw()
		}
	})
	on.WmMouseLeave(func() {
		b.tracking = false
		b.hover = false
		if b.move != nil {
			b.move(win.POINT{X: -1, Y: -1})
		}
		b.redraw()
	})
	on.WmLButtonDown(func(p ui.WmMouse) {
		if focusable {
			b.hwnd().SetFocus()
		}
		b.hwnd().SetCapture()
		b.pressed = true
		if b.press != nil {
			b.press(p.Pos())
		}
		b.redraw()
	})
	on.WmLButtonDblClk(func(p ui.WmMouse) { // CS_DBLCLKS: a fast second click
		b.hwnd().SetCapture()
		b.pressed = true
		b.redraw()
	})
	on.WmLButtonUp(func(p ui.WmMouse) {
		was := b.pressed
		b.pressed = false
		win.ReleaseCapture()
		b.redraw()
		pt := p.Pos()
		rc, _ := b.hwnd().GetClientRect()
		inside := pt.X >= rc.Left && pt.X < rc.Right && pt.Y >= rc.Top && pt.Y < rc.Bottom
		if was && inside && b.click != nil && b.enabled() {
			b.click(pt)
		}
	})
	on.WmCaptureChanged(func(ui.WmCaptureChanged) {
		if b.pressed {
			b.pressed = false
			b.redraw()
		}
	})
	on.WmKeyDown(func(p ui.WmKey) {
		if b.key != nil && b.enabled() {
			b.key(p.VirtualKeyCode())
		}
	})
	// IsDialogMessage turns Enter into the default button and never
	// delivers it: claim that one key so it activates the focused control.
	on.WmGetDlgCode(func(p ui.WmGetDlgCode) co.DLGC {
		if !p.IsQuery() && p.Message().Msg == co.WM_KEYDOWN && p.VirtualKeyCode() == co.VK_RETURN {
			return b.dlgCode | co.DLGC_WANTMESSAGE
		}
		return b.dlgCode
	})
	on.WmSetFocus(func(ui.WmSetFocus) {
		b.redraw()
		if b.focus != nil {
			b.focus()
		}
	})
	on.WmKillFocus(func(ui.WmKillFocus) {
		b.redraw()
		if b.blur != nil {
			b.blur()
		}
	})
	on.WmEnable(func(ui.WmEnable) { b.redraw() })
	on.Wm(co.WM_UPDATEUISTATE, func(p ui.Wm) uintptr {
		r := b.hwnd().DefWindowProc(p.Msg, p.WParam, p.LParam) // keeps the state itself
		b.redraw()
		return r
	})
}

// setBackdrop replaces the colour painted behind the control's shape.
func (b *ctlBase) setBackdrop(fn func() argb) {
	b.backdrop = fn
	b.redraw()
}

// inset shrinks rc by n 96-DPI units on every side.
func inset(rc win.RECT, n int) win.RECT {
	dx, dy := int32(ui.DpiX(n)), int32(ui.DpiY(n))
	return win.RECT{Left: rc.Left + dx, Top: rc.Top + dy, Right: rc.Right - dx, Bottom: rc.Bottom - dy}
}

// paintFocus draws the keyboard focus rectangle just outside shape.
func paintFocus(c *canvas, shape win.RECT, radius int) {
	ring := inset(shape, -2)
	c.strokeRoundRect(ring, ui.DpiX(radius+2), fp().focusStroke, float32(ui.DpiX(2)))
}

// toggleSwitch is the WinUI ToggleSwitch: 40x20 pill, no label (the row
// label is painted by the page).
type toggleSwitch struct {
	*ctlBase
	on       bool
	onChange func()
}

func newToggle(parent ui.Parent, x, y int) *toggleSwitch {
	t := &toggleSwitch{}
	t.ctlBase = newCtlBase(parent, x-focusPad, y-focusPad, 40+2*focusPad, 20+2*focusPad, true,
		func() argb { return fp().cardBg })
	t.paint = t.draw
	t.click = func(win.POINT) { t.flip() }
	t.key = func(vk co.VK) bool {
		if vk == co.VK_SPACE || vk == co.VK_RETURN {
			t.flip()
			return true
		}
		return false
	}
	return t
}

func (t *toggleSwitch) flip() {
	t.on = !t.on
	t.redraw()
	if t.onChange != nil {
		t.onChange()
	}
}

func (t *toggleSwitch) IsOn() bool { return t.on }

// SetOn sets the state without calling OnChange.
func (t *toggleSwitch) SetOn(on bool) {
	if t.on != on {
		t.on = on
		t.redraw()
	}
}

func (t *toggleSwitch) Enable(on bool) { t.hwnd().EnableWindow(on) }

// OnChange runs when the user toggles the switch, not on SetOn.
func (t *toggleSwitch) OnChange(fun func()) { t.onChange = fun }

func (t *toggleSwitch) draw(c *canvas, rc win.RECT) {
	p := fp()
	pill := inset(rc, focusPad)
	h := pill.Bottom - pill.Top
	radius := int(h / 2)
	en := t.enabled()

	knob := ui.DpiY(12)
	if en && (t.hover || t.pressed) {
		knob = ui.DpiY(14)
	}
	var knobCol argb
	if t.on {
		track := p.accent
		switch {
		case !en:
			track = p.textDisabled
		case t.pressed:
			track = p.accentPressed
		case t.hover:
			track = p.accentHover
		}
		c.fillRoundRect(pill, radius, track)
		knobCol = p.toggleOnKnob
	} else {
		if en && t.hover {
			c.fillRoundRect(pill, radius, p.navHover)
		}
		stroke, kc := p.toggleOffStroke, p.toggleOffKnob
		if !en {
			stroke, kc = p.textDisabled, p.textDisabled
		}
		half := int32(ui.DpiX(1)) / 2
		c.strokeRoundRect(win.RECT{Left: pill.Left + half, Top: pill.Top + half,
			Right: pill.Right - half, Bottom: pill.Bottom - half}, radius, stroke, float32(ui.DpiX(1)))
		knobCol = kc
	}

	// Knob centre: one radius in from the edge it rests against.
	cy := (pill.Top + pill.Bottom) / 2
	cx := pill.Left + h/2
	if t.on {
		cx = pill.Right - h/2
	}
	k := int32(knob) / 2
	c.fillEllipse(win.RECT{Left: cx - k, Top: cy - k, Right: cx - k + int32(knob), Bottom: cy - k + int32(knob)}, knobCol)

	if t.showFocus() {
		paintFocus(c, pill, 10) // half the 20px height
	}
}

// navItem is one entry of the navigation pane.
type navItem struct {
	glyph rune
	text  string
}

// navList is the navigation pane's item list: 36px rows, glyph + text,
// rounded navSelected fill and a 3x16 accent pill on the selected row.
type navList struct {
	*ctlBase
	items    []navItem
	sel      int
	hot      int // row under the mouse, -1 for none
	onSelect func(int)
}

const (
	navRowHeight = 36
	navRowGap    = 4
	navMarginX   = 4
)

func newNavList(parent ui.Parent, x, y, width, height int, items []navItem) *navList {
	n := &navList{items: items, hot: -1}
	n.ctlBase = newCtlBase(parent, x, y, width, height, true, func() argb { return fp().navBg })
	n.paint = n.draw
	n.dlgCode = co.DLGC_WANTARROWS
	n.move = func(pt win.POINT) {
		if hot := n.rowAt(pt); hot != n.hot {
			n.hot = hot
			n.redraw()
		}
	}
	n.click = func(pt win.POINT) {
		if i := n.rowAt(pt); i >= 0 {
			n.pick(i)
		}
	}
	n.key = func(vk co.VK) bool {
		switch vk {
		case co.VK_UP:
			n.pick(max(n.sel-1, 0))
		case co.VK_DOWN:
			n.pick(min(n.sel+1, len(n.items)-1))
		case co.VK_HOME:
			n.pick(0)
		case co.VK_END:
			n.pick(len(n.items) - 1)
		default:
			return false
		}
		return true
	}
	return n
}

// rowRect is row i's rounded rectangle, physical pixels.
func (n *navList) rowRect(i int, rc win.RECT) win.RECT {
	top := rc.Top + int32(ui.DpiY(navRowGap+i*(navRowHeight+navRowGap)))
	return win.RECT{
		Left: rc.Left + int32(ui.DpiX(navMarginX)), Right: rc.Right - int32(ui.DpiX(navMarginX)),
		Top: top, Bottom: top + int32(ui.DpiY(navRowHeight)),
	}
}

func (n *navList) rowAt(pt win.POINT) int {
	if pt.X < 0 {
		return -1
	}
	rc, _ := n.hwnd().GetClientRect()
	for i := range n.items {
		r := n.rowRect(i, rc)
		if pt.X >= r.Left && pt.X < r.Right && pt.Y >= r.Top && pt.Y < r.Bottom {
			return i
		}
	}
	return -1
}

func (n *navList) pick(i int) {
	if i == n.sel {
		return
	}
	n.sel = i
	n.redraw()
	if n.onSelect != nil {
		n.onSelect(i)
	}
}

func (n *navList) Selected() int { return n.sel }

// Select moves the selection without calling OnSelect.
func (n *navList) Select(i int) {
	if i >= 0 && i < len(n.items) && i != n.sel {
		n.sel = i
		n.redraw()
	}
}

// OnSelect runs when the user picks a row, not on Select.
func (n *navList) OnSelect(fun func(i int)) { n.onSelect = fun }

func (n *navList) draw(c *canvas, rc win.RECT) {
	p := fp()
	en := n.enabled()
	radius := ui.DpiX(controlRadius)
	for i, it := range n.items {
		r := n.rowRect(i, rc)
		switch {
		case i == n.sel:
			c.fillRoundRect(r, radius, p.navSelected)
		case i == n.hot && en:
			c.fillRoundRect(r, radius, p.navHover)
		}
		if i == n.sel {
			ph, pw := int32(ui.DpiY(16)), int32(ui.DpiX(3))
			cy := (r.Top + r.Bottom) / 2
			pill := win.RECT{Left: r.Left, Top: cy - ph/2, Right: r.Left + pw, Bottom: cy - ph/2 + ph}
			col := p.navIndicator
			if !en {
				col = p.textDisabled
			}
			c.fillRoundRect(pill, int(pw/2), col)
		}
		col := p.text
		if !en {
			col = p.textDisabled
		}
		g := win.RECT{Left: r.Left + int32(ui.DpiX(12)), Top: r.Top, Right: r.Left + int32(ui.DpiX(12+16)), Bottom: r.Bottom}
		c.glyph(it.glyph, g, ui.DpiY(16), col)
		t := win.RECT{Left: g.Right + int32(ui.DpiX(14)), Top: r.Top, Right: r.Right - int32(ui.DpiX(8)), Bottom: r.Bottom}
		c.text(it.text, t, fontBody, col, co.DT_LEFT|co.DT_VCENTER|co.DT_SINGLELINE|co.DT_END_ELLIPSIS|co.DT_NOPREFIX)
		if i == n.sel && n.showFocus() {
			c.strokeRoundRect(inset(r, 1), radius, p.focusStroke, float32(ui.DpiX(2)))
		}
	}
}

// fluentButton is a WinUI Button (32px high); accent=true is the AccentButton.
// glyph 0 means text only.
type fluentButton struct {
	*ctlBase
	text    string
	glyph   rune
	accent  bool
	onClick func()
}

func newFluentButton(parent ui.Parent, text string, glyph rune, accent bool, x, y, width int) *fluentButton {
	b := &fluentButton{text: text, glyph: glyph, accent: accent}
	b.ctlBase = newCtlBase(parent, x-focusPad, y-focusPad, width+2*focusPad, 32+2*focusPad, true,
		func() argb { return fp().footerBg })
	b.paint = b.draw
	b.click = func(win.POINT) { b.fire() }
	b.key = func(vk co.VK) bool {
		if vk == co.VK_SPACE || vk == co.VK_RETURN {
			b.fire()
			return true
		}
		return false
	}
	return b
}

func (b *fluentButton) fire() {
	if b.onClick != nil {
		b.onClick()
	}
}

func (b *fluentButton) OnClick(fun func()) { b.onClick = fun }
func (b *fluentButton) Enable(on bool)     { b.hwnd().EnableWindow(on) }

// paintButtonFace draws a WinUI Button face in face: fluentButton's look,
// shared with productList's per-row "Verifica" buttons.
func paintButtonFace(c *canvas, face win.RECT, text string, glyph rune, accent, en, hover, pressed bool) {
	p := fp()
	radius := ui.DpiX(controlRadius)

	var fill, txt argb
	stroke := argb(0)
	if accent {
		fill, txt = p.accent, p.textOnAccent
		switch {
		case !en:
			fill, txt = p.buttonBg, p.textDisabled
			stroke = p.buttonStroke
		case pressed:
			fill = p.accentPressed
		case hover:
			fill = p.accentHover
		}
	} else {
		fill, txt, stroke = p.buttonBg, p.text, p.buttonStroke
		switch {
		case !en:
			txt = p.textDisabled
		case pressed:
			fill, txt = p.buttonPressed, p.textSecondary
		case hover:
			fill = p.buttonHover
		}
	}
	c.fillRoundRect(face, radius, fill)
	if stroke != 0 {
		half := int32(ui.DpiX(1)) / 2
		c.strokeRoundRect(win.RECT{Left: face.Left + half, Top: face.Top + half,
			Right: face.Right - half, Bottom: face.Bottom - half}, radius, stroke, float32(ui.DpiX(1)))
	}

	// Content centred: [glyph 16 + 8 gap] + text.
	tw, _ := c.measure(text, fontBody)
	gw, gap := 0, 0
	if glyph != 0 {
		gw, gap = ui.DpiX(16), ui.DpiX(8)
	}
	left := (face.Left+face.Right)/2 - int32(gw+gap+tw)/2
	if glyph != 0 {
		c.glyph(glyph, win.RECT{Left: left, Top: face.Top, Right: left + int32(gw), Bottom: face.Bottom}, ui.DpiY(14), txt)
	}
	t := win.RECT{Left: left + int32(gw+gap), Top: face.Top, Right: face.Right, Bottom: face.Bottom}
	if glyph == 0 && tw == 0 { // measure unavailable: centre in the face
		t = face
		c.text(text, t, fontBody, txt, co.DT_CENTER|co.DT_VCENTER|co.DT_SINGLELINE|co.DT_NOPREFIX)
	} else {
		c.text(text, t, fontBody, txt, co.DT_LEFT|co.DT_VCENTER|co.DT_SINGLELINE|co.DT_NOPREFIX)
	}
}

func (b *fluentButton) draw(c *canvas, rc win.RECT) {
	face := inset(rc, focusPad)
	paintButtonFace(c, face, b.text, b.glyph, b.accent, b.enabled(), b.hover, b.pressed)
	if b.showFocus() {
		paintFocus(c, face, controlRadius)
	}
}

// infoSeverity selects an InfoBar's colours and glyph.
type infoSeverity int

const (
	severityInfo infoSeverity = iota
	severityWarning
)

// infoBar is the WinUI InfoBar: rounded box, glyph, bold title and a
// wrapped body line.
type infoBar struct {
	*ctlBase
	sev         infoSeverity
	title, body string
}

func newInfoBar(parent ui.Parent, x, y, width, height int) *infoBar {
	b := &infoBar{}
	b.ctlBase = newCtlBase(parent, x, y, width, height, false, func() argb { return fp().windowBg })
	b.paint = b.draw
	return b
}

func (b *infoBar) Set(sev infoSeverity, title, body string) {
	b.sev, b.title, b.body = sev, title, body
	b.redraw()
}

func (b *infoBar) draw(c *canvas, rc win.RECT) {
	p := fp()
	bg, stroke, icon, g := p.infoBg, p.infoStroke, p.infoIcon, glyphInfo
	if b.sev == severityWarning {
		bg, stroke, icon, g = p.warnBg, p.warnStroke, p.warnIcon, glyphWarning
	}
	radius := ui.DpiX(cardRadius)
	c.fillRoundRect(rc, radius, bg)
	half := int32(ui.DpiX(1)) / 2
	c.strokeRoundRect(win.RECT{Left: rc.Left + half, Top: rc.Top + half, Right: rc.Right - half, Bottom: rc.Bottom - half},
		radius, stroke, float32(ui.DpiX(1)))

	// Glyph on a 16px line aligned with the title, like WinUI.
	top := rc.Top + int32(ui.DpiY(14))
	gx := rc.Left + int32(ui.DpiX(16))
	c.glyph(g, win.RECT{Left: gx, Top: top, Right: gx + int32(ui.DpiX(16)), Bottom: top + int32(ui.DpiY(18))}, ui.DpiY(16), icon)

	tx := gx + int32(ui.DpiX(16+14))
	right := rc.Right - int32(ui.DpiX(16))
	title := win.RECT{Left: tx, Top: top, Right: right, Bottom: top + int32(ui.DpiY(20))}
	c.text(b.title, title, fontBodyStrong, p.text, co.DT_LEFT|co.DT_SINGLELINE|co.DT_END_ELLIPSIS|co.DT_NOPREFIX)
	body := win.RECT{Left: tx, Top: title.Bottom + int32(ui.DpiY(2)), Right: right, Bottom: rc.Bottom - int32(ui.DpiY(8))}
	c.text(b.body, body, fontCaption, p.text, co.DT_LEFT|co.DT_WORDBREAK|co.DT_END_ELLIPSIS|co.DT_NOPREFIX)
}

// lockBadge is the square lock beside a governed setting, as tall as the
// inputs and combos next to it. Clicking it explains why the value cannot
// be edited (OnClick); it takes no focus, so it adds no tab stop.
type lockBadge struct {
	*ctlBase
	onClick func()
}

func newLockBadge(parent ui.Parent, x, y int) *lockBadge {
	b := &lockBadge{}
	b.ctlBase = newCtlBase(parent, x, y, lockBadgeWidth, lockBadgeWidth, false, func() argb { return fp().cardBg })
	b.paint = b.draw
	b.click = func(win.POINT) {
		if b.onClick != nil {
			b.onClick()
		}
	}
	return b
}

func (b *lockBadge) OnClick(fun func()) { b.onClick = fun }

func (b *lockBadge) Show(on bool) {
	cmd := co.SW_HIDE
	if on {
		cmd = co.SW_SHOWNA
	}
	b.hwnd().ShowWindow(cmd)
}

func (b *lockBadge) draw(c *canvas, rc win.RECT) {
	p := fp()
	fill, glyph := p.badgeBg, p.badgeText
	switch {
	case b.pressed:
		fill = p.buttonPressed
	case b.hover:
		fill, glyph = p.navHover, p.text
	}
	c.fillRoundRect(rc, ui.DpiX(controlRadius), fill)
	c.glyph(glyphLock, rc, ui.DpiY(14), glyph)
}

// lockBadgeWidth is the badge's side in 96-DPI units: square, as tall as a
// combo or an input (comboHeight, inputH).
const lockBadgeWidth = 32
