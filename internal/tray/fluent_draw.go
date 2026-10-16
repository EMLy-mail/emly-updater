//go:build windows

package tray

import (
	"math"
	"sync"
	"unsafe"

	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
	"golang.org/x/sys/windows"
)

// Antialiased drawing for the Fluent controls. GDI alone cannot draw a
// smooth rounded corner, so shapes go through GDI+'s flat API (gdiplus.dll,
// on every Windows since XP: no new dependency). Text stays on GDI: GDI+'s
// own text rendering is not ClearType and looks blurry beside the native
// Edit and ComboBox controls.
//
// Float arguments are passed to the flat API as math.Float32bits in a
// uintptr: on windows/amd64 Go's syscall path copies the first four
// arguments into XMM0-3 as well as the integer registers, and later ones go
// on the stack, so the callee finds each float where it expects it.

var (
	gdiplus                = windows.NewLazySystemDLL("gdiplus.dll")
	procGdiplusStartup     = gdiplus.NewProc("GdiplusStartup")
	procGdipCreateFromHDC  = gdiplus.NewProc("GdipCreateFromHDC")
	procGdipDeleteGraphics = gdiplus.NewProc("GdipDeleteGraphics")
	procGdipSetSmoothing   = gdiplus.NewProc("GdipSetSmoothingMode")
	procGdipSetPixelOffset = gdiplus.NewProc("GdipSetPixelOffsetMode")
	procGdipGetDC          = gdiplus.NewProc("GdipGetDC")
	procGdipReleaseDC      = gdiplus.NewProc("GdipReleaseDC")
	procGdipCreatePath     = gdiplus.NewProc("GdipCreatePath")
	procGdipDeletePath     = gdiplus.NewProc("GdipDeletePath")
	procGdipAddPathArc     = gdiplus.NewProc("GdipAddPathArc")
	procGdipAddPathRect    = gdiplus.NewProc("GdipAddPathRectangle")
	procGdipClosePathFig   = gdiplus.NewProc("GdipClosePathFigure")
	procGdipFillPath       = gdiplus.NewProc("GdipFillPath")
	procGdipDrawPath       = gdiplus.NewProc("GdipDrawPath")
	procGdipFillRectangle  = gdiplus.NewProc("GdipFillRectangle")
	procGdipFillEllipse    = gdiplus.NewProc("GdipFillEllipse")
	procGdipDrawEllipse    = gdiplus.NewProc("GdipDrawEllipse")
	procGdipDrawLine       = gdiplus.NewProc("GdipDrawLine")
	procGdipCreateSolid    = gdiplus.NewProc("GdipCreateSolidFill")
	procGdipDeleteBrush    = gdiplus.NewProc("GdipDeleteBrush")
	procGdipCreatePen1     = gdiplus.NewProc("GdipCreatePen1")
	procGdipDeletePen      = gdiplus.NewProc("GdipDeletePen")
	procGdipSetClipRectI   = gdiplus.NewProc("GdipSetClipRectI")
	procGdipResetClip      = gdiplus.NewProc("GdipResetClip")

	procFdGetDC       = user32.NewProc("GetDC")
	procFdReleaseDC   = user32.NewProc("ReleaseDC")
	procFdGetTextFace = gdi32.NewProc("GetTextFaceW")
)

const (
	gpSmoothingAntiAlias = 4
	// Half: pixel (x, y) covers [x, x+1), so an integer rectangle fills
	// whole pixels and a stroke centred on x+0.5 lands on one column.
	gpPixelOffsetHalf  = 4
	gpUnitPixel        = 2
	gpCombineReplace   = 0
	gpFillAlternate    = 0
	dtCalcRect         = 0x0400
	fluentDefaultFlags = dtSingleLine | dtNoPrefix | dtEndEllipsis | dtVCenter
)

var gdiplusOnce sync.Once

// startGdiplus starts GDI+ once for the process; it is never shut down; the
// tray lives until logoff and GdiplusShutdown at exit buys nothing.
func startGdiplus() {
	gdiplusOnce.Do(func() {
		input := struct {
			version                       uint32
			debugCallback                 uintptr
			suppressBgThread, suppressExt int32
		}{version: 1}
		var token uintptr
		procGdiplusStartup.Call(uintptr(unsafe.Pointer(&token)), uintptr(unsafe.Pointer(&input)), 0)
	})
}

func f32(v float32) uintptr { return uintptr(math.Float32bits(v)) }

// canvas is one WM_PAINT's double-buffered surface: a memory DC (32-bit DIB)
// with a GDI+ Graphics on it (antialiased shapes) and the same DC for GDI
// text (ClearType works because the buffer is opaque). All coordinates are
// physical pixels in the painted window's client area.
type canvas struct {
	hdc win.HDC
	g   uintptr // *GpGraphics
}

// paintBuffered runs a WM_PAINT for hwnd: BeginPaint, a memory DC the size of
// the client area, paint(c, client rect), one BitBlt, cleanup, EndPaint.
// Use it from a WM_PAINT handler and return 0; pair it with a WM_ERASEBKGND
// handler returning 1, so nothing flickers.
func paintBuffered(hwnd win.HWND, paint func(c *canvas, rc win.RECT)) {
	var ps paintStruct
	dc, _, _ := procBeginPaint.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&ps)))
	defer procEndPaint.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&ps)))
	if dc == 0 {
		return
	}
	rc, err := hwnd.GetClientRect()
	if err != nil || rc.Right <= 0 || rc.Bottom <= 0 {
		return // minimised or not laid out yet: nothing to draw
	}
	s, err := newSurface(rc.Right, rc.Bottom)
	if err != nil {
		return
	}
	defer s.free()
	c := newCanvas(win.HDC(s.dc))
	paint(c, rc)
	c.release()
	procBitBlt.Call(dc, 0, 0, uintptr(rc.Right), uintptr(rc.Bottom), s.dc, 0, 0, srcCopy)
}

// newCanvas wraps hdc in a GDI+ Graphics set up for crisp antialiasing.
// Pair with release. Exposed for tests and for painting outside WM_PAINT.
func newCanvas(hdc win.HDC) *canvas {
	startGdiplus()
	c := &canvas{hdc: hdc}
	procGdipCreateFromHDC.Call(uintptr(hdc), uintptr(unsafe.Pointer(&c.g)))
	if c.g != 0 {
		procGdipSetSmoothing.Call(c.g, gpSmoothingAntiAlias)
		procGdipSetPixelOffset.Call(c.g, gpPixelOffsetHalf)
	}
	return c
}

func (c *canvas) release() {
	if c.g != 0 {
		procGdipDeleteGraphics.Call(c.g)
		c.g = 0
	}
}

// withBrush / withPen create a GDI+ object for one call and delete it after.
func withBrush(col argb, fn func(brush uintptr)) {
	var b uintptr
	if procGdipCreateSolid.Call(uintptr(col), uintptr(unsafe.Pointer(&b))); b == 0 {
		return
	}
	defer procGdipDeleteBrush.Call(b)
	fn(b)
}

func withPen(col argb, width float32, fn func(pen uintptr)) {
	var p uintptr
	if procGdipCreatePen1.Call(uintptr(col), f32(width), gpUnitPixel, uintptr(unsafe.Pointer(&p))); p == 0 {
		return
	}
	defer procGdipDeletePen.Call(p)
	fn(p)
}

// roundPath builds a closed rounded rectangle path; radius <= 0 is a plain
// rectangle. The caller deletes it.
func roundPath(x, y, w, h, radius float32) uintptr {
	var path uintptr
	procGdipCreatePath.Call(gpFillAlternate, uintptr(unsafe.Pointer(&path)))
	if path == 0 {
		return 0
	}
	d := min(2*radius, w, h)
	if d <= 0 {
		procGdipAddPathRect.Call(path, f32(x), f32(y), f32(w), f32(h))
		return path
	}
	arc := func(ax, ay, start float32) {
		procGdipAddPathArc.Call(path, f32(ax), f32(ay), f32(d), f32(d), f32(start), f32(90))
	}
	arc(x, y, 180)
	arc(x+w-d, y, 270)
	arc(x+w-d, y+h-d, 0)
	arc(x, y+h-d, 90)
	procGdipClosePathFig.Call(path)
	return path
}

func rectF(rc win.RECT) (x, y, w, h float32) {
	return float32(rc.Left), float32(rc.Top), float32(rc.Right - rc.Left), float32(rc.Bottom - rc.Top)
}

func (c *canvas) fillRect(rc win.RECT, col argb) {
	x, y, w, h := rectF(rc)
	withBrush(col, func(b uintptr) {
		procGdipFillRectangle.Call(c.g, b, f32(x), f32(y), f32(w), f32(h))
	})
}

func (c *canvas) fillRoundRect(rc win.RECT, radius int, col argb) {
	x, y, w, h := rectF(rc)
	path := roundPath(x, y, w, h, float32(radius))
	if path == 0 {
		return
	}
	defer procGdipDeletePath.Call(path)
	withBrush(col, func(b uintptr) { procGdipFillPath.Call(c.g, b, path) })
}

// strokeRoundRect draws the outline inside rc: the pen is centred on a path
// inset by width/2, so a 1px stroke covers exactly the rectangle's edge
// pixels instead of blurring across two.
func (c *canvas) strokeRoundRect(rc win.RECT, radius int, col argb, width float32) {
	x, y, w, h := rectF(rc)
	in := width / 2
	path := roundPath(x+in, y+in, w-width, h-width, max(float32(radius)-in, 0))
	if path == 0 {
		return
	}
	defer procGdipDeletePath.Call(path)
	withPen(col, width, func(p uintptr) { procGdipDrawPath.Call(c.g, p, path) })
}

func (c *canvas) fillEllipse(rc win.RECT, col argb) {
	x, y, w, h := rectF(rc)
	withBrush(col, func(b uintptr) {
		procGdipFillEllipse.Call(c.g, b, f32(x), f32(y), f32(w), f32(h))
	})
}

// strokeEllipse insets like strokeRoundRect.
func (c *canvas) strokeEllipse(rc win.RECT, col argb, width float32) {
	x, y, w, h := rectF(rc)
	in := width / 2
	withPen(col, width, func(p uintptr) {
		procGdipDrawEllipse.Call(c.g, p, f32(x+in), f32(y+in), f32(w-width), f32(h-width))
	})
}

// line runs through the centres of pixels (x1, y1) and (x2, y2).
func (c *canvas) line(x1, y1, x2, y2 int, col argb, width float32) {
	withPen(col, width, func(p uintptr) {
		procGdipDrawLine.Call(c.g, p, f32(float32(x1)+0.5), f32(float32(y1)+0.5),
			f32(float32(x2)+0.5), f32(float32(y2)+0.5))
	})
}

// clip limits the following shapes to rc until unclip. GDI text ignores it.
func (c *canvas) clip(rc win.RECT) {
	procGdipSetClipRectI.Call(c.g, uintptr(rc.Left), uintptr(rc.Top),
		uintptr(rc.Right-rc.Left), uintptr(rc.Bottom-rc.Top), gpCombineReplace)
}

func (c *canvas) unclip() { procGdipResetClip.Call(c.g) }

// gdiDC hands the canvas's DC to GDI: GDI+ may hold drawing back in a batch,
// and GdipGetDC flushes it first, so text lands over the shapes drawn before.
func (c *canvas) gdiDC(fn func(dc uintptr)) {
	if c.g == 0 {
		fn(uintptr(c.hdc))
		return
	}
	var dc uintptr
	if procGdipGetDC.Call(c.g, uintptr(unsafe.Pointer(&dc))); dc == 0 {
		return
	}
	defer procGdipReleaseDC.Call(c.g, dc)
	fn(dc)
}

// text draws s in rc with flags as given (co.DT_*); 0 means one line,
// vertically centred, left-aligned, with an end ellipsis and no '&' prefix.
func (c *canvas) text(s string, rc win.RECT, role fontRole, col argb, flags co.DT) {
	if flags == 0 {
		flags = fluentDefaultFlags
	}
	c.gdiDC(func(dc uintptr) { drawWithFont(dc, uintptr(fluentFont(role)), s, col, &rc, uint32(flags)) })
}

// glyph draws an icon-font code point centred in rc, sizePx tall.
func (c *canvas) glyph(g rune, rc win.RECT, sizePx int, col argb) {
	c.gdiDC(func(dc uintptr) {
		drawWithFont(dc, glyphFont(sizePx), string(g), col, &rc, dtCenter|dtVCenter|dtSingleLine|dtNoPrefix)
	})
}

// measure returns the size of s in role's font, on one line. It works on a
// nil canvas too (layout before any paint), through a screen DC.
func (c *canvas) measure(s string, role fontRole) (cx, cy int) {
	calc := func(dc uintptr) {
		var rc win.RECT
		drawWithFont(dc, uintptr(fluentFont(role)), s, 0, &rc, dtCalcRect|dtSingleLine|dtNoPrefix)
		cx, cy = int(rc.Right-rc.Left), int(rc.Bottom-rc.Top)
	}
	if c == nil || c.hdc == 0 {
		withScreenDC(calc)
	} else {
		c.gdiDC(calc)
	}
	return cx, cy
}

const (
	dtCenter = 0x0001
)

// drawWithFont is DrawTextW with font selected and a transparent background.
func drawWithFont(dc, font uintptr, s string, col argb, rc *win.RECT, format uint32) {
	if font != 0 {
		old, _, _ := procSelectObject.Call(dc, font)
		defer procSelectObject.Call(dc, old)
	}
	procSetBkMode.Call(dc, bkTransparent)
	procSetTextColor.Call(dc, uintptr(col.colorref()))
	p, _ := windows.UTF16FromString(s)
	if len(p) <= 1 && format&dtCalcRect == 0 {
		return
	}
	procDrawTextW.Call(dc, uintptr(unsafe.Pointer(&p[0])), uintptr(len(p)-1), uintptr(unsafe.Pointer(rc)), uintptr(format))
}

func withScreenDC(fn func(dc uintptr)) {
	dc, _, _ := procFdGetDC.Call(0)
	if dc == 0 {
		return
	}
	defer procFdReleaseDC.Call(0, dc)
	fn(dc)
}

// fontRole names a Fluent type ramp entry (Segoe UI Variable, falling back
// to Segoe UI on Windows 10).
type fontRole int

const (
	fontBody       fontRole = iota // 14px regular: row titles, inputs, buttons
	fontBodyStrong                 // 14px semibold: InfoBar title
	fontCaption                    // 12px regular: row subtitles, footer note, badges
	fontSubtitle                   // 20px semibold: page title
)

var (
	fontMu     sync.Mutex
	roleFonts  = map[fontRole]win.HFONT{}
	glyphFonts = map[int]uintptr{}
	faceProbe  = map[string]bool{}
)

// fluentFont returns the role's font scaled to the system DPI, created once
// and kept for the process lifetime. Also for native controls (WM_SETFONT).
func fluentFont(role fontRole) win.HFONT {
	fontMu.Lock()
	defer fontMu.Unlock()
	if f, ok := roleFonts[role]; ok {
		return f
	}
	face, size, weight := "Segoe UI Variable Text", 14, 400
	switch role {
	case fontBodyStrong:
		weight = 600
	case fontCaption:
		size = 12
	case fontSubtitle:
		face, size, weight = "Segoe UI Variable Display", 20, 600
	}
	if !faceInstalled(face) {
		face = "Segoe UI" // Windows 10
	}
	// GDI sees a variable font's semibold instance as a family of its own
	// ("... Semibold"): weight 600 on the regular family is emboldened
	// synthetically, wider and smudgier than the real instance.
	if weight == 600 && faceInstalled(face+" Semibold") {
		face += " Semibold"
	}
	f := win.HFONT(createFont(face, ui.DpiY(size), weight))
	roleFonts[role] = f
	return f
}

// glyphFont is the icon font sizePx tall (physical pixels), cached per size.
func glyphFont(sizePx int) uintptr {
	fontMu.Lock()
	defer fontMu.Unlock()
	if f, ok := glyphFonts[sizePx]; ok {
		return f
	}
	f := createFontQ(iconFontFaceLocked(), sizePx, 400, antialiasedQuality)
	glyphFonts[sizePx] = f
	return f
}

// iconFontFace is "Segoe Fluent Icons" when installed, else "Segoe MDL2 Assets".
func iconFontFace() string {
	fontMu.Lock()
	defer fontMu.Unlock()
	return iconFontFaceLocked()
}

func iconFontFaceLocked() string {
	if faceInstalled("Segoe Fluent Icons") {
		return "Segoe Fluent Icons"
	}
	return "Segoe MDL2 Assets"
}

func createFont(face string, heightPx, weight int) uintptr {
	return createFontQ(face, heightPx, weight, clearTypeQualty)
}

// antialiasedQuality is grayscale antialiasing, for the icon font: ClearType
// gives a glyph's thin strokes visible colour fringes, which text hides at
// reading size but an isolated icon does not.
const antialiasedQuality = 4

func createFontQ(face string, heightPx, weight, quality int) uintptr {
	p, _ := windows.UTF16PtrFromString(face)
	f, _, _ := procCreateFontW.Call(uintptr(-heightPx), 0, 0, 0, uintptr(weight), 0, 0, 0,
		1 /* DEFAULT_CHARSET */, 0, 0, uintptr(quality), 0, uintptr(unsafe.Pointer(p)))
	return f
}

// faceInstalled reports whether GDI maps face to itself rather than to a
// substitute: a font asked for by a name that is not installed comes back
// as another face from GetTextFace. Cached; call with fontMu held.
func faceInstalled(face string) bool {
	if ok, done := faceProbe[face]; done {
		return ok
	}
	ok := false
	f := createFont(face, 16, 400)
	if f != 0 {
		withScreenDC(func(dc uintptr) {
			old, _, _ := procSelectObject.Call(dc, f)
			var buf [64]uint16
			n, _, _ := procFdGetTextFace.Call(dc, uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
			procSelectObject.Call(dc, old)
			ok = n > 0 && windows.UTF16ToString(buf[:]) == face
		})
		procDeleteObject.Call(f)
	}
	faceProbe[face] = ok
	return ok
}

// paintCard draws a card: rounded cardBg fill, 1px cardStroke, and a divider
// line across the card at each y in dividers (absolute client y).
func paintCard(c *canvas, rc win.RECT, dividers []int) {
	p := fp()
	r := ui.DpiX(cardRadius)
	c.fillRoundRect(rc, r, p.cardBg)
	for _, y := range dividers {
		c.fillRect(win.RECT{Left: rc.Left + 1, Top: int32(y), Right: rc.Right - 1, Bottom: int32(y) + 1}, p.divider)
	}
	c.strokeRoundRect(rc, r, p.cardStroke, 1)
}

// paintRowLabel draws a settings row's title (fontBody) and, when not empty,
// its subtitle (fontCaption, textSecondary) left-aligned and vertically
// centred in rc. Disabled rows use textDisabled for both.
func paintRowLabel(c *canvas, rc win.RECT, title, subtitle string, enabled bool) {
	p := fp()
	titleCol, subCol := p.text, p.textSecondary
	if !enabled {
		titleCol, subCol = p.textDisabled, p.textDisabled
	}
	_, th := c.measure(title, fontBody)
	if subtitle == "" {
		c.text(title, rc, fontBody, titleCol, 0)
		return
	}
	_, sh := c.measure(subtitle, fontCaption)
	gap := ui.DpiY(2)
	top := int(rc.Top) + (int(rc.Bottom-rc.Top)-(th+gap+sh))/2
	flags := co.DT(dtLeft | dtSingleLine | dtNoPrefix | dtEndEllipsis)
	c.text(title, win.RECT{Left: rc.Left, Top: int32(top), Right: rc.Right, Bottom: int32(top + th)}, fontBody, titleCol, flags)
	top += th + gap
	c.text(subtitle, win.RECT{Left: rc.Left, Top: int32(top), Right: rc.Right, Bottom: int32(top + sh)}, fontCaption, subCol, flags)
}

// paintInputFrame draws the Fluent text-box chrome around a borderless
// native Edit/ComboBox: rounded inputBg fill, inputStroke outline and a 1px
// inputBottom bottom edge (2px inputFocusBottom when focused). rc is the
// frame; the native control sits inset by inputInset.
func paintInputFrame(c *canvas, rc win.RECT, focused, enabled bool) {
	p := fp()
	r := ui.DpiX(controlRadius)
	c.fillRoundRect(rc, r, p.inputBg)
	c.strokeRoundRect(rc, r, p.inputStroke, 1)
	if !enabled {
		return
	}
	// The bottom edge is the frame's own rounded shape clipped to its last
	// one or two rows, so it follows the corners as WinUI's does.
	edge, col := int32(1), p.inputBottom
	if focused {
		edge, col = 2, p.inputFocusBottom
	}
	c.clip(win.RECT{Left: rc.Left, Top: rc.Bottom - edge, Right: rc.Right, Bottom: rc.Bottom})
	c.fillRoundRect(rc, r, col)
	c.unclip()
}

// inputInset is the gap, in 96-DPI units, between an input frame and the
// native control inside it.
const inputInset = 6
