//go:build windows

package tray

import (
	"strconv"

	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/ui"
	"github.com/rodrigocfd/windigo/win"

	"emlyupdater/internal/version"
)

// agentSlug is the products window's row for AryxD Agent itself: checked
// against the updater manifest instead of a product manifest.
const agentSlug = "aryxd-agent"

// Columns of the products list.
const (
	colName = iota
	colEnabled
	colDetected
	colPending
	colOffered
)

// productsWindow lists the agent and every product with what the agent
// knows locally (detection, pending entry), and runs the manifest check per
// row on request. The check is a dry run - service.Updater.CheckProduct /
// CheckUpdater - so it downloads and installs nothing; "Controlla ora" in
// the settings window is what makes the service act.
type productsWindow struct {
	a        *app
	wnd      *ui.Modal
	list     *ui.ListView
	status   *ui.Static
	btnOne   *ui.Button
	btnAll   *ui.Button
	btnClose *ui.Button
	busy     int      // checks in flight
	slugs    []string // per row index; rows are only ever appended
}

// showProducts opens the window and blocks until it is closed (a modal loop,
// which keeps dispatching the tray icon's messages meanwhile).
func showProducts(a *app) {
	p := &productsWindow{a: a}
	p.wnd = ui.NewModal(a.wnd, ui.OptsModal().
		Title(version.ProductName+" - Prodotti e versioni").
		Size(ui.Dpi(700, 330)))

	p.list = ui.NewListView(p.wnd, ui.OptsListView().
		Position(ui.Dpi(12, 12)).Size(ui.Dpi(676, 230)).
		CtrlStyle(co.LVS_REPORT|co.LVS_SINGLESEL|co.LVS_SHOWSELALWAYS|co.LVS_NOSORTHEADER).
		CtrlExStyle(co.LVS_EX_FULLROWSELECT|co.LVS_EX_DOUBLEBUFFER).
		Column("Prodotto", ui.DpiX(120)).
		Column("Aggiornamenti", ui.DpiX(90)).
		Column("Installato", ui.DpiX(200)).
		Column("In sospeso", ui.DpiX(110)).
		Column("Offerto dal server", ui.DpiX(150)))
	p.status = ui.NewStatic(p.wnd, ui.OptsStatic().Position(ui.Dpi(12, 252)).Size(ui.Dpi(676, 18)).
		Text("Caricamento…"))
	p.btnOne = ui.NewButton(p.wnd, ui.OptsButton().Text("&Verifica selezionato").
		Position(ui.Dpi(12, 280)).Width(ui.DpiX(140)))
	p.btnAll = ui.NewButton(p.wnd, ui.OptsButton().Text("Verifica &tutti").
		Position(ui.Dpi(162, 280)).Width(ui.DpiX(110)))
	p.btnClose = ui.NewButton(p.wnd, ui.OptsButton().Text("&Chiudi").
		Position(ui.Dpi(603, 280)).Width(ui.DpiX(85)))

	installTheme(p.wnd)
	darkListHeader(p.list)
	for _, b := range []*ui.Button{p.btnOne, p.btnAll, p.btnClose} {
		darkButton(b)
	}
	p.wnd.On().Wm(wmSettingChange, func(m ui.Wm) uintptr {
		if isThemeChange(m) {
			applyTheme(p.wnd)
		}
		return 0
	})
	p.wnd.On().WmCreate(func(_ ui.WmCreate) int {
		fixHeaderID(p.wnd)
		applyTheme(p.wnd)
		p.load()
		return 0
	})
	p.btnOne.On().BnClicked(func() {
		for _, it := range p.list.SelectedItems() {
			p.check(it)
		}
	})
	p.list.On().NmDblClk(func(_ *win.NMITEMACTIVATE) {
		for _, it := range p.list.SelectedItems() {
			p.check(it)
		}
	})
	p.btnAll.On().BnClicked(func() {
		for _, it := range p.list.Items() {
			p.check(it)
		}
	})
	p.btnClose.On().BnClicked(func() { p.wnd.Hwnd().SendMessage(co.WM_CLOSE, 0, 0) })
	p.wnd.ShowModal()
}

func (p *productsWindow) load() {
	p.addRow(agentSlug, version.ProductName, "sì", version.Version, "")
	go func() {
		rows, err := p.a.be.products(p.a.ctx)
		p.wnd.UiThread(func() {
			if err != nil {
				p.status.Hwnd().SetWindowText("Impossibile leggere i prodotti: " + err.Error())
				return
			}
			for _, r := range rows {
				name := r.Name
				if name == "" {
					name = r.Slug
				}
				enabled := "sì"
				if !r.Enabled {
					enabled = "disattivati"
				}
				p.addRow(r.Slug, name, enabled, r.Detected, r.Pending)
			}
			p.status.Hwnd().SetWindowText(strconv.Itoa(len(rows)) +
				" prodotti. \"Verifica\" chiede al server la versione pubblicata senza scaricare nulla.")
		})
	}()
}

// addRow appends a row and remembers its slug by index.
// Not ListViewItem.SetData: in testing, the agent's row read back EMLy's
// slug. The list is never sorted or pruned, so the index is stable.
func (p *productsWindow) addRow(slug string, texts ...string) {
	p.list.AddItem(texts...)
	p.slugs = append(p.slugs, slug)
}

// check runs the dry-run manifest check for one row, off the UI thread.
// Checks are serialised by the backend, so "Verifica tutti" asks the server
// one manifest at a time, like a cycle does.
func (p *productsWindow) check(it ui.ListViewItem) {
	idx := it.Index()
	if idx < 0 || idx >= len(p.slugs) {
		return
	}
	slug := p.slugs[idx]
	if slug == "" {
		return
	}
	it.SetText(colOffered, "verifica in corso…")
	p.busy++
	p.status.Hwnd().SetWindowText("Verifica in corso…")
	go func() {
		offered, err := p.a.be.check(p.a.ctx, slug)
		p.wnd.UiThread(func() {
			if err != nil {
				offered = "errore: " + err.Error()
			}
			it.SetText(colOffered, offered)
			p.busy--
			if p.busy == 0 {
				p.status.Hwnd().SetWindowText("Verifica completata.")
			}
		})
	}()
}
