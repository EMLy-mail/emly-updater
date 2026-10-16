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
	"emlyupdater/internal/crash"
	"emlyupdater/internal/version"
)

// settingsBarTimer is the settings window's loadingBar timer ID.
const settingsBarTimer = 1

// Channel combo entries, in order. Index 0 is "no override".
var channelValues = []string{"", "stable", "beta"}

// Primary source combo entries, in order.
var primaryValues = []string{config.SourceInternal, config.SourceExternal}

// settingsForm is the settings window's content: one control per editable
// config.ini key (config.EditableKeys), on pages picked from the navigation
// pane (layout in settings_layout.go), plus the Prodotti page (products.go).
//
// Keys the remote configuration document overrides (governedKeys) are shown
// with the document's effective value, disabled and marked with a lock
// badge while a document is in force: editing them would change nothing
// until remoteConfig is turned off. Switching "Configurazione remota" off
// re-enables them with config.ini's values, since that is what the service
// will use once the edit is saved.
type settingsForm struct {
	a    *app
	snap snapshot // last load; snap.cfg nil while loading or on error

	nav                 *navList
	info                *infoBar
	view                *scrollView
	site, servers       *ui.Edit // read-only: what the service resolved
	poll                *ui.Edit
	channel, primary    *fluentCombo
	remote              *toggleSwitch
	dcAttempts, dcDelay *ui.Edit
	selfUpdate, crit    *toggleSwitch
	critSeconds         *ui.Edit
	ipc, cert, progress *toggleSwitch
	wsDelay             *toggleSwitch
	wsDelaySeconds      *ui.Edit
	theme               *fluentCombo
	badges              []*lockBadge // one per governed row
	products            *productsTab
	btnSave, btnCancel  *fluentButton
	saving              bool
	bar                 *loadingBar // while loading or saving
}

// The pages, in navigation order: newSettingsForm builds them in this order.
const (
	pageConnection = iota
	pageUpdates
	pageProducts
	pageService
	pageLook
)

// settingsNote is the footer's reminder of what saving implies.
const settingsNote = "Salvare richiede i privilegi di amministratore e riavvia il servizio."

func newSettingsForm(a *app) *settingsForm {
	f := &settingsForm{a: a}
	w := a.wnd

	w.On().WmEraseBkgnd(func(_ ui.WmEraseBkgnd) int { return 1 }) // over installTheme's
	w.On().WmPaint(func() {
		paintBuffered(w.Hwnd(), func(c *canvas, rc win.RECT) { paintSettingsChrome(c, rc, settingsNote) })
	})

	f.nav = newNavList(w, 8, 8, navPaneW-16, 5*40, []navItem{
		{glyphGlobe, "Connessione"},
		{glyphSync, "Aggiornamenti"},
		{glyphPackage, "Prodotti"},
		{glyphServer, "Servizio"},
		{glyphColor, "Aspetto"},
	})
	f.info = newInfoBar(w, contentX, infoBarY, contentW, infoBarH)
	f.bar = newLoadingBar(w, settingsBarTimer, contentX, barY, contentW, barH)
	f.view = newScrollView(w)
	f.buildConnection()
	f.buildUpdates()
	f.buildProducts()
	f.buildService()
	f.buildLook()
	f.nav.OnSelect(func(i int) {
		f.view.show(i)
		f.pageShown(i)
	})

	// Footer buttons, right-aligned on the band's centre line, created left
	// to right for the tab order. "Controlla ora" is in the tray menu only.
	const saveW, cancelW = 90, 90
	by := settingsClientH - footerH + (footerH-32)/2
	x := settingsClientW - 24 - saveW - cancelW - ctlGap
	f.btnCancel = newFluentButton(w, "Annulla", 0, false, x, by, cancelW)
	x += cancelW + ctlGap
	f.btnSave = newFluentButton(w, "Salva", 0, true, x, by, saveW)

	f.remote.OnChange(func() { f.remoteToggled() })
	f.crit.OnChange(func() { f.critToggled() })
	f.wsDelay.OnChange(func() { f.wsDelayToggled() })
	f.theme.OnChange(func() { f.previewTheme(f.theme.SelectedIndex()) })
	f.btnSave.OnClick(func() { f.save() })
	f.btnCancel.OnClick(func() { f.cancel() })
	for _, b := range f.badges {
		b.OnClick(func() { f.explainLock() })
	}
	return f
}

// buildConnection is the "Connessione" page: where the service gets its
// manifests from.
func (f *settingsForm) buildConnection() {
	p := newSettingsPage(f.view, "Connessione", true)

	// Site and servers as read-only boxes: the URL can be selected and copied.
	r := p.row("Sede (domain controller)", "Rilevata automaticamente", rowMinHeight)
	xs, _ := r.place(300)
	f.site = r.input(xs[0], 300, co.ES_READONLY|co.ES_AUTOHSCROLL)

	const listH = 72   // three URLs
	const listTop = 62 // under the label block, which is centred in the row's first rowMinHeight
	r = p.row("Server, in ordine di prova", "Un indirizzo per riga", listTop+listH+16)
	f.servers = p.input(pageCardX+rowPadX, r.top+listTop, contentW-2*rowPadX, listH,
		co.ES_READONLY|co.ES_MULTILINE|co.ES_AUTOVSCROLL, 0)

	r = p.row("Rilevamento DC", "Tentativi e attesa (s)", rowMinHeight)
	xs, bx := r.place(64, 64)
	f.badges = append(f.badges, r.badge(bx))
	f.dcAttempts = r.input(xs[0], 64, co.ES_AUTOHSCROLL|co.ES_NUMBER|co.ES_RIGHT)
	f.dcDelay = r.input(xs[1], 64, co.ES_AUTOHSCROLL|co.ES_NUMBER|co.ES_RIGHT)

	r = p.row("Configurazione remota attiva", "Scarica le impostazioni da /v2/config", rowMinHeight)
	xs, _ = r.place(toggleW)
	f.remote = r.toggle(xs[0])

	r = p.row("Sorgente primaria del manifest", "Da dove leggere gli aggiornamenti", rowMinHeight)
	xs, bx = r.place(200)
	f.badges = append(f.badges, r.badge(bx))
	f.primary = r.combo(xs[0], 200, "internal (server interno)", "external (API pubblica)")
	p.finish()
}

// buildUpdates is the "Aggiornamenti" page: when and what the agent updates.
func (f *settingsForm) buildUpdates() {
	p := newSettingsPage(f.view, "Aggiornamenti", false)

	r := p.row("Intervallo di controllo", "Minuti tra un controllo e l'altro", rowMinHeight)
	xs, bx := r.place(80)
	f.badges = append(f.badges, r.badge(bx))
	f.poll = r.input(xs[0], 80, co.ES_AUTOHSCROLL|co.ES_NUMBER|co.ES_RIGHT)

	r = p.row("Canale di rilascio (override)", "Sostituisce quello di EMLy", rowMinHeight)
	xs, bx = r.place(240)
	f.badges = append(f.badges, r.badge(bx))
	f.channel = r.combo(xs[0], 240, "Nessuno (segue il canale di EMLy)", "stable", "beta")

	r = p.row("Aggiornamento automatico dell'agente", "Installa da sé le nuove versioni (self-update)", rowMinHeight)
	xs, bx = r.place(toggleW)
	f.badges = append(f.badges, r.badge(bx))
	f.selfUpdate = r.toggle(xs[0])

	r = p.row("Avvisi per gli aggiornamenti critici", "Attesa in secondi prima di installare", rowMinHeight)
	xs, bx = r.place(64, toggleW)
	f.badges = append(f.badges, r.badge(bx))
	f.critSeconds = r.input(xs[0], 64, co.ES_AUTOHSCROLL|co.ES_NUMBER|co.ES_RIGHT)
	f.crit = r.toggle(xs[1])

	r = p.row("Finestra di avanzamento", "Mostrata durante gli aggiornamenti", rowMinHeight)
	xs, _ = r.place(toggleW)
	f.progress = r.toggle(xs[0])
	p.finish()
}

// buildService is the "Servizio" page: what else the service runs.
func (f *settingsForm) buildService() {
	p := newSettingsPage(f.view, "Servizio", false)

	r := p.row("Server IPC per EMLy", "Named pipe per dialogare con EMLy", rowMinHeight)
	xs, _ := r.place(toggleW)
	f.ipc = r.toggle(xs[0])

	r = p.row("Certificato di code-signing 3gIT", "Installato nell'archivio della macchina", rowMinHeight)
	xs, bx := r.place(toggleW)
	f.badges = append(f.badges, r.badge(bx))
	f.cert = r.toggle(xs[0])

	r = p.row("Ritardo casuale del canale WS", "Attesa massima in secondi all'avvio", rowMinHeight)
	xs, _ = r.place(64, toggleW)
	f.wsDelaySeconds = r.input(xs[0], 64, co.ES_AUTOHSCROLL|co.ES_NUMBER|co.ES_RIGHT)
	f.wsDelay = r.toggle(xs[1])
	p.finish()
}

// buildLook is the "Aspetto" page: the tray's own, per-user preferences.
func (f *settingsForm) buildLook() {
	p := newSettingsPage(f.view, "Aspetto", false)
	r := p.row("Tema", "Chiaro, scuro o come Windows", rowMinHeight)
	xs, _ := r.place(200)
	f.theme = r.combo(xs[0], 200, "Sistema", "Chiaro", "Scuro") // themeSystem, themeLight, themeDark
	p.finish()
}

// showPage switches to page i as if picked in the navigation pane. With the
// window hidden it only selects the page: showSettings' reload runs
// pageShown once the window is filled.
func (f *settingsForm) showPage(i int) {
	f.nav.Select(i)
	f.view.show(i)
	if f.a.wnd.Hwnd().IsWindowVisible() {
		f.pageShown(i)
	}
}

// pageShown runs when page i becomes the visible one: the Prodotti page
// loads its rows the first time after each reload.
func (f *settingsForm) pageShown(i int) {
	if i == pageProducts && !f.products.loaded {
		f.products.load()
	}
}

// previewTheme applies the theme combo's mode at once, without saving it.
func (f *settingsForm) previewTheme(mode int) {
	themeMode = mode
	applyTheme(f.a.wnd)
	f.a.themeIcons()
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
		f.a.themeIcons()
	}
	f.a.wnd.Hwnd().ShowWindow(co.SW_HIDE)
}

// governedKeys are the config.ini keys a remote document overrides
// (policy.DefaultsFromConfig/FromLegacy are where they turn into the
// document's defaults). remoteConfig.enabled, ipc.enabled,
// progressWindow.enabled and the clientWs.startupDelay* keys have no
// counterpart in the document.
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

// reload re-reads everything off the UI thread and refills the form, then
// calls done (if any) on the UI thread.
func (f *settingsForm) reload(done func()) {
	f.snap = snapshot{}
	f.products.loaded = false
	f.theme.SelectIndex(themeMode)
	f.setAllEnabled(false)
	f.info.Set(severityInfo, "Caricamento della configurazione…", "")
	f.bar.Start()
	go func() {
		defer crash.Guard(nil)
		snap := f.a.be.load(f.a.ctx)
		f.a.wnd.UiThread(func() {
			f.bar.Stop()
			f.fill(snap)
			f.pageShown(f.view.cur)
			if done != nil {
				done()
			}
		})
	}()
}

func (f *settingsForm) fill(snap snapshot) {
	f.snap = snap
	if snap.err != nil {
		f.info.Set(severityWarning, "Impossibile leggere config.ini", snap.err.Error())
		f.btnCancel.Enable(true)
		return
	}
	f.setAllEnabled(true)
	f.showStatus()
	f.showSource()
	f.showValues()
}

// showStatus puts where the settings come from in the InfoBar.
func (f *settingsForm) showStatus() {
	f.info.Set(f.status())
}

func (f *settingsForm) status() (sev infoSeverity, title, body string) {
	p := f.snap.policy
	switch {
	case p.Governed:
		title = fmt.Sprintf("Versione configurazione remota: revisione %d", p.Revision)
		body = capitalize(p.Source)
		if !p.FetchedAt.IsZero() {
			body += ", scaricata il " + p.FetchedAt.Local().Format("02/01/2006 15:04")
		}
		body += "."
	case f.snap.cfg.RemoteConfigEnabled:
		title = "Nessuna configurazione remota ricevuta"
		body = "Uso config.ini."
	default:
		title = "Configurazione remota disattivata"
		body = "Uso config.ini."
	}
	if p.Paused {
		return severityWarning, title, "Aggiornamenti SOSPESI dalla configurazione remota. " + body
	}
	return severityInfo, title, body
}

// capitalize upper-cases s's first letter: policy sources are lower case.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// governed reports whether the document overrides the governed keys given
// the form's current remoteConfig switch.
func (f *settingsForm) governed() bool {
	return f.snap.policy.Governed && f.remote.IsOn()
}

// showValues fills every control: the document's values for governed keys
// when a document is in force, config.ini's otherwise.
func (f *settingsForm) showValues() {
	cfg := f.snap.cfg
	f.remote.SetOn(cfg.RemoteConfigEnabled)
	f.ipc.SetOn(cfg.IPCEnabled)
	f.progress.SetOn(cfg.ProgressWindowEnabled)
	f.primary.SelectIndex(indexOf(primaryValues, cfg.Primary))
	f.wsDelay.SetOn(cfg.ClientWSStartupDelayEnabled)
	f.wsDelaySeconds.SetText(strconv.Itoa(int(cfg.ClientWSStartupDelayMax.Seconds())))
	f.wsDelayToggled()
	f.showGoverned()
}

// showSource fills the read-only site and server boxes.
func (f *settingsForm) showSource() {
	p := f.snap.policy
	site := p.Site
	if site == "" {
		site = "N/A (fuori sede o DC non riconosciuto)"
	}
	f.site.SetText(site)
	servers := "nessuno"
	if len(p.Servers) > 0 {
		servers = strings.Join(p.Servers, "\r\n")
	}
	f.servers.SetText(servers)
}

// wsDelayToggled greys the seconds out while the delay itself is off.
func (f *settingsForm) wsDelayToggled() {
	f.wsDelaySeconds.Hwnd().EnableWindow(f.wsDelay.IsOn())
	f.view.invalidate()
}

// remoteToggled runs when "Configurazione remota" is switched on or off:
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
		f.selfUpdate.SetOn(u.SelfUpdate.Enabled)
		f.crit.SetOn(u.CriticalWarning.Enabled)
		f.critSeconds.SetText(strconv.Itoa(u.CriticalWarning.Seconds))
		f.cert.SetOn(u.InstallCertificate.Enabled)
	} else {
		f.poll.SetText(strconv.Itoa(int(cfg.PollInterval.Minutes())))
		f.channel.SelectIndex(indexOf(channelValues, cfg.ChannelOverride))
		f.dcAttempts.SetText(strconv.Itoa(cfg.DCLookupRetryAttempts))
		f.dcDelay.SetText(strconv.Itoa(int(cfg.DCLookupRetryDelay.Seconds())))
		f.selfUpdate.SetOn(cfg.SelfUpdateEnabled)
		f.crit.SetOn(cfg.CriticalWarningEnabled)
		f.critSeconds.SetText(strconv.Itoa(cfg.CriticalWarningSeconds))
		f.cert.SetOn(cfg.CertificateEnabled)
	}
	f.enableGoverned()
}

// explainLock answers a click on a lock badge: why the field next to it
// cannot be edited, and how to edit it anyway.
func (f *settingsForm) explainLock() {
	msg := fmt.Sprintf("Questo valore è impostato dalla configurazione remota (revisione %d) "+
		"e non si può modificare da qui.\n\n"+
		"Per usare il valore di config.ini, disattiva \"Configurazione remota attiva\" "+
		"nella pagina Connessione.", f.snap.policy.Revision)
	f.a.wnd.Hwnd().MessageBox(msg, version.ProductName, co.MB_ICONINFORMATION)
}

// enableGoverned disables the governed keys, and shows their lock badges,
// while a document overrides them.
func (f *settingsForm) enableGoverned() {
	gov := f.governed()
	for _, h := range []win.HWND{f.poll.Hwnd(), f.channel.Hwnd(), f.primary.Hwnd(), f.dcAttempts.Hwnd(),
		f.dcDelay.Hwnd()} {
		h.EnableWindow(!gov)
	}
	for _, t := range []*toggleSwitch{f.selfUpdate, f.crit, f.cert} {
		t.Enable(!gov)
	}
	for _, b := range f.badges {
		b.Show(gov)
	}
	f.critToggled()
}

// critToggled greys the seconds out while the warning itself is off.
func (f *settingsForm) critToggled() {
	f.critSeconds.Hwnd().EnableWindow(!f.governed() && f.crit.IsOn())
	f.view.invalidate()
}

func (f *settingsForm) setAllEnabled(on bool) {
	for _, h := range []win.HWND{f.poll.Hwnd(), f.channel.Hwnd(), f.primary.Hwnd(),
		f.dcAttempts.Hwnd(), f.dcDelay.Hwnd(), f.critSeconds.Hwnd(), f.wsDelaySeconds.Hwnd()} {
		h.EnableWindow(on)
	}
	for _, t := range []*toggleSwitch{f.remote, f.selfUpdate, f.crit, f.ipc, f.cert, f.progress, f.wsDelay} {
		t.Enable(on)
	}
	f.btnSave.Enable(on)
	f.btnCancel.Enable(on)
	f.view.invalidate()
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
	bool_ := func(t *toggleSwitch) string { return strconv.FormatBool(t.IsOn()) }

	set("remoteConfig.enabled", bool_(f.remote), strconv.FormatBool(cfg.RemoteConfigEnabled))
	set("ipc.enabled", bool_(f.ipc), strconv.FormatBool(cfg.IPCEnabled))
	set("progressWindow.enabled", bool_(f.progress), strconv.FormatBool(cfg.ProgressWindowEnabled))
	set("clientWs.startupDelayEnabled", bool_(f.wsDelay), strconv.FormatBool(cfg.ClientWSStartupDelayEnabled))
	wsSecs, err := num(f.wsDelaySeconds, "Ritardo WS")
	if err != nil {
		return nil, err
	}
	set("clientWs.startupDelayMaxSeconds", wsSecs, strconv.Itoa(int(cfg.ClientWSStartupDelayMax.Seconds())))
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
	f.info.Set(severityInfo, "Salvataggio in corso", "Confermare la richiesta di amministratore…")
	go func() {
		defer crash.Guard(nil)
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
		f.reload(nil)
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
	f.wsDelayToggled()
	f.showStatus()
}

func indexOf(list []string, v string) int {
	for i, s := range list {
		if strings.EqualFold(s, v) {
			return i
		}
	}
	return 0
}
