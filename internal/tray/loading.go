//go:build windows

package tray

import (
	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
)

// barParent is what ui.Main and ui.Modal offer a loadingBar.
type barParent interface {
	ui.Parent
	On() *ui.WindowEvents
}

// loadingBar is an indeterminate progress bar in the style of the Windows 11
// Explorer copy dialog - flat, 1px border, accent-coloured segment sliding
// across the track - shown while the window waits on the backend (loading
// the configuration, the products, a manifest check, a save). It is an
// owner-drawn Static rather than a msctls_progress32 with PBS_MARQUEE, which
// stays light-themed in the dark palette.
//
// Start/Stop nest: the bar stays up until every Start has had its Stop, so
// several manifest checks in flight keep a single animation running. UI
// thread only. One per window: windigo keeps a single WM_DRAWITEM handler.
type loadingBar struct {
	st      *ui.Static
	timerID int
	phase   int // 0..barPhases-1, the segment's position
	busy    int
}

const (
	barPhases   = 120 // one sweep, at barTickMs per phase ≈ 1.8 s
	barTickMs   = 15
	barSegShare = 30 // segment width, % of the track
)

// newLoadingBar creates the bar hidden. Call before the window is created.
func newLoadingBar(parent barParent, timerID, x, y, width, height int) *loadingBar {
	b := &loadingBar{timerID: timerID}
	b.st = ui.NewStatic(parent, ui.OptsStatic().
		Position(ui.Dpi(x, y)).Size(ui.Dpi(width, height)).
		CtrlStyle(co.SS_OWNERDRAW).
		WndStyle(co.WS_CHILD)) // no WS_VISIBLE: hidden until Start

	parent.On().WmDrawItem(func(p ui.WmDrawItem) {
		dis := p.DrawItemStruct()
		if dis.HwndItem == b.st.Hwnd() {
			b.paint(dis.Hdc, dis.RcItem)
		}
	})
	parent.On().WmTimer(timerID, func() {
		b.phase = (b.phase + 1) % barPhases
		b.st.Hwnd().InvalidateRect(nil, false)
	})
	return b
}

// Start shows the bar (or keeps it up) for one more pending operation.
func (b *loadingBar) Start() {
	b.busy++
	if b.busy > 1 {
		return
	}
	b.phase = 0
	b.st.Hwnd().ShowWindow(co.SW_SHOWNA)
	_ = b.parentHwnd().SetTimer(b.timerID, barTickMs)
}

// Stop ends one pending operation; the bar hides when none is left.
func (b *loadingBar) Stop() {
	if b.busy == 0 {
		return
	}
	b.busy--
	if b.busy > 0 {
		return
	}
	_ = b.parentHwnd().KillTimer(b.timerID)
	b.st.Hwnd().ShowWindow(co.SW_HIDE)
}

func (b *loadingBar) parentHwnd() win.HWND {
	h, _ := b.st.Hwnd().GetParent()
	return h
}

func (b *loadingBar) paint(hdc win.HDC, rc win.RECT) {
	border, track, fill := win.RGB(173, 173, 173), win.RGB(230, 230, 230), win.RGB(0, 120, 215)
	if curPal == &darkPal {
		border, track, fill = win.RGB(110, 110, 110), win.RGB(43, 43, 43), win.RGB(76, 194, 255)
	}
	fillColor(hdc, rc, border) // 1px border: the outer rect, then the track inset
	in := win.RECT{Left: rc.Left + 1, Top: rc.Top + 1, Right: rc.Right - 1, Bottom: rc.Bottom - 1}
	fillColor(hdc, in, track)

	// The segment enters from the left edge and leaves past the right one.
	width := in.Right - in.Left
	seg := width * barSegShare / 100
	left := in.Left - seg + (width+seg)*int32(b.phase)/barPhases
	s := win.RECT{Left: max(left, in.Left), Top: in.Top, Right: min(left+seg, in.Right), Bottom: in.Bottom}
	fillColor(hdc, s, fill)
}

// fillColor fills rc with c; an empty rect is a no-op.
func fillColor(hdc win.HDC, rc win.RECT, c win.COLORREF) {
	if rc.Right <= rc.Left || rc.Bottom <= rc.Top {
		return
	}
	h, _, _ := procCreateSolidBrush.Call(uintptr(c))
	br := win.HBRUSH(h)
	defer br.DeleteObject()
	hdc.FillRect(&rc, br)
}
