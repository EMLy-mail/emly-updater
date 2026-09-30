package service

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"emlyupdater/internal/assoc"
	"emlyupdater/internal/notify"
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
	lang     string
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
		lang:     u.Cfg.ResolveEMLy().Language,
		iconPath: assoc.ExePath(u.Cfg.EMLyInstallDir, u.Cfg.EMLyExeName),
		product:  "EMLy",
	}
	if self {
		p.product = version.ProductName
		p.iconPath = "" // the helper falls back to its own icon: this binary's
	}
	return p
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
		p.u.Log.Debug("progress window skipped: no active console session", "product", p.product)
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

func (p *progressUI) it() bool { return p.lang == "it" }

func (p *progressUI) title() string {
	if p.it() {
		return p.product + " - Aggiornamento"
	}
	return p.product + " - Update"
}

func (p *progressUI) downloadText(done, total int64) (heading, detail string) {
	if p.it() {
		heading = fmt.Sprintf("Download di %s %s in corso", p.product, p.version)
		if total > 0 {
			detail = fmt.Sprintf("%s di %s. L'operazione potrebbe richiedere alcuni minuti.",
				formatMB(done, true), formatMB(total, true))
		} else {
			detail = fmt.Sprintf("%s scaricati. L'operazione potrebbe richiedere alcuni minuti.", formatMB(done, true))
		}
		return heading, detail
	}
	heading = fmt.Sprintf("Downloading %s %s", p.product, p.version)
	if total > 0 {
		detail = fmt.Sprintf("%s of %s. This might take several minutes.", formatMB(done, false), formatMB(total, false))
	} else {
		detail = fmt.Sprintf("%s downloaded. This might take several minutes.", formatMB(done, false))
	}
	return heading, detail
}

func (p *progressUI) installText() (heading, detail string) {
	if p.it() {
		heading = fmt.Sprintf("Installazione di %s %s in corso", p.product, p.version)
		if p.self {
			return heading, "Attendere il completamento. EMLy resta utilizzabile."
		}
		return heading, "Attendere il completamento. EMLy non è disponibile fino al termine."
	}
	heading = fmt.Sprintf("Installing %s %s", p.product, p.version)
	if p.self {
		return heading, "Please wait for it to complete. EMLy remains available."
	}
	return heading, "Please wait for it to complete. EMLy is unavailable until it finishes."
}

// formatMB renders n bytes as megabytes with one decimal, with the decimal
// comma in Italian.
func formatMB(n int64, italian bool) string {
	s := fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	if italian {
		s = strings.Replace(s, ".", ",", 1)
	}
	return s
}
