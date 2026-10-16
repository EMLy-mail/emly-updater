//go:build windows

package tray

import (
	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
)

// productList is the Prodotti page's card: one row per product - name,
// what is installed or pending, what the server offers - each with its own
// "Verifica" button.
//
// It is one painted control, not a row of child controls per product:
// windigo creates child controls only at their parent's WM_CREATE, and the
// rows arrive later, from backend.products off the UI thread. So the card,
// the row labels and the buttons are all drawn here and hit-tested by hand,
// and the control resizes itself to its rows (onResize lets the page grow).
//
// Keyboard: it is one tab stop; Up/Down/Home/End move the highlighted row,
// whose button shows the focus ring, and Space/Enter check it.
type productList struct {
	*ctlBase
	width    int // 96-DPI units
	rows     []productEntry
	hi       int // highlighted (keyboard) row, -1 none
	hot      int // button under the mouse, -1 none
	onCheck  func(i int)
	onResize func(height int) // new height, 96-DPI units
	onHi     func(top, bottom int)
}

// productEntry is one row. offered is the last check's result line; busy
// while a check is in flight.
type productEntry struct {
	slug, name, sub string
	offered         string
	busy            bool
}

// Row geometry, 96-DPI units.
const (
	productBtnW     = 96
	productOfferedW = 190
)

func newProductList(parent ui.Parent, x, y, width int) *productList {
	l := &productList{width: width, hi: -1, hot: -1}
	// Zero height until rows arrive: an empty card would read as "no
	// products" while they are still loading.
	l.ctlBase = newCtlBase(parent, x, y, width, 0, true, func() argb { return fp().windowBg })
	l.paint = l.draw
	l.dlgCode = co.DLGC_WANTARROWS
	l.move = func(pt win.POINT) {
		if hot := l.buttonAt(pt); hot != l.hot {
			l.hot = hot
			l.redraw()
		}
	}
	l.click = func(pt win.POINT) {
		if i := l.buttonAt(pt); i >= 0 {
			l.highlight(i)
			l.check(i)
		}
	}
	l.key = func(vk co.VK) bool {
		if len(l.rows) == 0 {
			return false
		}
		switch vk {
		case co.VK_UP:
			l.highlight(max(l.hi-1, 0))
		case co.VK_DOWN:
			l.highlight(min(l.hi+1, len(l.rows)-1))
		case co.VK_HOME:
			l.highlight(0)
		case co.VK_END:
			l.highlight(len(l.rows) - 1)
		case co.VK_SPACE, co.VK_RETURN:
			l.check(max(l.hi, 0))
		default:
			return false
		}
		return true
	}
	// Tabbing in starts on the first row, so the focus ring has a button.
	l.focus = func() {
		if l.hi < 0 && len(l.rows) > 0 {
			l.highlight(0)
		}
	}
	return l
}

// setRows replaces every row and resizes the control to fit them.
func (l *productList) setRows(rows []productEntry) {
	l.rows = rows
	l.hi, l.hot = -1, -1
	h := 0
	if len(rows) > 0 {
		h = len(rows) * rowMinHeight
	}
	_ = l.hwnd().SetWindowPos(0, win.POINT{}, win.SIZE{Cx: int32(ui.DpiX(l.width)), Cy: int32(ui.DpiY(h))},
		co.SWP_NOMOVE|co.SWP_NOZORDER|co.SWP_NOACTIVATE)
	l.redraw()
	if l.onResize != nil {
		l.onResize(h)
	}
}

// setOffered shows a check's state on row i: busy, or its result line.
func (l *productList) setOffered(i int, offered string, busy bool) {
	if i < 0 || i >= len(l.rows) {
		return
	}
	l.rows[i].offered, l.rows[i].busy = offered, busy
	l.redraw()
}

func (l *productList) highlight(i int) {
	if i == l.hi {
		return
	}
	l.hi = i
	l.redraw()
	if l.onHi != nil {
		top := i * rowMinHeight
		l.onHi(top, top+rowMinHeight)
	}
}

func (l *productList) check(i int) {
	if i < 0 || i >= len(l.rows) || l.rows[i].busy || l.onCheck == nil {
		return
	}
	l.onCheck(i)
}

// rowRect is row i's band across the card, physical pixels.
func (l *productList) rowRect(i int) win.RECT {
	return dpiRect(0, i*rowMinHeight, l.width, rowMinHeight)
}

// buttonRect is row i's "Verifica" face, physical pixels.
func (l *productList) buttonRect(i int) win.RECT {
	return dpiRect(l.width-rowPadX-productBtnW, i*rowMinHeight+(rowMinHeight-32)/2, productBtnW, 32)
}

func (l *productList) buttonAt(pt win.POINT) int {
	if pt.X < 0 {
		return -1
	}
	for i := range l.rows {
		r := l.buttonRect(i)
		if pt.X >= r.Left && pt.X < r.Right && pt.Y >= r.Top && pt.Y < r.Bottom {
			return i
		}
	}
	return -1
}

func (l *productList) draw(c *canvas, rc win.RECT) {
	if len(l.rows) == 0 {
		return
	}
	p := fp()
	var dividers []int
	for i := 1; i < len(l.rows); i++ {
		dividers = append(dividers, int(l.rowRect(i).Top))
	}
	paintCard(c, rc, dividers)

	en := l.enabled()
	for i, r := range l.rows {
		top := i * rowMinHeight
		offered := r.offered
		switch {
		case r.busy:
			offered = "verifica in corso…"
		case offered == "":
			offered = "—"
		}
		// The result takes what it needs, up to productOfferedW; the label
		// gets the rest, so a short result does not cut the subtitle.
		ow, _ := c.measure(offered, fontCaption)
		ow = min(ow*96/max(ui.DpiX(96), 1)+4, productOfferedW)
		labelW := l.width - 2*rowPadX - productBtnW - 2*ctlGap - ow
		paintRowLabel(c, dpiRect(rowPadX, top, labelW, rowMinHeight), r.name, r.sub, en)
		ox := l.width - rowPadX - productBtnW - ctlGap - ow
		c.text(offered, dpiRect(ox, top, ow, rowMinHeight), fontCaption, p.textSecondary,
			co.DT_RIGHT|co.DT_VCENTER|co.DT_SINGLELINE|co.DT_END_ELLIPSIS|co.DT_NOPREFIX)

		face := l.buttonRect(i)
		btnEn := en && !r.busy
		paintButtonFace(c, face, "Verifica", 0, false, btnEn, btnEn && i == l.hot,
			btnEn && i == l.hot && l.pressed)
		if i == l.hi && l.showFocus() {
			paintFocus(c, face, controlRadius)
		}
	}
}
