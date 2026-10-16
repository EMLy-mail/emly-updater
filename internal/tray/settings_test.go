//go:build windows

package tray

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"golang.org/x/sys/windows"

	"emlyupdater/internal/config"
	"emlyupdater/internal/service"
)

func TestStatus(t *testing.T) {
	cfg, err := config.Parse(defaultConfigINI(t))
	if err != nil {
		t.Fatal(err)
	}
	f := &settingsForm{snap: snapshot{cfg: cfg, policy: service.PolicyView{
		Governed: true, Source: "cache", Revision: 18,
		FetchedAt: time.Date(2026, 10, 16, 18, 47, 0, 0, time.Local),
	}}}
	sev, title, body := f.status()
	if sev != severityInfo || title != "Versione configurazione remota: revisione 18" ||
		body != "Cache, scaricata il 16/10/2026 18:47." {
		t.Errorf("governed: %v %q %q", sev, title, body)
	}

	f.snap.policy = service.PolicyView{Paused: true}
	cfg.RemoteConfigEnabled = false
	sev, title, _ = f.status()
	if sev != severityWarning || title != "Configurazione remota disattivata" {
		t.Errorf("paused: %v %q", sev, title)
	}
}

func defaultConfigINI(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "config", "config.default.ini"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestSettingsWindowLive builds the real settings window over a fake
// snapshot, governed and not, and saves a screenshot of each page in both
// themes to $EMLY_FLUENT_SHOTS (default: shots under the temp dir). It
// needs a desktop, so it is gated: EMLY_FLUENT_TEST=1.
func TestSettingsWindowLive(t *testing.T) {
	if os.Getenv("EMLY_FLUENT_TEST") != "1" {
		t.Skip("set EMLY_FLUENT_TEST=1 to run (needs a desktop)")
	}
	dir := os.Getenv("EMLY_FLUENT_SHOTS")
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "emly-fluent-shots")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	useVisualStyles(t)
	cfg, err := config.Parse(defaultConfigINI(t))
	if err != nil {
		t.Fatal(err)
	}
	ch := "beta"
	governed := service.PolicyView{
		Governed: true, Source: "cache", Revision: 18,
		FetchedAt: time.Date(2026, 10, 16, 18, 47, 0, 0, time.Local),
		Servers:   []string{"https://api.emly.ffois.it/v2/updates/manifest", "http://10.0.0.5/v2/updates/manifest"},
	}
	governed.Updater.PollIntervalMinutes = 30
	governed.Updater.ChannelOverride = &ch
	governed.Updater.DCLookupRetry.Attempts = 6
	governed.Updater.DCLookupRetry.DelaySeconds = 5
	governed.Updater.SelfUpdate.Enabled = true
	governed.Updater.CriticalWarning.Enabled = true
	governed.Updater.CriticalWarning.Seconds = 60

	saved := themeMode
	defer func() { themeMode = saved }()
	for _, tc := range []struct {
		name string
		mode int
		pv   service.PolicyView
	}{
		{"dark", themeDark, governed},
		{"light", themeLight, governed},
		{"dark_local", themeDark, service.PolicyView{Site: "Sede centrale", Servers: governed.Servers}},
	} {
		themeMode = tc.mode
		if err := showSettingsShots(dir, tc.name, snapshot{cfg: cfg, policy: tc.pv}); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
	t.Logf("saved to %s", dir)
}

// showSettingsShots runs one settings window, captures each page in turn
// (and the first page scrolled to the bottom) and closes it.
func showSettingsShots(dir, name string, snap snapshot) error {
	const shotTimer = 99
	a := &app{be: newBackend(), ctx: context.Background()}
	a.wnd = ui.NewMain(ui.OptsMain().
		Title("Impostazioni (test)").
		Size(ui.Dpi(settingsClientW, settingsClientH)).
		Style(co.WS_CAPTION | co.WS_SYSMENU | co.WS_MINIMIZEBOX | co.WS_CLIPCHILDREN | co.WS_BORDER))
	installTheme(a.wnd)
	a.form = newSettingsForm(a)
	f := a.form

	pages := []string{"connessione", "aggiornamenti", "prodotti", "servizio", "aspetto"}
	step := 0
	var shotErr error
	a.wnd.On().WmCreate(func(ui.WmCreate) int {
		applyTheme(a.wnd)
		f.theme.SelectIndex(themeMode)
		f.fill(snap)
		hideFocusCues(a.wnd.Hwnd()) // as showSettings does
		a.wnd.Hwnd().SetTimer(shotTimer, 400)
		return 0
	})
	a.wnd.On().WmTimer(shotTimer, func() {
		var file string
		switch {
		case step < len(pages):
			file = fmt.Sprintf("settings_%s_%s.png", pages[step], name)
		case step == len(pages):
			file = fmt.Sprintf("settings_%s_%s_scrolled.png", pages[0], name)
		}
		if file != "" {
			if err := captureWindowPNG(a.wnd.Hwnd(), filepath.Join(dir, file)); err != nil && shotErr == nil {
				shotErr = err
			}
		}
		step++
		switch {
		case step < len(pages):
			f.nav.Select(step)
			f.view.show(step)
			if step == pageProducts {
				fakeProducts(f.products)
			}
		case step == len(pages):
			f.nav.Select(0)
			f.view.show(0)
			f.view.scrollTo(1 << 30)
		default:
			a.wnd.Hwnd().KillTimer(shotTimer)
			a.wnd.Hwnd().DestroyWindow()
		}
	})
	a.wnd.RunAsMain()
	return shotErr
}

// fakeProducts fills the Prodotti page as a load and three checks would:
// one result, one check in flight, one error.
func fakeProducts(t *productsTab) {
	t.loaded = true
	t.list.setRows([]productEntry{
		{slug: agentSlug, name: "Aryx Agent", sub: productSub("1.8.2", "", true)},
		{slug: "emly", name: "EMLy", sub: productSub("3.5.0", "3.6.0", true)},
		{slug: "viewer", name: "Visualizzatore", sub: productSub("non installato", "", false)},
		{slug: "sign", name: "Firma digitale", sub: productSub("sconosciuto", "", true)},
	})
	t.list.setOffered(0, "Server: 1.8.2", false)
	t.list.setOffered(1, "", true)
	t.list.setOffered(2, "errore: manifest unavailable (503 Service Unavailable)", false)
	t.setStatus("3 prodotti. \"Verifica\" non scarica nulla.")
}

// actCtx is ACTCTXW.
type actCtx struct {
	cbSize                 uint32
	dwFlags                uint32
	lpSource               *uint16
	wProcessorArchitecture uint16
	wLangID                uint16
	lpAssemblyDirectory    *uint16
	lpResourceName         *uint16
	lpApplicationName      *uint16
	hModule                uintptr
}

// useVisualStyles activates app.manifest on this thread: the test binary
// has no manifest of its own, so without it the native controls (the edits'
// scroll bars, the viewport's) would come out in the classic, pre-XP look
// and the screenshots would not show what the real exe draws.
func useVisualStyles(t *testing.T) {
	t.Helper()
	src, _ := windows.UTF16PtrFromString(filepath.Join("..", "..", "app.manifest"))
	ctx := actCtx{lpSource: src}
	ctx.cbSize = uint32(unsafe.Sizeof(ctx))
	h, _, err := kernel32.NewProc("CreateActCtxW").Call(uintptr(unsafe.Pointer(&ctx)))
	if h == uintptr(windows.InvalidHandle) {
		t.Fatalf("CreateActCtx: %v", err)
	}
	var cookie uintptr
	if r, _, err := kernel32.NewProc("ActivateActCtx").Call(h, uintptr(unsafe.Pointer(&cookie))); r == 0 {
		t.Fatalf("ActivateActCtx: %v", err)
	}
}
