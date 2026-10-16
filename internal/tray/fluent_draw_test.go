//go:build windows

package tray

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
	"golang.org/x/sys/windows"
)

func TestFluentFontsAndMeasure(t *testing.T) {
	for _, r := range []fontRole{fontBody, fontBodyStrong, fontCaption, fontSubtitle} {
		if fluentFont(r) == 0 {
			t.Fatalf("fluentFont(%d) = 0", r)
		}
		if fluentFont(r) != fluentFont(r) {
			t.Fatalf("fluentFont(%d) not cached", r)
		}
	}
	var c *canvas
	w, h := c.measure("Connessione", fontBody)
	sw, sh := c.measure("Connessione", fontSubtitle)
	if w <= 0 || h <= 0 || sw <= w || sh <= h {
		t.Fatalf("measure: body %dx%d, subtitle %dx%d", w, h, sw, sh)
	}
	if f := iconFontFace(); f != "Segoe Fluent Icons" && f != "Segoe MDL2 Assets" {
		t.Fatalf("iconFontFace = %q", f)
	}
}

var (
	procFdtPeekMessage    = user32.NewProc("PeekMessageW")
	procFdtTranslateMsg   = user32.NewProc("TranslateMessage")
	procFdtDispatchMsg    = user32.NewProc("DispatchMessageW")
	procFdtUpdateWindow   = user32.NewProc("UpdateWindow")
	procFdtGetModuleHndle = kernel32.NewProc("GetModuleHandleW")
)

// TestFluentDrawLive paints a swatch of every primitive in both palettes and
// saves screenshots to look at; it asserts nothing about the pixels.
//
//	$env:EMLY_FLUENT_TEST=1; go test ./internal/tray/ -run FluentDrawLive -v
//
// EMLY_FLUENT_SHOTS sets the output directory (default: the system temp).
func TestFluentDrawLive(t *testing.T) {
	if os.Getenv("EMLY_FLUENT_TEST") != "1" {
		t.Skip("set EMLY_FLUENT_TEST=1 to run (needs a desktop)")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	dir := os.Getenv("EMLY_FLUENT_SHOTS")
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "fluent-shots")
	}

	hinst, _, _ := procFdtGetModuleHndle.Call(0)
	cls, _ := windows.UTF16PtrFromString("AryxDFluentDrawTest")
	proc := syscall.NewCallback(func(hwnd, msg, wp, lp uintptr) uintptr {
		switch msg {
		case wmEraseBkgnd:
			return 1
		case wmPaint:
			paintBuffered(win.HWND(hwnd), paintSwatch)
			return 0
		}
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
		return r
	})
	wc := wndClassEx{lpfnWndProc: proc, hInstance: hinst, lpszClass: cls}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	const wsOverlappedWindow = 0x00CF0000
	title, _ := windows.UTF16PtrFromString("Fluent draw test")
	w, h := ui.Dpi(720, 600)
	hwnd, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(title)),
		wsOverlappedWindow, 40, 40, uintptr(w), uintptr(h), 0, 0, hinst, 0)
	if hwnd == 0 {
		t.Fatal(err)
	}
	defer procDestroyWindow.Call(hwnd)
	procShowWindow.Call(hwnd, uintptr(co.SW_SHOWNORMAL))

	for _, mode := range []struct {
		name string
		mode int
	}{{"dark", themeDark}, {"light", themeLight}} {
		themeMode = mode.mode
		applyFrame(win.HWND(hwnd), mode.mode == themeDark)
		procInvalidateRect.Call(hwnd, 0, 0)
		procFdtUpdateWindow.Call(hwnd)
		pump(400 * time.Millisecond)
		path := filepath.Join(dir, "draw_"+mode.name+".png")
		if err := captureWindowPNG(win.HWND(hwnd), path); err != nil {
			t.Fatal(err)
		}
		t.Log("saved", path)
	}
}

// pump runs the thread's message loop for d, so DWM composes the window
// before PrintWindow reads it.
func pump(d time.Duration) {
	var msg [48]byte // MSG
	for end := time.Now().Add(d); time.Now().Before(end); {
		for {
			r, _, _ := procFdtPeekMessage.Call(uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0, 1 /* PM_REMOVE */)
			if r == 0 {
				break
			}
			procFdtTranslateMsg.Call(uintptr(unsafe.Pointer(&msg[0])))
			procFdtDispatchMsg.Call(uintptr(unsafe.Pointer(&msg[0])))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func paintSwatch(c *canvas, rc win.RECT) {
	p := fp()
	px := func(v int) int32 { return int32(ui.DpiX(v)) }
	box := func(l, t, r, b int) win.RECT { return win.RECT{Left: px(l), Top: px(t), Right: px(r), Bottom: px(b)} }

	c.fillRect(rc, p.windowBg)
	c.fillRect(box(0, 0, 160, 600), p.navBg)
	c.fillRoundRect(box(6, 10, 154, 46), ui.DpiX(controlRadius), p.navSelected)
	c.fillRoundRect(box(6, 20, 9, 36), ui.DpiX(2), p.navIndicator)
	c.glyph(glyphGlobe, box(16, 10, 40, 46), ui.DpiY(16), p.text)
	c.text("Connessione", box(46, 10, 154, 46), fontBody, p.text, 0)
	c.glyph(glyphSync, box(16, 50, 40, 86), ui.DpiY(16), p.text)
	c.text("Aggiornamenti", box(46, 50, 154, 86), fontBody, p.text, 0)

	c.text("Connessione", box(180, 10, 700, 44), fontSubtitle, p.text, 0)

	// InfoBar.
	info := box(180, 50, 700, 104)
	c.fillRoundRect(info, ui.DpiX(cardRadius), p.infoBg)
	c.strokeRoundRect(info, ui.DpiX(cardRadius), p.infoStroke, 1)
	c.glyph(glyphInfo, box(190, 58, 210, 78), ui.DpiY(16), p.infoIcon)
	c.text("Configurazione remota in vigore: revisione 18", box(218, 58, 690, 78), fontBodyStrong, p.text, 0)
	c.text("Cache, scaricata il 16/10/2026 18:47.", box(218, 78, 690, 96), fontCaption, p.textSecondary, 0)

	// Card with three rows: label states and input frame states.
	card := box(180, 114, 700, 312)
	paintCard(c, card, []int{int(px(180)), int(px(246))})
	rows := []struct {
		title, sub string
		enabled    bool
		focused    bool
		value      string
	}{
		{"Sede (domain controller)", "Rilevata automaticamente", true, false, "Nessuna sede mappata"},
		{"Intervallo di controllo", "Minuti tra due controlli", true, true, "60"},
		{"Rilevamento DC", "", false, false, "6"},
	}
	for i, r := range rows {
		top := 114 + i*66
		paintRowLabel(c, box(196, top, 460, top+66), r.title, r.sub, r.enabled)
		frame := box(470, top+17, 684, top+49)
		paintInputFrame(c, frame, r.focused, r.enabled)
		col := p.text
		if !r.enabled {
			col = p.textDisabled
		}
		c.text(r.value, box(470+inputInset+4, top+17, 684-inputInset, top+49), fontBody, col, 0)
	}

	// Toggles, badge, buttons.
	c.fillRoundRect(box(180, 326, 220, 346), ui.DpiX(10), p.accent)
	c.fillEllipse(box(204, 330, 216, 342), p.toggleOnKnob)
	c.strokeRoundRect(box(232, 326, 272, 346), ui.DpiX(10), p.toggleOffStroke, 1)
	c.fillEllipse(box(237, 331, 247, 341), p.toggleOffKnob)
	badge := box(286, 324, 376, 348)
	c.fillRoundRect(badge, ui.DpiX(controlRadius), p.badgeBg)
	c.glyph(glyphLock, box(290, 324, 306, 348), ui.DpiY(12), p.badgeText)
	c.text("Dal server", box(308, 324, 376, 348), fontCaption, p.badgeText, 0)
	btn := box(390, 320, 500, 352)
	c.fillRoundRect(btn, ui.DpiX(controlRadius), p.buttonBg)
	c.strokeRoundRect(btn, ui.DpiX(controlRadius), p.buttonStroke, 1)
	c.text("Annulla", btn, fontBody, p.text, co.DT_CENTER|co.DT_VCENTER|co.DT_SINGLELINE)
	acc := box(510, 320, 620, 352)
	c.fillRoundRect(acc, ui.DpiX(controlRadius), p.accent)
	c.glyph(glyphShield, box(526, 320, 546, 352), ui.DpiY(14), p.textOnAccent)
	c.text("Salva", box(546, 320, 620, 352), fontBody, p.textOnAccent, 0)

	// Glyphs at two sizes, font roles, lines and ellipses.
	glyphs := []rune{glyphGlobe, glyphSync, glyphServer, glyphColor, glyphInfo, glyphWarning, glyphLock, glyphShield, glyphChevronDown}
	for i, g := range glyphs {
		c.glyph(g, box(180+i*32, 366, 212+i*32, 394), ui.DpiY(16), p.text)
		c.glyph(g, box(180+i*32, 396, 212+i*32, 428), ui.DpiY(20), p.accent)
	}
	c.text("Body 14 - Il servizio è in esecuzione", box(180, 436, 700, 456), fontBody, p.text, 0)
	c.text("Body strong 14 - Configurazione remota", box(180, 456, 700, 476), fontBodyStrong, p.text, 0)
	c.text("Caption 12 - Salvare richiede i privilegi di amministratore", box(180, 476, 700, 494), fontCaption, p.textSecondary, 0)
	c.text("Subtitle 20 - Aggiornamenti", box(180, 494, 700, 524), fontSubtitle, p.text, 0)
	c.line(int(px(180)), int(px(534)), int(px(400)), int(px(534)), p.divider, 1)
	c.line(int(px(180)), int(px(540)), int(px(400)), int(px(560)), p.accent, 1)
	c.strokeEllipse(box(420, 532, 448, 560), p.text, 1)
	c.fillEllipse(box(456, 532, 484, 560), p.accent)
	c.strokeRoundRect(box(500, 532, 600, 560), ui.DpiX(cardRadius), p.focusStroke, 2)
}
