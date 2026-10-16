//go:build windows

package tray

import (
	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
)

// The settings window's Fluent layout (see fluent.go): the navigation pane
// on the left, the InfoBar and a scrolling viewport of pages on the right,
// the footer band with the note and the buttons across the bottom. The
// pages are built by settingsPage; settings.go only says which rows go
// where and keeps the form's logic.
//
// All constants are 96-DPI units, scaled with ui.DpiX/DpiY where used.
const (
	settingsClientW = 800
	settingsClientH = 620
	navPaneW        = 200
	footerH         = 60
	contentX        = navPaneW + 28 // InfoBar and cards share this left edge
	contentW        = settingsClientW - contentX - 24
	infoBarY        = 12
	infoBarH        = 64
	barY            = infoBarY + infoBarH + 4 // loadingBar, under the InfoBar
	barH            = 4
	viewportY       = barY + barH + 4
	viewportH       = settingsClientH - footerH - viewportY
	pageCardX       = contentX - navPaneW // card left edge, page coordinates
	pageTitleH      = 44
	inputH          = 32
	toggleW         = 40
	toggleH         = 20
	badgeH          = lockBadgeWidth
	ctlGap          = 8
)

// paintSettingsChrome paints the main window behind its children: the
// navigation pane, the content area with WinUI's rounded top-left corner,
// and the footer band with its shield glyph and note.
func paintSettingsChrome(c *canvas, rc win.RECT, note string) {
	p := fp()
	c.fillRect(rc, p.navBg)
	footerTop := int32(ui.DpiY(settingsClientH - footerH))
	content := win.RECT{Left: int32(ui.DpiX(navPaneW)), Top: 0,
		Right: rc.Right + int32(ui.DpiX(16)), Bottom: footerTop + int32(ui.DpiY(16))}
	c.fillRoundRect(content, ui.DpiX(8), p.windowBg)

	footer := win.RECT{Left: 0, Top: footerTop, Right: rc.Right, Bottom: rc.Bottom}
	c.fillRect(footer, p.footerBg)
	c.line(0, int(footerTop), int(rc.Right), int(footerTop), p.footerStroke, 1)

	// One line, glyph and note both centred on the band, like the buttons.
	shield := win.RECT{Left: int32(ui.DpiX(20)), Top: footerTop,
		Right: int32(ui.DpiX(36)), Bottom: rc.Bottom}
	c.glyph(glyphShield, shield, ui.DpiY(14), p.textSecondary)
	text := win.RECT{Left: int32(ui.DpiX(44)), Top: footerTop,
		Right: int32(ui.DpiX(footerNoteRight)), Bottom: rc.Bottom}
	c.text(note, text, fontCaption, p.textSecondary, co.DT_LEFT|co.DT_VCENTER|co.DT_SINGLELINE|co.DT_END_ELLIPSIS|co.DT_NOPREFIX)
}

// footerNoteRight is where the footer note ends, left of the buttons.
const footerNoteRight = 560

// scrollView is the content area's viewport: one page visible at a time,
// moved up and down inside it when taller (the scroll bar, the mouse wheel).
// The pages are its children; a page is moved, never redrawn by scrolling.
type scrollView struct {
	ctl   *ui.Control
	bar   *fluentScrollBar
	pages []*settingsPage
	cur   int
	pos   int // scroll position, pixels
}

func newScrollView(parent ui.Parent) *scrollView {
	v := &scrollView{}
	v.ctl = ui.NewControl(parent, ui.OptsControl().
		Position(ui.Dpi(navPaneW, viewportY)).
		Size(ui.Dpi(settingsClientW-navPaneW, viewportH)).
		Style(co.WS_CHILD|co.WS_VISIBLE|co.WS_CLIPCHILDREN|co.WS_CLIPSIBLINGS).
		ExStyle(co.WS_EX_CONTROLPARENT))
	v.bar = newFluentScrollBar(v, settingsClientW-navPaneW-scrollBarW, 0, viewportH)

	v.ctl.On().WmCreate(func(_ ui.WmCreate) int {
		// The pages are created after the bar; keep it above them, it
		// overlays their right margin.
		_ = v.bar.hwnd().SetWindowPos(win.HWND(0), win.POINT{}, win.SIZE{},
			co.SWP_NOMOVE|co.SWP_NOSIZE|co.SWP_NOACTIVATE)
		v.update()
		return 0
	})
	v.ctl.On().WmEraseBkgnd(func(_ ui.WmEraseBkgnd) int { return 1 })
	v.ctl.On().WmPaint(func() {
		paintBuffered(v.ctl.Hwnd(), func(c *canvas, rc win.RECT) { c.fillRect(rc, fp().windowBg) })
	})
	// The wheel reaches here from every child: DefWindowProc hands an
	// unhandled WM_MOUSEWHEEL to the parent (the page, then this).
	v.ctl.On().Wm(wmMouseWheel, func(p ui.Wm) uintptr {
		v.wheel(p.WParam)
		return 0
	})
	return v
}

func (v *scrollView) wheel(wp win.WPARAM) {
	delta := int(int16(wp.HiWord()))
	v.scrollTo(v.pos - delta*ui.DpiY(48)/120)
}

func (v *scrollView) viewHeight() int {
	rc, _ := v.ctl.Hwnd().GetClientRect()
	return int(rc.Bottom)
}

// show makes page i the visible one, scrolled to the top.
func (v *scrollView) show(i int) {
	if i == v.cur {
		return
	}
	v.pages[v.cur].ctl.Hwnd().ShowWindow(co.SW_HIDE)
	v.cur = i
	v.pos = 0
	v.update()
	v.pages[i].ctl.Hwnd().ShowWindow(co.SW_SHOW)
}

// update fits the scroll bar to the current page and moves the page to pos.
func (v *scrollView) update() {
	v.bar.sync()
	v.place()
}

// contentHeight is the current page's height, pixels.
func (v *scrollView) contentHeight() int { return v.pages[v.cur].height }

func (v *scrollView) scrollTo(pos int) {
	pos = min(pos, v.pages[v.cur].height-v.viewHeight())
	pos = max(pos, 0)
	if pos == v.pos {
		return
	}
	v.pos = pos
	v.bar.redraw()
	v.place()
}

func (v *scrollView) place() {
	_ = v.pages[v.cur].ctl.Hwnd().SetWindowPos(0, win.POINT{X: 0, Y: int32(-v.pos)}, win.SIZE{},
		co.SWP_NOSIZE|co.SWP_NOZORDER|co.SWP_NOACTIVATE)
}

// ensureVisible scrolls the current page so that the child h shows whole:
// for keyboard focus moving onto a control below the fold.
func (v *scrollView) ensureVisible(h win.HWND) {
	page := v.pages[v.cur].ctl.Hwnd()
	rc, err := h.GetWindowRect()
	if err != nil {
		return
	}
	pt := win.POINT{X: rc.Left, Y: rc.Top}
	page.ScreenToClientPt(&pt)
	v.ensureSpan(int(pt.Y), int(pt.Y+rc.Bottom-rc.Top))
}

// ensureSpan scrolls the current page so that its pixels top..bottom (page
// coordinates) show whole.
func (v *scrollView) ensureSpan(top, bottom int) {
	margin := ui.DpiY(12)
	switch view := v.viewHeight(); {
	case top-margin < v.pos:
		v.scrollTo(top - margin)
	case bottom+margin > v.pos+view:
		v.scrollTo(bottom + margin - view)
	}
}

// invalidate repaints the visible page: input frames follow their edit's
// enabled and focused state.
func (v *scrollView) invalidate() {
	v.pages[v.cur].ctl.Hwnd().InvalidateRect(nil, false)
}

// settingsPage is one navigation entry's content: a title and one card of
// rows, each row a label painted by the page plus native or Fluent controls
// on its right. Built row by row before the window exists; the geometry is
// kept in pixels for WM_PAINT.
type settingsPage struct {
	v      *scrollView
	ctl    *ui.Control
	title  string
	y      int // build cursor, 96-DPI units
	height int // pixels, set by finish

	card     win.RECT
	dividers []int
	rows     []pageRow
	frames   []inputFrame
	edits    []*ui.Edit      // for WM_SETFONT
	extra    func(c *canvas) // optional: what a page without rows paints itself
}

type pageRow struct {
	rc         win.RECT
	title, sub string
}

// inputFrame is a borderless Edit and the Fluent text-box chrome around it.
type inputFrame struct {
	rc   win.RECT
	edit *ui.Edit
}

func newSettingsPage(v *scrollView, title string, visible bool) *settingsPage {
	p := &settingsPage{v: v, title: title, y: pageTitleH}
	style := co.WS_CHILD | co.WS_CLIPCHILDREN | co.WS_CLIPSIBLINGS
	if visible {
		style |= co.WS_VISIBLE
	}
	p.ctl = ui.NewControl(v.ctl, ui.OptsControl().
		Position(0, 0).
		Size(ui.Dpi(settingsClientW-navPaneW, viewportH)).
		Style(style).
		ExStyle(co.WS_EX_CONTROLPARENT))
	p.card = win.RECT{Left: int32(ui.DpiX(pageCardX)), Top: int32(ui.DpiY(p.y)),
		Right: int32(ui.DpiX(pageCardX + contentW))}
	v.pages = append(v.pages, p)

	p.ctl.On().WmCreate(func(_ ui.WmCreate) int {
		font := fluentFont(fontBody)
		for _, e := range p.edits {
			e.Hwnd().SendMessage(co.WM_SETFONT, win.WPARAM(font), 1)
		}
		_ = p.ctl.Hwnd().SetWindowPos(0, win.POINT{}, win.SIZE{
			Cx: int32(ui.DpiX(settingsClientW - navPaneW)), Cy: int32(max(p.height, ui.DpiY(viewportH)))},
			co.SWP_NOMOVE|co.SWP_NOZORDER|co.SWP_NOACTIVATE)
		return 0
	})
	p.ctl.On().WmEraseBkgnd(func(_ ui.WmEraseBkgnd) int { return 1 })
	p.ctl.On().WmPaint(func() { paintBuffered(p.ctl.Hwnd(), p.paint) })

	// Edits, read-only and disabled ones (WM_CTLCOLORSTATIC) included, sit
	// in input frames.
	inputColors := func(cp ui.WmCtlColor) win.HBRUSH {
		pal := fp()
		cp.Hdc().SetTextColor(pal.text.colorref())
		cp.Hdc().SetBkColor(pal.inputBg.colorref())
		return solidBrush(pal.inputBg)
	}
	p.ctl.On().WmCtlColorEdit(inputColors)
	p.ctl.On().WmCtlColorStatic(inputColors)
	return p
}

func (p *settingsPage) paint(c *canvas, rc win.RECT) {
	pal := fp()
	c.fillRect(rc, pal.windowBg)
	title := win.RECT{Left: p.card.Left, Top: int32(ui.DpiY(4)), Right: p.card.Right, Bottom: int32(ui.DpiY(pageTitleH - 4))}
	c.text(p.title, title, fontSubtitle, pal.text, co.DT_LEFT|co.DT_VCENTER|co.DT_SINGLELINE|co.DT_NOPREFIX)
	if len(p.rows) > 0 {
		paintCard(c, p.card, p.dividers)
	}
	if p.extra != nil {
		p.extra(c)
	}
	for _, r := range p.rows {
		paintRowLabel(c, r.rc, r.title, r.sub, true)
	}
	focus := win.GetFocus()
	for _, f := range p.frames {
		h := f.edit.Hwnd()
		paintInputFrame(c, f.rc, focus == h, isEnabled(h))
	}
}

// rowSlot is one row being built: controls are placed right-aligned on its
// centre line, a lock badge (governed rows) on their left.
type rowSlot struct {
	p      *settingsPage
	top    int // 96-DPI units, page coordinates
	center int
}

// row adds a row of the given height (rowMinHeight for the usual one).
func (p *settingsPage) row(title, sub string, height int) *rowSlot {
	if len(p.rows) > 0 {
		p.dividers = append(p.dividers, ui.DpiY(p.y))
	}
	labelW := contentW - 2*rowPadX
	if height == rowMinHeight {
		labelW = 260 // controls take the rest
	}
	p.rows = append(p.rows, pageRow{
		rc:    dpiRect(pageCardX+rowPadX, p.y, labelW, min(height, rowMinHeight)),
		title: title, sub: sub,
	})
	s := &rowSlot{p: p, top: p.y, center: p.y + min(height, rowMinHeight)/2}
	p.y += height
	return s
}

// finish closes the card; call after the last row.
func (p *settingsPage) finish() {
	p.card.Bottom = int32(ui.DpiY(p.y))
	p.height = ui.DpiY(p.y + 24)
}

// grow sets the height of a page whose content changes after creation (the
// Prodotti page, as its rows arrive): bottom is the content's bottom edge,
// 96-DPI units. The page window and, when it is the visible page, the
// scroll bar follow.
func (p *settingsPage) grow(bottom int) {
	p.y = bottom
	p.height = ui.DpiY(bottom + 24)
	if h := p.ctl.Hwnd(); h != 0 {
		_ = h.SetWindowPos(0, win.POINT{}, win.SIZE{
			Cx: int32(ui.DpiX(settingsClientW - navPaneW)), Cy: int32(max(p.height, ui.DpiY(viewportH)))},
			co.SWP_NOMOVE|co.SWP_NOZORDER|co.SWP_NOACTIVATE)
	}
	if v := p.v; v.pages[v.cur] == p && v.ctl.Hwnd() != 0 {
		v.pos = min(v.pos, max(p.height-v.viewHeight(), 0))
		v.update()
	}
}

// place lays out controls of the given widths right-aligned on the row, left
// to right, and returns their x and the lock badge's x on their left.
func (s *rowSlot) place(widths ...int) (xs []int, badgeX int) {
	right := pageCardX + contentW - rowPadX
	total := 0
	for _, w := range widths {
		total += w
	}
	total += ctlGap * (len(widths) - 1)
	x := right - total
	for _, w := range widths {
		xs = append(xs, x)
		x += w + ctlGap
	}
	return xs, right - total - ctlGap - lockBadgeWidth
}

// input adds a borderless Edit inside an input frame at x on the row.
func (s *rowSlot) input(x, width int, style co.ES) *ui.Edit {
	return s.p.input(x, s.center-inputH/2, width, inputH, style, 0)
}

func (p *settingsPage) input(x, y, width, height int, style co.ES, ws co.WS) *ui.Edit {
	in := inputInset
	e := ui.NewEdit(p.ctl, ui.OptsEdit().
		Position(ui.Dpi(x+in+2, y+in)).
		Width(ui.DpiX(width-2*in-4)).Height(ui.DpiY(height-2*in)).
		CtrlStyle(style).
		WndStyle(co.WS_CHILD|co.WS_VISIBLE|co.WS_TABSTOP|co.WS_GROUP|ws).
		WndExStyle(co.WS_EX_LEFT))
	p.frames = append(p.frames, inputFrame{rc: dpiRect(x, y, width, height), edit: e})
	p.edits = append(p.edits, e)
	// The frame's bottom edge shows the focus.
	e.On().EnSetFocus(func() {
		p.v.invalidate()
		p.v.ensureVisible(e.Hwnd())
	})
	e.On().EnKillFocus(func() { p.v.invalidate() })
	return e
}

// combo adds a fluentCombo at x on the row. Its backdrop is the card's.
func (s *rowSlot) combo(x, width int, texts ...string) *fluentCombo {
	p := s.p
	cb := newFluentCombo(p.ctl, x, s.center-comboHeight/2, width, texts)
	cb.focus = func() { p.v.ensureVisible(cb.Hwnd()) }
	return cb
}

// toggle adds a toggle switch at x on the row.
func (s *rowSlot) toggle(x int) *toggleSwitch {
	return newToggle(s.p.ctl, x, s.center-toggleH/2)
}

// badge adds the row's lock badge at x, hidden until a document governs it.
func (s *rowSlot) badge(x int) *lockBadge {
	return newLockBadge(s.p.ctl, x, s.center-badgeH/2)
}

// wmMouseWheel is WM_MOUSEWHEEL, which windigo's co does not name.
const wmMouseWheel = co.WM(0x020A)

var procIsWindowEnabled = user32.NewProc("IsWindowEnabled")

func isEnabled(h win.HWND) bool {
	r, _, _ := procIsWindowEnabled.Call(uintptr(h))
	return r != 0
}

func dpiRect(x, y, w, h int) win.RECT {
	return win.RECT{Left: int32(ui.DpiX(x)), Top: int32(ui.DpiY(y)),
		Right: int32(ui.DpiX(x + w)), Bottom: int32(ui.DpiY(y + h))}
}

// brushes are the solid brushes of the palettes' colours, created on first
// use and kept for the process lifetime. UI thread only.
var brushes = map[argb]win.HBRUSH{}

func solidBrush(c argb) win.HBRUSH {
	if b, ok := brushes[c]; ok {
		return b
	}
	h, _, _ := procCreateSolidBrush.Call(uintptr(c.colorref()))
	brushes[c] = win.HBRUSH(h)
	return win.HBRUSH(h)
}
