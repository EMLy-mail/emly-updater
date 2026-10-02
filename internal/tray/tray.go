//go:build windows

// Package tray is AryxD Agent's notification-area icon, run in each user's
// session as `EMLyUpdater.exe tray`: a settings window over config.ini, a
// "check now" that wakes the service, and a dry-run manifest check per
// product. It is a client of the service, never a second agent: it installs
// nothing, and it reaches the service only through the SCM (state query and
// service.CheckNowControl) and config.ini, the latter only through the
// elevated `apply-settings` subcommand.
package tray

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"syscall"

	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"
	"github.com/rodrigocfd/windigo/x/cosh"
	"github.com/rodrigocfd/windigo/x/winsh"
	"golang.org/x/sys/windows/svc"

	"emlyupdater/internal/config"
	"emlyupdater/internal/version"
)

const (
	wmTray     = co.WM_USER + 1 // the icon's callback message
	trayIconID = 1

	idMenuStatus   uint16 = 1000
	idMenuSettings uint16 = 1001
	idMenuProducts uint16 = 1002
	idMenuCheckNow uint16 = 1003
	idMenuLogs     uint16 = 1004
	idMenuExit     uint16 = 1005
)

type app struct {
	exe     string
	wnd     *ui.Main // the settings window, hidden until asked for
	nid     winsh.NOTIFYICONDATA
	icon    win.HICON
	bigIcon win.HICON
	be      *backend
	ctx     context.Context
	form    *settingsForm
	// wmTaskbarCreated is broadcast when Explorer restarts: the icon must be
	// added again, or it silently disappears until the next logon.
	wmTaskbarCreated co.WM
}

// Run shows the tray icon until the user picks "Esci" or the session ends.
// A second instance in the same session exits at once.
func Run() error {
	runtime.LockOSThread() // Win32 windows belong to the thread that made them

	release, ok := singleInstance()
	if !ok {
		return nil
	}
	defer release()

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	registerRestart()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &app{exe: exe, be: newBackend(), ctx: ctx}
	a.icon, a.bigIcon = exeIcons(exe)
	a.wmTaskbarCreated, _ = win.RegisterWindowMessage("TaskbarCreated")
	setDarkMenus(isDark()) // the app mode must be set before any window exists

	a.wnd = ui.NewMain(ui.OptsMain().
		Title(version.ProductName + " - Impostazioni").
		Size(ui.Dpi(560, 512)).
		Center(true).
		Style(co.WS_CAPTION | co.WS_SYSMENU | co.WS_MINIMIZEBOX | co.WS_CLIPCHILDREN | co.WS_BORDER).
		CmdShow(co.SW_HIDE))
	a.form = newSettingsForm(a)
	a.events()
	a.wnd.RunAsMain()
	return nil
}

func (a *app) events() {
	installTheme(a.wnd)
	a.wnd.On().Wm(wmSettingChange, func(p ui.Wm) uintptr {
		if isThemeChange(p) {
			applyTheme(a.wnd)
		}
		return 0
	})

	a.wnd.On().WmCreate(func(_ ui.WmCreate) int {
		applyTheme(a.wnd) // the controls exist only once the window does
		if a.bigIcon != 0 {
			a.wnd.Hwnd().SendMessage(co.WM_SETICON, win.WPARAM(co.ICON_SZ_BIG), win.LPARAM(a.bigIcon))
		}
		if a.icon != 0 {
			a.wnd.Hwnd().SendMessage(co.WM_SETICON, win.WPARAM(co.ICON_SZ_SMALL), win.LPARAM(a.icon))
		}
		a.addTrayIcon()
		return 0
	})

	a.wnd.On().Wm(wmTray, func(p ui.Wm) uintptr {
		switch co.WM(p.LParam.LoWord()) {
		case co.WM_LBUTTONDBLCLK:
			a.showSettings()
		case co.WM_RBUTTONUP, co.WM_CONTEXTMENU:
			a.showMenu()
		}
		return 0
	})

	if a.wmTaskbarCreated != 0 {
		a.wnd.On().Wm(a.wmTaskbarCreated, func(_ ui.Wm) uintptr {
			a.addTrayIcon()
			return 0
		})
	}

	// Closing the window only hides it; "Esci" in the menu is the real exit.
	// Like "Annulla", it drops an unsaved theme preview.
	a.wnd.On().Wm(co.WM_CLOSE, func(_ ui.Wm) uintptr {
		a.form.cancel()
		return 0
	})

	// Logoff, shutdown, or the Restart Manager closing the tray so that the
	// agent's setup can replace EMLyUpdater.exe (registerRestart brings it
	// back afterwards). Hiding on WM_CLOSE above would keep the file locked.
	a.wnd.On().Wm(co.WM_QUERYENDSESSION, func(_ ui.Wm) uintptr { return 1 })
	a.wnd.On().Wm(co.WM_ENDSESSION, func(p ui.Wm) uintptr {
		if p.WParam != 0 {
			a.quit()
		}
		return 0
	})
}

func (a *app) quit() {
	_ = winsh.Shell_NotifyIcon(cosh.NIM_DELETE, &a.nid)
	a.wnd.Hwnd().DestroyWindow() // WM_DESTROY ends the message loop
}

func (a *app) addTrayIcon() {
	icon := a.icon
	if icon == 0 {
		icon, _ = win.HINSTANCE(0).LoadIcon(win.IconResIdi(co.IDI_APPLICATION))
	}
	a.nid.SetCbSize()
	a.nid.HWnd = a.wnd.Hwnd()
	a.nid.UID = trayIconID
	a.nid.UFlags = cosh.NIF_MESSAGE | cosh.NIF_ICON | cosh.NIF_TIP
	a.nid.UCallbackMessage = wmTray
	a.nid.HIcon = icon
	a.nid.SetSzTip(version.ProductName + " " + version.Version)
	_ = winsh.Shell_NotifyIcon(cosh.NIM_ADD, &a.nid)
}

// balloon shows a notification from the tray icon.
func (a *app) balloon(title, text string, warn bool) {
	nid := a.nid
	nid.UFlags = cosh.NIF_INFO
	nid.SetSzInfoTitle(title)
	nid.SetSzInfo(text)
	nid.DwInfoFlags = cosh.NIIF_INFO
	if warn {
		nid.DwInfoFlags = cosh.NIIF_WARNING
	}
	_ = winsh.Shell_NotifyIcon(cosh.NIM_MODIFY, &nid)
}

func (a *app) showSettings() {
	h := a.wnd.Hwnd()
	if !h.IsWindowVisible() {
		a.form.reload()
	}
	h.ShowWindow(co.SW_SHOWNORMAL)
	h.SetForegroundWindow()
}

func (a *app) showMenu() {
	hMenu, err := win.CreatePopupMenu()
	if err != nil {
		return
	}
	defer hMenu.DestroyMenu()

	pos := 0
	add := func(id uint16, text string, state co.MFS) {
		var mii win.MENUITEMINFO
		mii.SetCbSize()
		mii.FMask = co.MIIM_ID | co.MIIM_STRING | co.MIIM_FTYPE | co.MIIM_STATE
		mii.FType = co.MFT_STRING
		mii.FState = state
		mii.WId = uint32(id)
		w, _ := syscall.UTF16FromString(text)
		mii.DwTypeData = &w[0]
		_ = hMenu.InsertMenuItemByPos(pos, &mii)
		pos++
	}
	sep := func() {
		var mii win.MENUITEMINFO
		mii.SetCbSize()
		mii.FMask = co.MIIM_FTYPE
		mii.FType = co.MFT_SEPARATOR
		_ = hMenu.InsertMenuItemByPos(pos, &mii)
		pos++
	}

	add(idMenuStatus, fmt.Sprintf("%s %s - servizio %s", version.ProductName, version.Version,
		stateText(serviceState())), co.MFS_DISABLED)
	sep()
	add(idMenuSettings, "Impostazioni…", co.MFS_DEFAULT)
	add(idMenuProducts, "Prodotti e versioni…", 0)
	add(idMenuCheckNow, "Controlla aggiornamenti ora", 0)
	sep()
	add(idMenuLogs, "Apri cartella dei log", 0)
	sep()
	add(idMenuExit, "Esci", 0)

	pt, _ := win.GetCursorPos()
	h := a.wnd.Hwnd()
	h.SetForegroundWindow() // otherwise the menu does not close on an outside click
	cmd, _ := hMenu.TrackPopupMenu(co.TPM_RIGHTBUTTON|co.TPM_RETURNCMD|co.TPM_BOTTOMALIGN,
		int(pt.X), int(pt.Y), h)
	switch uint16(cmd) {
	case idMenuSettings:
		a.showSettings()
	case idMenuProducts:
		showProducts(a)
	case idMenuCheckNow:
		a.checkNow()
	case idMenuLogs:
		openFolder(config.LogsDir())
	case idMenuExit:
		a.quit()
	}
}

// checkNow wakes the service for an immediate cycle (service.RequestCheck):
// it re-reads the configuration and checks the agent and every product,
// downloading and installing what is due - exactly an ordinary cycle.
func (a *app) checkNow() {
	if st, err := serviceState(); err != nil || st != svc.Running {
		a.balloon(version.ProductName, "Il servizio non è in esecuzione ("+stateText(st, err)+"): controllo non avviato.", true)
		return
	}
	if err := requestCheck(); err != nil {
		a.balloon(version.ProductName, "Richiesta al servizio non riuscita: "+err.Error(), true)
		return
	}
	a.balloon(version.ProductName,
		"Controllo aggiornamenti avviato per l'agente e per tutti i prodotti. "+
			"Eventuali aggiornamenti verranno scaricati e installati come di consueto.", false)
}
