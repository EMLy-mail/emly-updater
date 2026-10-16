//go:build windows

package tray

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
	"golang.org/x/sys/windows"
)

// TestFluentControlsLive shows every control in every state it can be put
// in without a mouse, on the backdrops it is meant for (nav pane, content,
// card, footer), once per palette, and saves a screenshot of each to
// $EMLY_FLUENT_SHOTS (default: shots under the temp dir). It needs a desktop,
// so it is gated: EMLY_FLUENT_TEST=1.
func TestFluentControlsLive(t *testing.T) {
	if os.Getenv("EMLY_FLUENT_TEST") != "1" {
		t.Skip("set EMLY_FLUENT_TEST=1 to run (needs a desktop)")
	}
	dir := os.Getenv("EMLY_FLUENT_SHOTS")
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "emly-fluent-shots")
	}
	saved := themeMode
	defer func() { themeMode = saved }()
	for _, tc := range []struct {
		name string
		mode int
	}{{"dark", themeDark}, {"light", themeLight}} {
		themeMode = tc.mode
		path := filepath.Join(dir, "controls_"+tc.name+".png")
		if err := showControls(path); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		t.Logf("saved %s", path)
	}
}

// showControls runs one window to completion and captures it at path.
func showControls(path string) error {
	const shotTimer = 1
	w := ui.NewMain(ui.OptsMain().
		Title("Fluent controls").
		Size(ui.Dpi(720, 440)).
		Style(co.WS_CAPTION | co.WS_SYSMENU | co.WS_CLIPCHILDREN))

	// Regions, 96-DPI units: nav pane | content (InfoBars, then a card) |
	// footer across the bottom.
	const navW, footerY, cardX, cardY, cardW, cardH = 200, 360, 220, 160, 480, 180
	nav := newNavList(w, 4, 8, navW-8, 200, []navItem{
		{glyphGlobe, "Connessione"}, {glyphSync, "Aggiornamenti"},
		{glyphServer, "Servizio"}, {glyphColor, "Aspetto"},
	})
	info := newInfoBar(w, cardX, 12, cardW, 64)
	warn := newInfoBar(w, cardX, 84, cardW, 64)
	on := newToggle(w, cardX+16, cardY+20)
	off := newToggle(w, cardX+76, cardY+20)
	onDis := newToggle(w, cardX+136, cardY+20)
	offDis := newToggle(w, cardX+196, cardY+20)
	badge := newLockBadge(w, cardX+16, cardY+70)
	cardBtn := newFluentButton(w, "Su card", 0, false, cardX+16, cardY+120, 100)
	cardBtn.setBackdrop(func() argb { return fp().cardBg })
	check := newFluentButton(w, "Controlla ora", 0, false, 16, footerY+20, 120)
	dis := newFluentButton(w, "Disattivato", 0, false, 146, footerY+20, 110)
	cancel := newFluentButton(w, "Annulla", 0, false, 480, footerY+20, 100)
	save := newFluentButton(w, "Salva", glyphShield, true, 590, footerY+20, 110)
	accDis := newFluentButton(w, "Salva", glyphShield, true, 370, footerY+20, 100)

	w.On().WmEraseBkgnd(func(ui.WmEraseBkgnd) int { return 1 })
	w.On().WmPaint(func() {
		paintBuffered(w.Hwnd(), func(c *canvas, rc win.RECT) {
			p := fp()
			c.fillRect(rc, p.windowBg)
			c.fillRect(win.RECT{Right: int32(ui.DpiX(navW)), Bottom: int32(ui.DpiY(footerY))}, p.navBg)
			card := win.RECT{Left: int32(ui.DpiX(cardX)), Top: int32(ui.DpiY(cardY)),
				Right: int32(ui.DpiX(cardX + cardW)), Bottom: int32(ui.DpiY(cardY + cardH))}
			paintCard(c, card, []int{int(card.Top) + ui.DpiY(60)})
			footer := win.RECT{Top: int32(ui.DpiY(footerY)), Right: rc.Right, Bottom: rc.Bottom}
			c.fillRect(footer, p.footerBg)
			c.line(0, int(footer.Top), int(rc.Right), int(footer.Top), p.footerStroke, 1)
		})
	})

	var shotErr error
	w.On().WmCreate(func(ui.WmCreate) int {
		applyTheme(w)
		nav.Select(1)
		info.Set(severityInfo, "Configurazione remota in vigore: revisione 18",
			"Cache, scaricata il 16/10/2026 18:47. I campi con il lucchetto mostrano il valore remoto.")
		warn.Set(severityWarning, "Aggiornamenti sospesi", "La configurazione remota ha sospeso gli aggiornamenti.")
		on.SetOn(true)
		onDis.SetOn(true)
		onDis.Enable(false)
		offDis.Enable(false)
		badge.Show(true)
		dis.Enable(false)
		accDis.Enable(false)
		// Hover, as if under the mouse (no pointer in a test).
		off.hover = true
		cancel.hover = true
		_ = check
		// Focus ring on "Salva", as after a Tab.
		w.Hwnd().SendMessage(co.WM_CHANGEUISTATE, win.MAKEWPARAM(2 /*UIS_CLEAR*/, uisfHideFocus), 0)
		save.hwnd().SetFocus()
		w.Hwnd().SetTimer(shotTimer, 500)
		return 0
	})
	w.On().WmTimer(shotTimer, func() {
		w.Hwnd().KillTimer(shotTimer)
		shotErr = captureWindowPNG(w.Hwnd(), path)
		w.Hwnd().DestroyWindow()
	})
	w.RunAsMain()
	return shotErr
}

// TestFluentComboLive shows fluentCombo closed in its states (normal, hover,
// disabled, keyboard focus) and then open, the selected item over the box,
// once per palette, saving combo_closed_<theme>.png (PrintWindow) and
// combo_open_<theme>.png (a screen copy: the popup is its own window, with
// a DWM shadow). Gated like the others: EMLY_FLUENT_TEST=1.
func TestFluentComboLive(t *testing.T) {
	if os.Getenv("EMLY_FLUENT_TEST") != "1" {
		t.Skip("set EMLY_FLUENT_TEST=1 to run (needs a desktop)")
	}
	dir := os.Getenv("EMLY_FLUENT_SHOTS")
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "emly-fluent-shots")
	}
	saved := themeMode
	defer func() { themeMode = saved }()
	for _, tc := range []struct {
		name string
		mode int
	}{{"dark", themeDark}, {"light", themeLight}} {
		themeMode = tc.mode
		if err := showCombos(dir, tc.name); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		t.Logf("saved combo_{closed,open}_%s.png", tc.name)
	}
}

func showCombos(dir, name string) error {
	const shotTimer = 1
	w := ui.NewMain(ui.OptsMain().
		Title("Fluent combo").
		Size(ui.Dpi(560, 330)).
		Center(true).
		ExStyle(co.WS_EX_TOPMOST). // the open shot copies the screen
		Style(co.WS_CAPTION | co.WS_SYSMENU | co.WS_CLIPCHILDREN))

	const cardX, cardY, cardW, cardH = 20, 20, 500, 240
	theme := []string{"Scuro", "Chiaro", "Impostazioni predefinite di Windows"}
	normal := newFluentCombo(w, cardX+20, cardY+20, 260, theme)
	hover := newFluentCombo(w, cardX+20, cardY+70, 260, []string{"internal (server interno)", "external (API pubblica)"})
	dis := newFluentCombo(w, cardX+20, cardY+120, 260, []string{"Nessuno (segue il canale di EMLy)", "stable", "beta"})
	focused := newFluentCombo(w, cardX+20, cardY+170, 260, theme)
	toOpen := newFluentCombo(w, cardX+300, cardY+120, 180, theme)

	w.On().WmEraseBkgnd(func(ui.WmEraseBkgnd) int { return 1 })
	w.On().WmPaint(func() {
		paintBuffered(w.Hwnd(), func(c *canvas, rc win.RECT) {
			c.fillRect(rc, fp().windowBg)
			paintCard(c, dpiRect(cardX, cardY, cardW, cardH), nil)
		})
	})

	step := 0
	var shotErr error
	keep := func(err error) {
		if err != nil && shotErr == nil {
			shotErr = err
		}
	}
	w.On().WmCreate(func(ui.WmCreate) int {
		applyTheme(w)
		normal.SelectIndex(2)
		hover.hover = true
		dis.SelectIndex(0)
		dis.Enable(false)
		focused.SelectIndex(1)
		toOpen.SelectIndex(2)
		w.Hwnd().SendMessage(co.WM_CHANGEUISTATE, win.MAKEWPARAM(2 /*UIS_CLEAR*/, uisfHideFocus), 0)
		focused.Hwnd().SetFocus()
		w.Hwnd().SetTimer(shotTimer, 500)
		return 0
	})
	w.On().WmTimer(shotTimer, func() {
		switch step {
		case 0:
			// Again: activation after WM_CREATE moved the focus elsewhere.
			focused.Hwnd().SetFocus()
			focused.Hwnd().UpdateWindow()
			keep(captureWindowPNG(w.Hwnd(), filepath.Join(dir, "combo_closed_"+name+".png")))
			toOpen.Hwnd().SetFocus()
			toOpen.open()
			toOpen.hot = 0 // as if under the mouse
			toOpen.popup.InvalidateRect(nil, false)
		case 1:
			wr, _ := w.Hwnd().GetWindowRect()
			pr, _ := toOpen.popup.GetWindowRect()
			const shadow = 24
			rc := windows.Rect{Left: min(wr.Left, pr.Left) - shadow, Top: min(wr.Top, pr.Top) - shadow,
				Right: max(wr.Right, pr.Right) + shadow, Bottom: max(wr.Bottom, pr.Bottom) + shadow}
			keep(captureScreenPNG(rc, filepath.Join(dir, "combo_open_"+name+".png")))
			toOpen.close(false)
		default:
			w.Hwnd().KillTimer(shotTimer)
			w.Hwnd().DestroyWindow()
		}
		step++
	})
	w.RunAsMain()
	return shotErr
}
