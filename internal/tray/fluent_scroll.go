//go:build windows

package tray

import (
	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
)

// fluentScrollBar is the settings viewport's scroll bar in the WinUI style:
// a thin line at rest that widens into a rounded thumb on a faint track
// while the mouse is over it, and is hidden when the page fits. A native
// WS_VSCROLL bar, even with the DarkMode_Explorer style, is a wide classic
// bar with arrows. It overlays the pages' right margin as a sibling kept on
// top of them (newScrollView), painted in windowBg like that margin.
//
// Dragging the thumb scrolls by the thumb's travel; a click on the track
// pages up or down. The wheel is not handled here: DefWindowProc passes it
// to the viewport, which scrolls.
type fluentScrollBar struct {
	*ctlBase
	v       *scrollView
	dragOff int // pointer offset inside the thumb while dragging, pixels; -1 when not dragging
}

// Scroll bar metrics, 96-DPI units.
const (
	scrollBarW     = 14 // the control: the hover hit area
	scrollThinW    = 2  // thumb at rest
	scrollWideW    = 6  // thumb under the mouse or while dragging
	scrollPad      = 4  // track inset at the top and bottom
	scrollMinThumb = 24
)

func newFluentScrollBar(v *scrollView, x, y, height int) *fluentScrollBar {
	s := &fluentScrollBar{v: v, dragOff: -1}
	s.ctlBase = newCtlBase(v.ctl, x, y, scrollBarW, height, false, func() argb { return fp().windowBg })
	s.paint = s.paintBar
	s.press = s.pressAt
	s.move = s.moveTo
	return s
}

// sync shows the bar only when the page is taller than the viewport.
func (s *fluentScrollBar) sync() {
	h := s.hwnd()
	if h == 0 {
		return
	}
	if s.v.contentHeight() > s.v.viewHeight() {
		h.ShowWindow(co.SW_SHOWNA)
	} else {
		h.ShowWindow(co.SW_HIDE)
	}
	s.redraw()
}

// thumb returns the thumb's top and height on the track, pixels, plus the
// track's top and height.
func (s *fluentScrollBar) thumb() (top, height, trackTop, trackH int) {
	rc, _ := s.hwnd().GetClientRect()
	trackTop = ui.DpiY(scrollPad)
	trackH = int(rc.Bottom) - 2*trackTop
	content, view := s.v.contentHeight(), s.v.viewHeight()
	if content <= view || trackH <= 0 {
		return trackTop, trackH, trackTop, trackH
	}
	height = max(trackH*view/content, ui.DpiY(scrollMinThumb))
	top = trackTop + (trackH-height)*s.v.pos/(content-view)
	return top, height, trackTop, trackH
}

func (s *fluentScrollBar) paintBar(c *canvas, rc win.RECT) {
	p := fp()
	wide := s.hover || s.pressed
	top, height, trackTop, trackH := s.thumb()
	w := ui.DpiX(scrollThinW)
	if wide {
		w = ui.DpiX(scrollWideW)
		track := win.RECT{Left: rc.Right - int32(ui.DpiX(scrollWideW+4)), Top: int32(trackTop),
			Right: rc.Right - int32(ui.DpiX(2)), Bottom: int32(trackTop + trackH)}
		c.fillRoundRect(track, ui.DpiX(5), p.navHover)
	}
	// Centred on the same line as the track, so it widens in place.
	cx := int(rc.Right) - ui.DpiX(2) - ui.DpiX(scrollWideW+2)/2
	th := win.RECT{Left: int32(cx - w/2), Top: int32(top), Right: int32(cx - w/2 + w), Bottom: int32(top + height)}
	col := p.textSecondary
	if !wide {
		col = col&0x00FFFFFF | 0x99000000 // the resting line is fainter
	}
	c.fillRoundRect(th, w/2, col)
}

func (s *fluentScrollBar) pressAt(pt win.POINT) {
	top, height, _, _ := s.thumb()
	y := int(pt.Y)
	switch {
	case y < top:
		s.dragOff = -1
		s.v.scrollTo(s.v.pos - s.v.viewHeight())
	case y >= top+height:
		s.dragOff = -1
		s.v.scrollTo(s.v.pos + s.v.viewHeight())
	default:
		s.dragOff = y - top
	}
}

// moveTo drags the thumb; ctlBase calls it for every mouse move, and with
// (-1, -1) when the mouse leaves.
func (s *fluentScrollBar) moveTo(pt win.POINT) {
	if !s.pressed || s.dragOff < 0 || pt.Y == -1 && pt.X == -1 {
		return
	}
	_, height, trackTop, trackH := s.thumb()
	travel := trackH - height
	if travel <= 0 {
		return
	}
	content, view := s.v.contentHeight(), s.v.viewHeight()
	s.v.scrollTo((int(pt.Y) - s.dragOff - trackTop) * (content - view) / travel)
}
