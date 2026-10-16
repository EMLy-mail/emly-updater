//go:build windows

package tray

import (
	"strconv"
	"strings"

	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"

	"emlyupdater/internal/crash"
	"emlyupdater/internal/product"
	"emlyupdater/internal/version"
)

// agentSlug is the Prodotti page's row for Aryx Agent itself: checked
// against the updater manifest instead of a product manifest.
const agentSlug = "aryxd-agent"

// productsTab is the settings window's Prodotti page: the agent and every
// product with what the agent knows locally (detection, pending entry), and
// the manifest check per row on request. The check is a dry run -
// service.Updater.CheckProduct / CheckUpdater - so it downloads and installs
// nothing; "Controlla ora" is what makes the service act.
//
// The rows are loaded the first time the page is shown after each reload of
// the settings window, not when the window opens: most openings never look
// at them.
type productsTab struct {
	f      *settingsForm
	page   *settingsPage
	list   *productList
	btnAll *fluentButton
	status string
	loaded bool // rows loaded since the last settings reload
	busy   int  // checks in flight, this generation
	gen    int  // bumped by every load: older results are dropped
}

// Prodotti page layout, 96-DPI units.
const (
	productsHeaderH = 44 // status line and "Verifica tutti"
	productsAllW    = 120
	productsListY   = pageTitleH + productsHeaderH + 4
)

// buildProducts is the "Prodotti" page.
func (f *settingsForm) buildProducts() {
	t := &productsTab{f: f}
	f.products = t
	p := newSettingsPage(f.view, "Prodotti", false)
	t.page = p

	t.btnAll = newFluentButton(p.ctl, "Verifica tutti", 0, false,
		pageCardX+contentW-productsAllW, pageTitleH+(productsHeaderH-32)/2, productsAllW)
	t.btnAll.setBackdrop(func() argb { return fp().windowBg })
	t.btnAll.OnClick(func() {
		for i := range t.list.rows {
			t.list.check(i)
		}
	})

	t.list = newProductList(p.ctl, pageCardX, productsListY, contentW)
	t.list.onCheck = t.check
	t.list.onResize = func(h int) { p.grow(productsListY + h) }
	t.list.onHi = func(top, bottom int) {
		f.view.ensureSpan(ui.DpiY(productsListY+top), ui.DpiY(productsListY+bottom))
	}

	p.extra = func(c *canvas) {
		rc := dpiRect(pageCardX, pageTitleH, contentW-productsAllW-ctlGap, productsHeaderH)
		c.text(t.status, rc, fontBody, fp().textSecondary,
			co.DT_LEFT|co.DT_VCENTER|co.DT_SINGLELINE|co.DT_END_ELLIPSIS|co.DT_NOPREFIX)
	}
	p.grow(productsListY)
}

func (t *productsTab) setStatus(s string) {
	t.status = s
	if h := t.page.ctl.Hwnd(); h != 0 {
		h.InvalidateRect(nil, false)
	}
}

// load shows the agent's own row at once, then the products from the
// backend, off the UI thread.
func (t *productsTab) load() {
	t.loaded = true
	t.gen++
	gen := t.gen
	t.busy = 0
	t.list.setRows([]productEntry{{slug: agentSlug, name: version.ProductName,
		sub: productSub(version.Version, "", true)}})
	t.setStatus("Caricamento dei prodotti…")
	f := t.f
	f.bar.Start()
	go func() {
		defer crash.Guard(nil)
		rows, err := f.a.be.products(f.a.ctx)
		f.a.wnd.UiThread(func() {
			f.bar.Stop()
			if gen != t.gen {
				return
			}
			if err != nil {
				t.setStatus("Impossibile leggere i prodotti: " + err.Error())
				return
			}
			entries := t.list.rows
			for _, r := range rows {
				name := r.Name
				if name == "" {
					name = r.Slug
				}
				entries = append(entries, productEntry{slug: r.Slug, name: name,
					sub: productSub(installedText(r.Detection), r.Pending, r.Enabled)})
			}
			t.list.setRows(entries)
			t.setStatus(strconv.Itoa(len(rows)) + " prodotti. \"Verifica\" non scarica nulla.")
		})
	}()
}

// productSub is a row's subtitle: what is installed, what is pending, and
// whether the agent updates the product at all.
func productSub(installed, pending string, enabled bool) string {
	parts := []string{"Installato: " + installed}
	if pending != "" {
		parts = append(parts, "in sospeso: "+pending)
	}
	if !enabled {
		parts = append(parts, "aggiornamenti disattivati")
	}
	return strings.Join(parts, " · ")
}

// installedText is the installed part of a row: just the version, not
// detection's full "installed X (source)" string; the source is in the
// service log.
func installedText(r product.Result) string {
	switch r.Outcome {
	case product.Installed:
		return r.Version
	case product.Unknown:
		return "sconosciuto"
	default:
		return "non installato"
	}
}

// check runs the dry-run manifest check for row i, off the UI thread.
// Checks are serialised by the backend, so "Verifica tutti" asks the server
// one manifest at a time, like a cycle does.
func (t *productsTab) check(i int) {
	slug := t.list.rows[i].slug
	if slug == "" {
		return
	}
	f := t.f
	gen := t.gen
	t.list.setOffered(i, "", true)
	t.busy++
	f.bar.Start()
	t.setStatus("Verifica in corso…")
	go func() {
		defer crash.Guard(nil)
		offered, err := f.a.be.check(f.a.ctx, slug)
		f.a.wnd.UiThread(func() {
			f.bar.Stop()
			if gen != t.gen {
				return // the rows were reloaded meanwhile
			}
			text := "Server: " + offered
			if err != nil {
				text = "errore: " + err.Error()
			}
			t.list.setOffered(i, text, false)
			t.busy--
			if t.busy == 0 {
				t.setStatus("Verifica completata.")
			}
		})
	}()
}
