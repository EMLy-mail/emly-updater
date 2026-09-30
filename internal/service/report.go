package service

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"emlyupdater/internal/product"
)

// ReportProducts prints, for EMLy and every product of the effective
// document (disabled ones included): whether it is enabled, what detection
// finds, the pending entry, and - with check - the version the manifest
// offers. Read-only: it downloads and installs nothing and does not touch
// the preferred server. Backs `emly-updater products`.
func (u *Updater) ReportProducts(ctx context.Context, w io.Writer, check bool) error {
	u.initPolicy() // no-op when already set; cached document or config.ini otherwise
	cyc := u.beginCycle(ctx, true)
	enabled := map[string]bool{product.EMLySlug: true}
	for _, p := range u.documentProducts(cyc, true) {
		enabled[p.Slug] = true
	}
	list := append(u.documentProducts(cyc, false), u.emlyProduct(cyc))

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PRODUCT\tENABLED\tDETECTED\tPENDING\tOFFERED")
	for _, p := range list {
		detected := product.Detect(p).String()
		pending := "-"
		if pend, err := u.Store.PendingFor(p.Slug); err == nil && pend != nil {
			pending = fmt.Sprintf("%s (attempts %d, gaveUp %v)", pend.Version, pend.Attempts, pend.GaveUp)
		}
		offered := "-"
		if check {
			ps, ok := u.resolveProductState(p)
			if !ok {
				offered = "not checked (version unknown)"
			} else if p.Legacy {
				// notePreferred=false: a report must not move the preferred server.
				if _, _, target, err := u.resolveTargetWith(ctx, cyc, ps.Channel, false); err != nil {
					offered = "error: " + err.Error()
				} else {
					offered = target.Version + " (" + ps.Channel + ")"
				}
			} else if _, _, target, err := u.resolveProductTarget(ctx, cyc, p, ps.Channel); err != nil {
				offered = "error: " + err.Error()
			} else {
				offered = target.Version + " (" + ps.Channel + ")"
			}
		}
		fmt.Fprintf(tw, "%s\t%v\t%s\t%s\t%s\n", p.Slug, enabled[p.Slug], detected, pending, offered)
	}
	return tw.Flush()
}
