package service

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"emlyupdater/internal/policy"
	"emlyupdater/internal/product"
)

// ProductRow is what the agent knows about one product on this machine,
// without asking any server. Backs `emly-updater products` and the tray's
// products window.
type ProductRow struct {
	Slug     string
	Name     string
	Enabled  bool
	Detected string // product.Detect's result, e.g. "installed(3.5.0)"
	// Detection is the same result unformatted, for callers (the tray) that
	// show only the version rather than Detected's full string.
	Detection product.Result
	Pending  string // "" when nothing is pending
}

// PolicyView is the effective policy for this machine, as the tray's
// settings window needs it: which config.ini keys a remote document
// overrides, and with what.
type PolicyView struct {
	// Governed is true when a remote document is in force (cached or just
	// fetched): its updater section then overrides config.ini's keys, and
	// the tray disables them. False means the policy was derived from
	// config.ini itself (remoteConfig disabled, or no document accepted yet).
	Governed  bool
	Source    string
	Revision  int64
	FetchedAt time.Time
	Site      string   // "" when no mapped site matched
	Servers   []string // manifest URLs in the order they are tried
	Updater   policy.UpdaterSettings
	Paused    bool // control.updater pauses the update machinery
	Overrides []string
}

// Prepare loads the policy and evaluates it for this host - the cached
// document, or the one derived from config.ini - as the service's cycle
// does, the domain controller lookup included. It makes the Updater
// read-only: it fetches nothing and writes nothing, not even moving an
// invalid cache aside. Must run before EffectivePolicy/ProductRows/
// CheckProduct.
//
// bootRetry applies the DC lookup's boot-time retry window (up to
// dcLookupRetry.attempts x delaySeconds) as the service's first cycle does.
// Without it a failed lookup is believed at once, which is what an
// interactive caller wants: the machine finished booting long ago, and the
// user is watching a "loading" label.
func (u *Updater) Prepare(ctx context.Context, bootRetry bool) {
	u.readOnly = true
	u.initPolicy() // no-op when already set; cached document or config.ini otherwise
	// first=false: resolveHost then looks the DC up once (the address set
	// differs from the empty one it remembers) with no retry.
	u.beginCycle(ctx, bootRetry)
}

// EffectivePolicy returns the effective policy prepared by Prepare.
func (u *Updater) EffectivePolicy() PolicyView {
	cyc := u.current()
	v := PolicyView{
		Governed:  cyc.snap.Source != policy.SourceDefault,
		Source:    cyc.snap.Source.String(),
		Revision:  cyc.snap.Revision(),
		FetchedAt: cyc.snap.FetchedAt,
		Site:      cyc.site,
		Updater:   cyc.eff.Doc.Updater,
		Overrides: cyc.eff.Applied,
	}
	for _, name := range cyc.chain {
		v.Servers = append(v.Servers, cyc.eff.ManifestURL(name))
	}
	enabled, _ := cyc.eff.UpdaterEnabled(cyc.host.Now)
	v.Paused = !enabled
	return v
}

// reportProducts is EMLy and every product of the effective document,
// disabled ones included, EMLy last as in a cycle.
func (u *Updater) reportProducts(cyc *cycleState) ([]*product.Product, map[string]bool) {
	enabled := map[string]bool{product.EMLySlug: true}
	for _, p := range u.documentProducts(cyc, true) {
		enabled[p.Slug] = true
	}
	return append(u.documentProducts(cyc, false), u.emlyProduct(cyc)), enabled
}

// ProductRows lists, for every product: whether it is enabled, what
// detection finds and the pending entry.
func (u *Updater) ProductRows() []ProductRow {
	list, enabled := u.reportProducts(u.current())
	rows := make([]ProductRow, 0, len(list))
	for _, p := range list {
		det := product.Detect(p)
		row := ProductRow{Slug: p.Slug, Name: p.Name, Enabled: enabled[p.Slug], Detected: det.String(), Detection: det}
		if pend, err := u.Store.PendingFor(p.Slug); err == nil && pend != nil {
			row.Pending = fmt.Sprintf("%s (attempts %d, gaveUp %v)", pend.Version, pend.Attempts, pend.GaveUp)
		}
		rows = append(rows, row)
	}
	return rows
}

// CheckProduct asks the server which version the product's manifest offers
// on its channel - a dry run: no download, no state.json write, and the
// preferred server does not move. The result reads "3.6.0 (stable)".
func (u *Updater) CheckProduct(ctx context.Context, slug string) (string, error) {
	cyc := u.current()
	list, _ := u.reportProducts(cyc)
	for _, p := range list {
		if p.Slug != slug {
			continue
		}
		ps, ok := u.resolveProductState(p)
		if !ok {
			return "", fmt.Errorf("installed version unknown, not checked")
		}
		if p.Legacy {
			// notePreferred=false: a report must not move the preferred server.
			_, _, target, err := u.resolveTargetWith(ctx, cyc, ps.Channel, false)
			if err != nil {
				return "", err
			}
			return target.Version + " (" + ps.Channel + ")", nil
		}
		_, _, target, err := u.resolveProductTarget(ctx, cyc, p, ps.Channel)
		if err != nil {
			return "", err
		}
		return target.Version + " (" + ps.Channel + ")", nil
	}
	return "", fmt.Errorf("unknown product %q", slug)
}

// CheckUpdater asks the server which version of the agent itself is
// published - the same dry run the client channel's updater.manifest.check
// performs.
func (u *Updater) CheckUpdater(ctx context.Context) (string, error) {
	_, m, _, err := u.resolveUpdaterManifestWith(ctx, u.current(), false)
	if err != nil {
		return "", err
	}
	return m.Version, nil
}

// ReportProducts prints ProductRows and - with check - the version each
// manifest offers. Read-only: it downloads and installs nothing and does not
// touch the preferred server. Backs `emly-updater products`.
func (u *Updater) ReportProducts(ctx context.Context, w io.Writer, check bool) error {
	u.Prepare(ctx, true)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PRODUCT\tENABLED\tDETECTED\tPENDING\tOFFERED")
	for _, r := range u.ProductRows() {
		pending := r.Pending
		if pending == "" {
			pending = "-"
		}
		offered := "-"
		if check {
			if v, err := u.CheckProduct(ctx, r.Slug); err != nil {
				offered = "error: " + err.Error()
			} else {
				offered = v
			}
		}
		fmt.Fprintf(tw, "%s\t%v\t%s\t%s\t%s\n", r.Slug, r.Enabled, r.Detected, pending, offered)
	}
	return tw.Flush()
}
