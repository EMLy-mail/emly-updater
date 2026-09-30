package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"emlyupdater/internal/assoc"
	"emlyupdater/internal/notify"
	"emlyupdater/internal/product"
	"emlyupdater/internal/progresswin"
	"emlyupdater/internal/source"
	"emlyupdater/internal/version"
)

// progressUpdateEvery bounds how often a download's progress reaches the
// window: the reader reports every few KB.
const progressUpdateEvery = 250 * time.Millisecond

// progressUI is one update's progress window, opened lazily on the first
// thing worth showing - the first byte of a download the server accepted, or
// the start of a setup - so a cached setup, a download refused with 429 or an
// update waiting for EMLy to be closed never flashes an empty window.
//
// Every method is a no-op on a nil *progressUI, which is what newProgressUI
// returns when [progressWindow] enabled = false: callers never check.
type progressUI struct {
	u        *Updater
	self     bool // the agent's own update rather than EMLy's
	product  string
	version  string
	iconPath string

	win    *notify.ProgressWindow
	tried  bool // opening was attempted (it is never retried within one update)
	lastAt time.Time
	lastPc int
}

// newProgressUI returns the window for updating EMLy (self = false) or this
// agent (self = true) to target, or nil when the window is disabled in
// config.ini.
func (u *Updater) newProgressUI(self bool, target string) *progressUI {
	if !u.Cfg.ProgressWindowEnabled {
		return nil
	}
	p := &progressUI{u: u, self: self, version: target, lastPc: -2,
		iconPath: assoc.ExePath(u.Cfg.EMLyInstallDir, u.Cfg.EMLyExeName),
		product:  "EMLy",
	}
	if self {
		p.product = version.ProductName
		p.iconPath = "" // the helper falls back to its own icon: this binary's
	}
	return p
}

// newProductProgressUI is newProgressUI for any product: EMLy keeps its own
// window, every other product gets its name and its executable's icon.
func (u *Updater) newProductProgressUI(p *product.Product, target string) *progressUI {
	ui := u.newProgressUI(false, target)
	if ui == nil || p.Legacy {
		return ui
	}
	ui.product = p.Name
	ui.iconPath = filepath.Join(p.InstallDir, p.ExeName)
	return ui
}

// download is a source.ProgressFunc.
func (p *progressUI) download(done, total int64) {
	if p == nil {
		return
	}
	pc := progresswin.Indeterminate
	if total > 0 {
		pc = int(done * 100 / total)
	}
	now := time.Now()
	finished := total > 0 && done >= total
	if pc == p.lastPc || (!finished && now.Sub(p.lastAt) < progressUpdateEvery) {
		return
	}
	p.lastAt, p.lastPc = now, pc
	heading, detail := p.downloadText(done, total)
	p.show(heading, detail, pc)
}

// installing switches the window to the setup phase, which has no measurable
// progress: Inno Setup runs /VERYSILENT and reports nothing until it exits.
func (p *progressUI) installing() {
	if p == nil {
		return
	}
	p.lastPc = -2 // a re-download after this starts showing progress again
	heading, detail := p.installText()
	p.show(heading, detail, progresswin.Indeterminate)
}

func (p *progressUI) show(heading, detail string, percent int) {
	if !p.tried {
		p.tried = true
		p.open()
	}
	p.win.Update(heading, detail, percent)
}

func (p *progressUI) open() {
	self, err := os.Executable()
	if err != nil {
		p.u.Log.Warn("progress window skipped: own executable path unknown", "error", err.Error())
		return
	}
	opts := notify.ProgressWindowOptions{SelfExe: self, Title: p.title(), IconPath: p.iconPath}
	if p.self {
		opts.WaitService = Name
		opts.WaitPID = uint32(os.Getpid())
	}
	win, err := notify.OpenProgressWindow(opts)
	switch {
	case err != nil:
		p.u.Log.Warn("progress window could not be opened, the update continues without it",
			"product", p.product, "error", err.Error())
	case win == nil:
		p.u.Log.Debug("progress window skipped: no active user session (console or RDP)", "product", p.product)
	default:
		p.u.Log.Info("progress window opened", "product", p.product, "version", p.version)
	}
	p.win = win
}

// watch returns ctx with the download progress reported to the window.
func (p *progressUI) watch(ctx context.Context) context.Context {
	if p == nil {
		return ctx
	}
	return source.WithProgress(ctx, p.download)
}

// endProgress closes the EMLy update's window at the end of a cycle, however
// it ended.
func (u *Updater) endProgress() {
	u.progress.close()
	u.progress = nil
}

// close closes the window, if one is open. A later show opens a new one.
func (p *progressUI) close() {
	if p == nil {
		return
	}
	p.win.Close()
	p.win, p.tried = nil, false
}

// detach leaves the window up past this process: see
// notify.ProgressWindow.Detach.
func (p *progressUI) detach() {
	if p == nil {
		return
	}
	p.win.Detach()
	p.win, p.tried = nil, false
}

// The window's text is Italian only, whatever EMLy's LANGUAGE says - a
// deliberate choice, unlike the toasts and the critical-update warning.

func (p *progressUI) title() string {
	return p.product + " - Aggiornamento"
}

func (p *progressUI) downloadText(done, total int64) (heading, detail string) {
	heading = fmt.Sprintf("Download di %s %s in corso", p.product, p.version)
	if total > 0 {
		return heading, fmt.Sprintf("%s di %s. L'operazione potrebbe richiedere alcuni minuti.",
			formatMB(done), formatMB(total))
	}
	return heading, fmt.Sprintf("%s scaricati. L'operazione potrebbe richiedere alcuni minuti.", formatMB(done))
}

func (p *progressUI) installText() (heading, detail string) {
	heading = fmt.Sprintf("Installazione di %s %s in corso", p.product, p.version)
	if p.self {
		return heading, "Attendere il completamento. EMLy resta utilizzabile."
	}
	return heading, fmt.Sprintf("Attendere il completamento. %s non è disponibile fino al termine.", p.product)
}

// formatMB renders n bytes as megabytes with one decimal and a decimal comma.
func formatMB(n int64) string {
	return strings.Replace(fmt.Sprintf("%.1f MB", float64(n)/(1024*1024)), ".", ",", 1)
}
