//go:build windows

package tray

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"math"
	"syscall"
	"time"
	"unsafe"

	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
	"golang.org/x/sys/windows"

	"emlyupdater/internal/version"
)

// splash is the borderless window shown in place of the settings window while
// it loads (backend.load: config.ini, the policy cache, the DC lookup), so a
// double click on the icon answers at once instead of opening a window full
// of disabled controls. Always dark, whatever the theme: it is artwork, not a
// form. The background (gradient and swoosh) is computed pixel by pixel once
// per show; the logo and text are drawn over it once; each tick then only
// repaints the indeterminate bar, from a back buffer, and every dotTicks the
// status line, whose trailing dots cycle ... .. . .. as a second sign of life.
//
// A raw Win32 window rather than a windigo one: windigo has no borderless
// top-level window, and this one has no controls and no events to wire.
// windigo's message loop dispatches its messages like any other window's.
// It is owned by the settings window, so it never gets a taskbar button and
// goes away with it. UI thread only.
type splash struct {
	owner  win.HWND
	hwnd   uintptr
	w, h   int32
	art    gdiSurface // background, logo and text: drawn once per show
	back   gdiSurface // art + bar, blitted to the screen
	bar    windows.Rect
	phase  int
	shown  time.Time
	status string // without the trailing dots, which paint animates

	statusFont uintptr
	statusRc   windows.Rect
	ticks      int
}

// splashMinShow keeps the splash up at least this long: a load that answers
// from the cache in a few ms would otherwise flash it.
const splashMinShow = 700 * time.Millisecond

// statusDots is the status line's trailing dots, one entry per dotTicks:
// ... .. . .. and round again.
var statusDots = [...]string{"...", "..", ".", ".."}

const dotTicks = 30 // ≈ 450 ms at barTickMs

const (
	splashTimer  = 1
	splashClass  = "AryxDAgentSplash"
	splashWidth  = 520 // logical pixels, scaled with ui.Dpi
	splashHeight = 250
)

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	procRegisterClassExW = user32.NewProc("RegisterClassExW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
	procShowWindow       = user32.NewProc("ShowWindow")
	procGetWindowRect    = user32.NewProc("GetWindowRect")
	procSetForegroundWnd = user32.NewProc("SetForegroundWindow")
	procBeginPaint       = user32.NewProc("BeginPaint")
	procEndPaint         = user32.NewProc("EndPaint")
	procSetTimer         = user32.NewProc("SetTimer")
	procKillTimer        = user32.NewProc("KillTimer")
	procInvalidateRect   = user32.NewProc("InvalidateRect")
	procDrawTextW        = user32.NewProc("DrawTextW")
	procLoadCursorW      = user32.NewProc("LoadCursorW")

	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procDeleteObject       = gdi32.NewProc("DeleteObject")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
	procBitBlt             = gdi32.NewProc("BitBlt")
	procCreateFontW        = gdi32.NewProc("CreateFontW")
	procSetTextColor       = gdi32.NewProc("SetTextColor")
	procSetBkMode          = gdi32.NewProc("SetBkMode")

	// The window procedure, one for the process: syscall.NewCallback slots
	// are never freed. splashes maps each live window to its splash.
	splashProc   = syscall.NewCallback(splashWndProc)
	splashes     = map[uintptr]*splash{}
	splashClassD bool
)

const (
	wsPopup         = 0x80000000
	wsExToolWindow  = 0x00000080
	csDropShadow    = 0x00020000
	wmDestroy       = 0x0002
	wmPaint         = 0x000F
	wmEraseBkgnd    = 0x0014
	wmTimer         = 0x0113
	srcCopy         = 0x00CC0020
	dtLeft          = 0x0000
	dtSingleLine    = 0x0020
	dtNoPrefix      = 0x0800
	dtEndEllipsis   = 0x8000
	dtRight         = 0x0002
	dtVCenter       = 0x0004
	bkTransparent   = 1
	clearTypeQualty = 5
	idcArrow        = 32512
)

type wndClassEx struct {
	cbSize, style           uint32
	lpfnWndProc             uintptr
	cbClsExtra, cbWndExtra  int32
	hInstance, hIcon        uintptr
	hCursor, hbrBackground  uintptr
	lpszMenuName, lpszClass *uint16
	hIconSm                 uintptr
}

type paintStruct struct {
	hdc         uintptr
	fErase      int32
	rcPaint     windows.Rect
	fRestore    int32
	fIncUpdate  int32
	rgbReserved [32]byte
}

type bitmapInfoHeader struct {
	biSize                       uint32
	biWidth, biHeight            int32
	biPlanes, biBitCount         uint16
	biCompression, biSizeImage   uint32
	biXPelsPerMeter, biYPelsPerM int32
	biClrUsed, biClrImportant    uint32
}

// gdiSurface is a memory DC with a 32bpp top-down DIB selected into it.
type gdiSurface struct {
	dc, bmp, old uintptr
	pix          []uint32 // BGRA, row-major, w*h
}

func newSurface(w, h int32) (gdiSurface, error) {
	var s gdiSurface
	bi := bitmapInfoHeader{biWidth: w, biHeight: -h, biPlanes: 1, biBitCount: 32}
	bi.biSize = uint32(unsafe.Sizeof(bi))
	var bits unsafe.Pointer
	s.dc, _, _ = procCreateCompatibleDC.Call(0)
	if s.dc == 0 {
		return s, fmt.Errorf("CreateCompatibleDC failed")
	}
	s.bmp, _, _ = procCreateDIBSection.Call(s.dc, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if s.bmp == 0 || bits == nil {
		procDeleteDC.Call(s.dc)
		return gdiSurface{}, fmt.Errorf("CreateDIBSection failed")
	}
	s.old, _, _ = procSelectObject.Call(s.dc, s.bmp)
	s.pix = unsafe.Slice((*uint32)(bits), int(w)*int(h))
	return s, nil
}

func (s *gdiSurface) free() {
	if s.dc == 0 {
		return
	}
	procSelectObject.Call(s.dc, s.old)
	procDeleteObject.Call(s.bmp)
	procDeleteDC.Call(s.dc)
	*s = gdiSurface{}
}

func newSplash(owner win.HWND) *splash {
	return &splash{owner: owner, status: "Caricamento della configurazione"}
}

// show puts the splash up, centred over the (hidden) settings window, where
// that window will appear. A failure leaves no splash: the caller goes on
// and shows the settings window when the load ends, as before.
func (s *splash) show() {
	if s.hwnd != 0 {
		procSetForegroundWnd.Call(s.hwnd)
		return
	}
	s.w, s.h = int32(ui.DpiX(splashWidth)), int32(ui.DpiY(splashHeight))
	if !registerSplashClass() || s.render() != nil {
		s.release()
		return
	}

	var or windows.Rect
	procGetWindowRect.Call(uintptr(s.owner), uintptr(unsafe.Pointer(&or)))
	x := or.Left + (or.Right-or.Left-s.w)/2
	y := or.Top + (or.Bottom-or.Top-s.h)/2

	cls, _ := windows.UTF16PtrFromString(splashClass)
	title, _ := windows.UTF16PtrFromString(version.ProductName)
	h, _, _ := procCreateWindowExW.Call(wsExToolWindow, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(title)),
		wsPopup, uintptr(x), uintptr(y), uintptr(s.w), uintptr(s.h), uintptr(s.owner), 0, 0, 0)
	if h == 0 {
		s.release()
		return
	}
	s.hwnd = h
	splashes[h] = s

	// Rounded corners and the system shadow, as on any Windows 11 window.
	corner := int32(dwmwcpRound)
	procDwmSetWindowAttr.Call(h, dwmwaWindowCornerPref, uintptr(unsafe.Pointer(&corner)), unsafe.Sizeof(corner))

	s.phase, s.ticks = 0, 0
	s.shown = time.Now()
	procShowWindow.Call(h, uintptr(co.SW_SHOW))
	procSetForegroundWnd.Call(h)
	procSetTimer.Call(h, splashTimer, barTickMs, 0)
}

// hide takes the splash down; a no-op when it is not up.
func (s *splash) hide() {
	if s.hwnd == 0 {
		return
	}
	procKillTimer.Call(s.hwnd, splashTimer)
	procDestroyWindow.Call(s.hwnd) // WM_DESTROY releases the surfaces
}

// release frees the drawing surfaces and forgets the window.
func (s *splash) release() {
	delete(splashes, s.hwnd)
	s.hwnd = 0
	s.art.free()
	s.back.free()
	if s.statusFont != 0 {
		procDeleteObject.Call(s.statusFont)
		s.statusFont = 0
	}
}

func registerSplashClass() bool {
	if splashClassD {
		return true
	}
	cls, _ := windows.UTF16PtrFromString(splashClass)
	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
	wc := wndClassEx{style: csDropShadow, lpfnWndProc: splashProc, hCursor: cursor, lpszClass: cls}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	if r, _, _ := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return false
	}
	splashClassD = true
	return true
}

func splashWndProc(hwnd, msg, wp, lp uintptr) uintptr {
	s := splashes[hwnd]
	if s == nil {
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
		return r
	}
	switch msg {
	case wmEraseBkgnd:
		return 1 // WM_PAINT covers every pixel
	case wmPaint:
		var ps paintStruct
		dc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		s.paint(dc)
		procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0
	case wmTimer:
		s.phase = (s.phase + 1) % barPhases
		procInvalidateRect.Call(hwnd, uintptr(unsafe.Pointer(&s.bar)), 0)
		if s.ticks++; s.ticks%dotTicks == 0 {
			procInvalidateRect.Call(hwnd, uintptr(unsafe.Pointer(&s.statusRc)), 0)
		}
		return 0
	case wmDestroy:
		s.release()
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

// paint copies the artwork into the back buffer, draws the status line and
// the bar over it and blits the result: neither flickers.
func (s *splash) paint(dc uintptr) {
	procBitBlt.Call(s.back.dc, 0, 0, uintptr(s.w), uintptr(s.h), s.art.dc, 0, 0, srcCopy)

	dots := statusDots[(s.ticks/dotTicks)%len(statusDots)]
	drawText(s.back.dc, s.statusFont, s.status+dots, win.RGB(214, 211, 224), s.statusRc, dtLeft|dtVCenter)

	r := s.bar
	fillColor(win.HDC(s.back.dc), win.RECT{Left: r.Left, Top: r.Top, Right: r.Right, Bottom: r.Bottom},
		win.RGB(58, 44, 100))
	width := r.Right - r.Left
	seg := width * barSegShare / 100
	left := r.Left - seg + (width+seg)*int32(s.phase)/barPhases
	fillColor(win.HDC(s.back.dc), win.RECT{Left: max(left, r.Left), Top: r.Top, Right: min(left+seg, r.Right), Bottom: r.Bottom},
		win.RGB(227, 176, 18))

	procBitBlt.Call(dc, 0, 0, uintptr(s.w), uintptr(s.h), s.back.dc, 0, 0, srcCopy)
}

// render draws the artwork: background, logo, product name, status line and
// footer. Coordinates are logical pixels through ui.Dpi, like the forms'.
func (s *splash) render() error {
	var err error
	if s.art, err = newSurface(s.w, s.h); err != nil {
		return err
	}
	if s.back, err = newSurface(s.w, s.h); err != nil {
		return err
	}
	paintBackground(s.art.pix, int(s.w), int(s.h))

	dc := s.art.dc
	procSetBkMode.Call(dc, bkTransparent)
	px := func(v int) int32 { return int32(ui.DpiX(v)) }

	drawLogo(s.art.pix, int(s.w), int(px(20)), int(px(36)), int(px(48)))
	text := func(str string, size, weight int, c win.COLORREF, l, t, r, b int32, format uint32) {
		font := splashFont(px(size), weight)
		if font != 0 {
			defer procDeleteObject.Call(font)
		}
		drawText(dc, font, str, c, windows.Rect{Left: l, Top: t, Right: r, Bottom: b}, format)
	}

	white, soft, dim := win.RGB(246, 245, 250), win.RGB(214, 211, 224), win.RGB(152, 148, 166)
	text(version.ProductName, 30, 600, white, px(78), px(32), s.w-px(20), px(72), dtLeft|dtVCenter)
	text("Agente di distribuzione e gestione", 19, 300, white, px(78), px(74), s.w-px(20), px(102), dtLeft|dtVCenter)
	// The status line is drawn by paint, over the artwork, for its dots.
	procSetBkMode.Call(s.back.dc, bkTransparent)
	s.statusFont = splashFont(px(13), 400)
	s.statusRc = windows.Rect{Left: px(20), Top: px(140), Right: s.w - px(20), Bottom: px(162)}
	text("Versione "+version.Version, 12, 400, dim, px(20), s.h-px(36), s.w/2, s.h-px(14), dtLeft|dtVCenter)
	text(fmt.Sprintf("© %d 3gIT", time.Now().Year()), 12, 400, soft, s.w/2, s.h-px(36), s.w-px(20), s.h-px(14), dtRight|dtVCenter)

	s.bar = windows.Rect{Left: px(20), Top: px(168), Right: s.w - px(20), Bottom: px(168) + max(px(3), 2)}
	return nil
}

// splashFont creates a Segoe UI font height pixels tall; 0 on failure, in
// which case DrawText falls back to the DC's default font.
func splashFont(height int32, weight int) uintptr {
	face, _ := windows.UTF16PtrFromString("Segoe UI")
	font, _, _ := procCreateFontW.Call(uintptr(int(-height)), 0, 0, 0, uintptr(weight), 0, 0, 0, 0, 0, 0,
		clearTypeQualty, 0, uintptr(unsafe.Pointer(face)))
	return font
}

// drawText draws one line of str in rc with font (when non-zero) and colour c.
func drawText(dc, font uintptr, str string, c win.COLORREF, rc windows.Rect, format uint32) {
	if font != 0 {
		old, _, _ := procSelectObject.Call(dc, font)
		defer procSelectObject.Call(dc, old)
	}
	procSetTextColor.Call(dc, uintptr(c))
	p, _ := windows.UTF16FromString(str)
	procDrawTextW.Call(dc, uintptr(unsafe.Pointer(&p[0])), uintptr(len(p)-1), uintptr(unsafe.Pointer(&rc)),
		uintptr(format|dtSingleLine|dtNoPrefix|dtEndEllipsis))
}

// paintBackground fills pix (BGRA, w*h) with the splash's artwork: a dark
// charcoal gradient, a grey disc in the top-right corner with a violet glow
// along its lower edge, and a violet swoosh rising from the bottom-right
// corner - the logo's own palette (charcoal #202020, violet #6a3ae6, with
// its yellow #e3b012 kept for the bar). Every edge is antialiased over one pixel.
func paintBackground(pix []uint32, w, h int) {
	W, H := float64(w), float64(h)
	type rgb struct{ r, g, b float64 }
	mix := func(a, b rgb, t float64) rgb {
		t = math.Max(0, math.Min(1, t))
		return rgb{a.r + (b.r-a.r)*t, a.g + (b.g-a.g)*t, a.b + (b.b-a.b)*t}
	}
	smooth := func(e0, e1, x float64) float64 {
		t := math.Max(0, math.Min(1, (x-e0)/(e1-e0)))
		return t * t * (3 - 2*t)
	}

	left, right := rgb{36, 33, 44}, rgb{20, 20, 22}
	grey, greyIn := rgb{42, 42, 45}, rgb{30, 30, 32}
	violet, violetHi, violetLo := rgb{106, 58, 230}, rgb{160, 124, 255}, rgb{48, 26, 112}

	// Grey disc, top right.
	gx, gy, gr := W*1.02, -H*0.10, H*0.95
	// Teal swoosh: a disc whose top-left edge crosses the lower right.
	sx, sy, sr := W*1.18, H*1.45, H*0.98
	glowW := H * 0.12

	for y := 0; y < h; y++ {
		fy := float64(y) + 0.5
		for x := 0; x < w; x++ {
			fx := float64(x) + 0.5
			c := mix(left, right, fx/W*0.85+fy/H*0.15)

			// Swoosh: violet body, darker inside, with a bright rim.
			ds := math.Hypot(fx-sx, fy-sy)
			if in := sr - ds + 0.5; in > 0 {
				body := mix(violet, violetLo, (sr-ds)/(sr*0.55))
				c = mix(c, body, in)
			}
			fade := smooth(W*0.30, W*0.80, fx)
			c = mix(c, violetHi, math.Exp(-math.Pow((ds-sr)/1.6, 2))*0.55*fade)

			// Glow outside the grey disc, strongest towards the bottom.
			dg := math.Hypot(fx-gx, fy-gy)
			if out := dg - gr; out > 0 {
				c = mix(c, violet, math.Exp(-out/glowW)*0.55*smooth(W*0.35, W*0.85, fx)*smooth(0, H*0.7, fy))
			}
			if in := gr - dg + 0.5; in > 0 {
				body := mix(greyIn, grey, (gr-dg)/(gr*0.6))
				c = mix(c, body, in)
			}

			pix[y*w+x] = uint32(c.b) | uint32(c.g)<<8 | uint32(c.r)<<16 | 0xFF<<24
		}
	}
}

// splashLogo is the agent's logo at 512 px (EMLy's build/appicon.png): the
// exe's own icon is at most 32 px through ExtractIconEx, blurry at 48.
//
//go:embed splash_logo.png
var splashLogo []byte

// drawLogo scales the logo down to size x size by area averaging (on
// premultiplied colour, so the transparent corners leave no dark fringe) and
// blends it into pix (BGRA, stride w) at x, y.
func drawLogo(pix []uint32, w, x, y, size int) {
	src, err := png.Decode(bytes.NewReader(splashLogo))
	if err != nil || size <= 0 {
		return
	}
	b := src.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy())) // premultiplied
	draw.Draw(rgba, rgba.Bounds(), src, b.Min, draw.Src)
	sw, sh := b.Dx(), b.Dy()
	h := len(pix) / w

	for dy := 0; dy < size; dy++ {
		y0, y1 := dy*sh/size, max((dy+1)*sh/size, dy*sh/size+1)
		for dx := 0; dx < size; dx++ {
			x0, x1 := dx*sw/size, max((dx+1)*sw/size, dx*sw/size+1)
			var r, g, bl, a, n int
			for sy := y0; sy < y1; sy++ {
				row := rgba.Pix[sy*rgba.Stride:]
				for sx := x0; sx < x1; sx++ {
					p := row[sx*4:]
					r, g, bl, a = r+int(p[0]), g+int(p[1]), bl+int(p[2]), a+int(p[3])
					n++
				}
			}
			px, py := x+dx, y+dy
			if px < 0 || py < 0 || px >= w || py >= h {
				continue
			}
			r, g, bl, a = r/n, g/n, bl/n, a/n
			d := pix[py*w+px]
			inv := 255 - a
			db, dg, dr := int(d&0xFF), int(d>>8&0xFF), int(d>>16&0xFF)
			nb, ng, nr := bl+db*inv/255, g+dg*inv/255, r+dr*inv/255
			pix[py*w+px] = uint32(nb) | uint32(ng)<<8 | uint32(nr)<<16 | 0xFF<<24
		}
	}
}
