//go:build windows

package tray

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"

	"emlyupdater/internal/config"
	"emlyupdater/internal/version"
)

// settingsBarTimer is the settings window's loadingBar timer ID.
const settingsBarTimer = 1

// Channel combo entries, in order. Index 0 is "no override".
var channelValues = []string{"", "stable", "beta"}

// Primary source combo entries, in order.
var primaryValues = []string{config.SourceInternal, config.SourceExternal}

// settingsForm is the settings window's content: one control per editable
// config.ini key (config.EditableKeys).
//
// Keys the remote configuration document overrides (governedKeys) are shown
// with the document's effective value and disabled while a document is in
// force: editing them would change nothing until remoteConfig is turned off.
// Unticking "Configurazione remota" re-enables them with config.ini's values,
// since that is what the service will use once the edit is saved.
type settingsForm struct {
	a    *app
	snap snapshot // last load; snap.cfg nil while loading or on error

	status                *ui.Static
	poll                  *ui.Edit
	channel, primary      *ui.ComboBox
	remote                *checkRow
	dcAttempts, dcDelay   *ui.Edit
	selfUpdate, crit      *checkRow
	critSeconds           *ui.Edit
	ipc, cert, progress   *checkRow
	theme                 *themeRow
	note                  *ui.Static
	btnCheck, btnProducts *ui.Button
	btnSave, btnCancel    *ui.Button
	saving                bool
	bar                   *loadingBar // while loading or saving
}

func newSettingsForm(a *app) *settingsForm {
	f := &settingsForm{a: a}
	w := a.wnd
	const lx, cx = 16, 260 // label column, control column
	y := 12

	f.status = ui.NewStatic(w, ui.OptsStatic().Position(ui.Dpi(lx, y)).Size(ui.Dpi(520, 48)).
		Text("Caricamento della configurazione…"))
	// Under the status line's first row: while it shows, the status is a
	// single line ("Caricamento…", "Salvataggio…").
	f.bar = newLoadingBar(w, settingsBarTimer, lx, y+24, 520, 14)
	y += 58

	label := func(text string) {
		ui.NewStatic(w, ui.OptsStatic().Text(text).Position(ui.Dpi(lx, y+3)).Size(ui.Dpi(cx-lx-8, 18)))
	}
	number := func(x, width int) *ui.Edit {
		return ui.NewEdit(w, ui.OptsEdit().Position(ui.Dpi(x, y)).Width(ui.DpiX(width)).
			CtrlStyle(co.ES_AUTOHSCROLL|co.ES_NUMBER))
	}
	check := func(text string) *checkRow { return newCheckRow(w, text, lx, y, 330) }

	label("Intervallo di controllo (minuti)")
	f.poll = number(cx, 70)
	y += 30

	label("Canale di rilascio (override)")
	f.channel = ui.NewComboBox(w, ui.OptsComboBox().Position(ui.Dpi(cx, y)).Width(ui.DpiX(270)).
		CtrlStyle(co.CBS_DROPDOWNLIST).Texts("Nessuno (segue il canale di EMLy)", "stable", "beta"))
	y += 30

	f.remote = check("Configurazione remota attiva (/v2/config)")
	y += 28

	label("Sorgente primaria del manifest")
	f.primary = ui.NewComboBox(w, ui.OptsComboBox().Position(ui.Dpi(cx, y)).Width(ui.DpiX(270)).
		CtrlStyle(co.CBS_DROPDOWNLIST).Texts("internal (server interno)", "external (API pubblica)"))
	y += 30

	label("Rilevamento DC: tentativi / attesa (s)")
	f.dcAttempts = number(cx, 60)
	f.dcDelay = number(cx+80, 60)
	y += 30

	f.selfUpdate = check("Aggiornamento automatico dell'agente (self-update)")
	y += 28

	f.crit = check("Avvisi per gli aggiornamenti critici, attesa (s):")
	f.critSeconds = number(cx+90, 60)
	y += 30

	f.ipc = check("Server IPC per EMLy (named pipe)")
	y += 28
	f.cert = check("Installa il certificato di code-signing 3gIT")
	y += 28
	f.progress = check("Finestra di avanzamento durante gli aggiornamenti")
	y += 30

	label("Tema")
	f.theme = newThemeRow(w, cx, y, func(mode int) { f.previewTheme(mode) })
	y += 34

	f.note = ui.NewStatic(w, ui.OptsStatic().Position(ui.Dpi(lx, y)).Size(ui.Dpi(520, 66)).Text(
		"Salvare richiede i privilegi di amministratore e riavvia il servizio.\n"+
			"Le modifiche valgono fino al prossimo aggiornamento dell'agente: ogni installazione "+
			"ripristina config.ini dai valori predefiniti (il precedente resta in config.prev.ini)."))
	y += 76

	f.btnCheck = ui.NewButton(w, ui.OptsButton().Text("&Controlla ora").Position(ui.Dpi(lx, y)).Width(ui.DpiX(110)))
	f.btnProducts = ui.NewButton(w, ui.OptsButton().Text("&Prodotti…").Position(ui.Dpi(lx+120, y)).Width(ui.DpiX(100)))
	f.btnSave = ui.NewButton(w, ui.OptsButton().Text("&Salva").Position(ui.Dpi(350, y)).Width(ui.DpiX(85)))
	f.btnCancel = ui.NewButton(w, ui.OptsButton().Text("&Annulla").Position(ui.Dpi(445, y)).Width(ui.DpiX(85)))

	for _, b := range []*ui.Button{f.btnCheck, f.btnProducts, f.btnSave, f.btnCancel} {
		darkButton(b)
	}
	f.remote.OnClick(func() { f.remoteToggled() })
	f.crit.OnClick(func() { f.critToggled() })
	f.btnCheck.On().BnClicked(func() { a.checkNow() })
	f.btnProducts.On().BnClicked(func() { showProducts(a) })
	f.btnSave.On().BnClicked(func() { f.save() })
	f.btnCancel.On().BnClicked(func() { f.cancel() })
	return f
}

// previewTheme applies a theme radio's mode at once, without saving it.
func (f *settingsForm) previewTheme(mode int) {
	themeMode = mode
	applyTheme(f.a.wnd)
}

// saveTheme keeps the previewed theme. It is the user's own preference, in
// HKCU: no UAC prompt, and independent of the config.ini edits.
func (f *settingsForm) saveTheme() {
	if themeMode == loadThemeMode() {
		return
	}
	if err := saveThemeMode(themeMode); err != nil {
		f.a.balloon(version.ProductName, "Impossibile salvare il tema: "+err.Error(), true)
	}
}

// cancel hides the window, dropping an unsaved theme preview.
func (f *settingsForm) cancel() {
	if saved := loadThemeMode(); saved != themeMode {
		themeMode = saved
		applyTheme(f.a.wnd)
	}
	f.a.wnd.Hwnd().ShowWindow(co.SW_HIDE)
}

// governedKeys are the config.ini keys a remote document overrides
// (policy.DefaultsFromConfig/FromLegacy are where they turn into the
// document's defaults). remoteConfig.enabled, ipc.enabled and
// progressWindow.enabled have no counterpart in the document.
var governedKeys = map[string]bool{
	"updater.pollIntervalMinutes":           true,
	"updater.channelOverride":               true,
	"source.primary":                        true,
	"source.dcLookupRetryAttempts":          true,
	"source.dcLookupRetryDelaySeconds":      true,
	"selfUpdate.enabled":                    true,
	"criticalUpdate.criticalWarningEnabled": true,
	"criticalUpdate.criticalWarningSeconds": true,
	"certificate.enabled":                   true,
}

// reload re-reads everything off the UI thread and refills the form.
func (f *settingsForm) reload() {
	f.snap = snapshot{}
	f.theme.Select(themeMode)
	f.setAllEnabled(false)
	f.status.Hwnd().SetWindowText("Caricamento della configurazione…")
	f.bar.Start()
	go func() {
		snap := f.a.be.load(f.a.ctx)
		f.a.wnd.UiThread(func() {
			f.bar.Stop()
			f.fill(snap)
		})
	}()
}

func (f *settingsForm) fill(snap snapshot) {
	f.snap = snap
	if snap.err != nil {
		f.status.Hwnd().SetWindowText("Impossibile leggere config.ini: " + snap.err.Error())
		f.btnCancel.Hwnd().EnableWindow(true)
		return
	}
	f.setAllEnabled(true)
	f.status.Hwnd().SetWindowText(f.statusText())
	f.showValues()
}

func (f *settingsForm) statusText() string {
	p := f.snap.policy
	var b strings.Builder
	if p.Governed {
		fmt.Fprintf(&b, "Configurazione remota in vigore: revisione %d (%s", p.Revision, p.Source)
		if !p.FetchedAt.IsZero() {
			fmt.Fprintf(&b, ", scaricata il %s", p.FetchedAt.Local().Format("02/01/2006 15:04"))
		}
		b.WriteString("). I campi disattivati mostrano il valore remoto.")
	} else if f.snap.cfg.RemoteConfigEnabled {
		b.WriteString("Nessun documento remoto ricevuto finora: vale config.ini.")
	} else {
		b.WriteString("Configurazione remota disattivata: vale config.ini.")
	}
	b.WriteString("\n")
	if p.Site != "" {
		fmt.Fprintf(&b, "Sede: %s - ", p.Site)
	}
	if len(p.Servers) > 0 {
		b.WriteString("server: " + strings.Join(p.Servers, ", "))
	}
	if p.Paused {
		b.WriteString("\nAggiornamenti SOSPESI dalla configurazione remota.")
	}
	return b.String()
}

// governed reports whether the document overrides the governed keys given
// the form's current remoteConfig tick.
func (f *settingsForm) governed() bool {
	return f.snap.policy.Governed && f.remote.IsChecked()
}

// showValues fills every control: the document's values for governed keys
// when a document is in force, config.ini's otherwise.
func (f *settingsForm) showValues() {
	cfg := f.snap.cfg
	f.remote.SetCheck(cfg.RemoteConfigEnabled)
	f.ipc.SetCheck(cfg.IPCEnabled)
	f.progress.SetCheck(cfg.ProgressWindowEnabled)
	f.primary.SelectIndex(indexOf(primaryValues, cfg.Primary))
	f.showGoverned()
}

// remoteToggled runs when "Configurazione remota" is ticked or unticked:
// with a document in force, that switches the governed keys between the
// document's values (disabled) and config.ini's (editable), since config.ini
// is what the service will use once remoteConfig is off.
func (f *settingsForm) remoteToggled() {
	if f.snap.cfg != nil && f.snap.policy.Governed {
		f.showGoverned()
	}
}

// showGoverned fills the governed keys from the right source and enables
// or disables them.
func (f *settingsForm) showGoverned() {
	cfg := f.snap.cfg
	gov := f.governed()
	if gov {
		u := f.snap.policy.Updater
		f.poll.SetText(strconv.Itoa(u.PollIntervalMinutes))
		f.channel.SelectIndex(indexOf(channelValues, u.Channel()))
		f.dcAttempts.SetText(strconv.Itoa(u.DCLookupRetry.Attempts))
		f.dcDelay.SetText(strconv.Itoa(u.DCLookupRetry.DelaySeconds))
		f.selfUpdate.SetCheck(u.SelfUpdate.Enabled)
		f.crit.SetCheck(u.CriticalWarning.Enabled)
		f.critSeconds.SetText(strconv.Itoa(u.CriticalWarning.Seconds))
		f.cert.SetCheck(u.InstallCertificate.Enabled)
	} else {
		f.poll.SetText(strconv.Itoa(int(cfg.PollInterval.Minutes())))
		f.channel.SelectIndex(indexOf(channelValues, cfg.ChannelOverride))
		f.dcAttempts.SetText(strconv.Itoa(cfg.DCLookupRetryAttempts))
		f.dcDelay.SetText(strconv.Itoa(int(cfg.DCLookupRetryDelay.Seconds())))
		f.selfUpdate.SetCheck(cfg.SelfUpdateEnabled)
		f.crit.SetCheck(cfg.CriticalWarningEnabled)
		f.critSeconds.SetText(strconv.Itoa(cfg.CriticalWarningSeconds))
		f.cert.SetCheck(cfg.CertificateEnabled)
	}
	f.enableGoverned()
}

// enableGoverned disables the governed keys while a document overrides them.
func (f *settingsForm) enableGoverned() {
	gov := f.governed()
	for _, h := range []win.HWND{f.poll.Hwnd(), f.channel.Hwnd(), f.primary.Hwnd(), f.dcAttempts.Hwnd(),
		f.dcDelay.Hwnd()} {
		h.EnableWindow(!gov)
	}
	for _, c := range []*checkRow{f.selfUpdate, f.crit, f.cert} {
		c.Enable(!gov)
	}
	f.critToggled()
}

// critToggled greys the seconds out while the warning itself is off.
func (f *settingsForm) critToggled() {
	f.critSeconds.Hwnd().EnableWindow(!f.governed() && f.crit.IsChecked())
}

func (f *settingsForm) setAllEnabled(on bool) {
	for _, h := range []win.HWND{f.poll.Hwnd(), f.channel.Hwnd(), f.primary.Hwnd(),
		f.dcAttempts.Hwnd(), f.dcDelay.Hwnd(), f.critSeconds.Hwnd(), f.btnSave.Hwnd(), f.btnCancel.Hwnd()} {
		h.EnableWindow(on)
	}
	for _, c := range []*checkRow{f.remote, f.selfUpdate, f.crit, f.ipc, f.cert, f.progress} {
		c.Enable(on)
	}
}

// edits compares the form with config.ini and returns the keys that
// changed. Governed keys are skipped while a document overrides them: the
// form shows the document's value there, not an edit.
func (f *settingsForm) edits() ([]config.Edit, error) {
	cfg := f.snap.cfg
	gov := f.governed()
	var out []config.Edit
	set := func(key, value, current string) {
		if gov && governedKeys[key] {
			return
		}
		if value != current {
			out = append(out, config.Edit{Key: key, Value: value})
		}
	}
	num := func(e *ui.Edit, name string) (string, error) {
		s := strings.TrimSpace(e.Text())
		if _, err := strconv.Atoi(s); err != nil {
			e.ShowBalloonTip("Valore non valido", name+": inserire un numero intero.", co.TTI_WARNING)
			e.Focus()
			return "", errors.New("invalid number")
		}
		return s, nil
	}
	bool_ := func(c *checkRow) string { return strconv.FormatBool(c.IsChecked()) }

	set("remoteConfig.enabled", bool_(f.remote), strconv.FormatBool(cfg.RemoteConfigEnabled))
	set("ipc.enabled", bool_(f.ipc), strconv.FormatBool(cfg.IPCEnabled))
	set("progressWindow.enabled", bool_(f.progress), strconv.FormatBool(cfg.ProgressWindowEnabled))
	if gov {
		return out, nil
	}

	poll, err := num(f.poll, "Intervallo")
	if err != nil {
		return nil, err
	}
	attempts, err := num(f.dcAttempts, "Tentativi")
	if err != nil {
		return nil, err
	}
	delay, err := num(f.dcDelay, "Attesa")
	if err != nil {
		return nil, err
	}
	critSecs, err := num(f.critSeconds, "Secondi")
	if err != nil {
		return nil, err
	}
	set("updater.pollIntervalMinutes", poll, strconv.Itoa(int(cfg.PollInterval.Minutes())))
	set("updater.channelOverride", channelValues[max(f.channel.SelectedIndex(), 0)], cfg.ChannelOverride)
	set("source.primary", primaryValues[max(f.primary.SelectedIndex(), 0)], cfg.Primary)
	set("source.dcLookupRetryAttempts", attempts, strconv.Itoa(cfg.DCLookupRetryAttempts))
	set("source.dcLookupRetryDelaySeconds", delay, strconv.Itoa(int(cfg.DCLookupRetryDelay.Seconds())))
	set("selfUpdate.enabled", bool_(f.selfUpdate), strconv.FormatBool(cfg.SelfUpdateEnabled))
	set("criticalUpdate.criticalWarningEnabled", bool_(f.crit), strconv.FormatBool(cfg.CriticalWarningEnabled))
	set("criticalUpdate.criticalWarningSeconds", critSecs, strconv.Itoa(cfg.CriticalWarningSeconds))
	set("certificate.enabled", bool_(f.cert), strconv.FormatBool(cfg.CertificateEnabled))
	return out, nil
}

// save validates the edits exactly as the service will load them, then
// hands them to `EMLyUpdater.exe apply-settings` through the UAC prompt:
// config.ini is writable only by administrators and SYSTEM.
func (f *settingsForm) save() {
	if f.saving {
		return
	}
	f.saveTheme()
	if f.snap.cfg == nil {
		return
	}
	edits, err := f.edits()
	if err != nil {
		return // the balloon tip already says what is wrong
	}
	h := f.a.wnd.Hwnd()
	if len(edits) == 0 {
		h.ShowWindow(co.SW_HIDE)
		return
	}

	// Pre-flight, unelevated: the same check apply-settings makes before
	// writing, so a bad value is reported here instead of after a UAC prompt.
	data, err := os.ReadFile(config.ConfigPath())
	if err == nil {
		var out []byte
		if out, err = config.ApplyEdits(data, edits); err == nil {
			_, err = config.Parse(out)
		}
	}
	if err != nil {
		h.MessageBox("Le impostazioni non sono valide:\n\n"+err.Error(), version.ProductName, co.MB_ICONWARNING)
		return
	}

	result, err := os.CreateTemp("", "aryxd-apply-*.txt")
	if err != nil {
		h.MessageBox(err.Error(), version.ProductName, co.MB_ICONERROR)
		return
	}
	resultPath := result.Name()
	result.Close()

	args := []string{"apply-settings", "--result", resultPath}
	for _, e := range edits {
		args = append(args, e.Key+"="+e.Value)
	}

	f.saving = true
	f.bar.Start()
	f.setAllEnabled(false)
	f.status.Hwnd().SetWindowText("Salvataggio: confermare la richiesta di amministratore…")
	go func() {
		code, runErr := runElevated(h, f.a.exe, args)
		out, _ := os.ReadFile(resultPath)
		os.Remove(resultPath)
		f.a.wnd.UiThread(func() { f.saved(code, runErr, strings.TrimSpace(string(out))) })
	}()
}

func (f *settingsForm) saved(code uint32, runErr error, detail string) {
	f.saving = false
	f.bar.Stop()
	h := f.a.wnd.Hwnd()
	switch {
	case errors.Is(runErr, errElevationCancelled):
		f.unlock() // nothing written: keep the user's edits on screen
		return
	case runErr != nil:
		h.MessageBox("Impossibile avviare il salvataggio:\n\n"+runErr.Error(), version.ProductName, co.MB_ICONERROR)
		f.unlock()
		return
	case code != 0:
		if detail == "" {
			detail = fmt.Sprintf("codice di uscita %d", code)
		}
		h.MessageBox("Salvataggio non riuscito:\n\n"+detail, version.ProductName, co.MB_ICONERROR)
		f.reload()
		return
	}
	f.a.balloon(version.ProductName, "Impostazioni salvate, servizio riavviato.", false)
	h.ShowWindow(co.SW_HIDE)
}

// unlock re-enables the form after a save that wrote nothing, keeping
// whatever the user had typed.
func (f *settingsForm) unlock() {
	f.setAllEnabled(true)
	f.enableGoverned()
	f.status.Hwnd().SetWindowText(f.statusText())
}

func indexOf(list []string, v string) int {
	for i, s := range list {
		if strings.EqualFold(s, v) {
			return i
		}
	}
	return 0
}
