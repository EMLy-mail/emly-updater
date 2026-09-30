package service

import (
	"errors"
	"maps"
	"slices"
	"time"

	"emlyupdater/internal/manifest"
	"emlyupdater/internal/policy"
	"emlyupdater/internal/product"
	"emlyupdater/internal/source"
	"emlyupdater/internal/state"
)

// productUnavailableFor is how long a product whose manifest answered 404,
// or listed nothing published, is left alone (spec §5.3).
const productUnavailableFor = 6 * time.Hour

// maxProductAttempts caps the failing installs of one target version of a
// product other than EMLy (spec §6.2).
const maxProductAttempts = 3

// productFromPolicy turns a document entry into a product definition.
func productFromPolicy(slug string, s policy.ProductSettings) *product.Product {
	p := &product.Product{
		Slug: slug, Name: s.Name, InstallDir: s.InstallDir, ExeName: s.ExeName, Channel: s.Channel,
		Installer:         product.InstallerSpec{Type: s.Installer.Type, CleanReinstall: s.Installer.CleanReinstall},
		InstallWhenAbsent: s.InstallWhenAbsent,
	}
	for _, d := range s.Detect {
		p.Detect = append(p.Detect, product.VersionSource{Type: d.Type, Path: d.Path, Section: d.Section, Key: d.Key})
	}
	return p
}

// documentProducts lists the document's products, sorted by slug, from the
// cycle's effective document (overrides applied) - or, with no cycle yet,
// from the global one.
func (u *Updater) documentProducts(cyc *cycleState, enabledOnly bool) []*product.Product {
	var doc *policy.Document
	switch {
	case cyc != nil && cyc.eff != nil:
		doc = cyc.eff.Doc
	case u.Policy != nil && u.Policy.Current() != nil:
		doc = u.Policy.Current().Parsed.Global
	default:
		return nil
	}
	var out []*product.Product
	for _, slug := range slices.Sorted(maps.Keys(doc.Products)) {
		s := doc.Products[slug]
		if enabledOnly && !s.Enabled {
			continue
		}
		out = append(out, productFromPolicy(slug, s))
	}
	return out
}

// cycleProducts is this cycle's round: the enabled products, then EMLy. EMLy
// goes last because it can block (WaitForExit); a product before it never
// waits on it.
func (u *Updater) cycleProducts(cyc *cycleState) []*product.Product {
	return append(u.documentProducts(cyc, true), u.emlyProduct(cyc))
}

// markUnavailable records that p's manifest has nothing for this machine and
// reports whether err was such an answer.
func (u *Updater) markUnavailable(p *product.Product, err error) bool {
	if !errors.Is(err, source.ErrNotFound) && !errors.Is(err, manifest.ErrNoRelease) {
		return false
	}
	if u.unavailableUntil == nil {
		u.unavailableUntil = map[string]time.Time{}
	}
	u.unavailableUntil[p.Slug] = u.clock().Add(productUnavailableFor)
	u.Log.Info("no release available for this product, not asking again for a while",
		"product", p.Slug, "for", productUnavailableFor.String(), "reason", err.Error())
	return true
}

func (u *Updater) unavailable(p *product.Product) bool {
	until, ok := u.unavailableUntil[p.Slug]
	if !ok {
		return false
	}
	if u.clock().Before(until) {
		return true
	}
	delete(u.unavailableUntil, p.Slug)
	return false
}

// recordFailedAttempt counts one failed install of pend's version and gives
// the version up at maxProductAttempts.
func (u *Updater) recordFailedAttempt(p *product.Product, pend *state.Pending) {
	pend.Attempts++
	if pend.Attempts >= maxProductAttempts {
		pend.GaveUp = true
		u.Log.Error("giving up on this version after repeated install failures; waiting for a different release",
			"product", p.Slug, "version", pend.Version, "attempts", pend.Attempts)
	}
	if err := u.Store.SetPendingFor(p.Slug, pend); err != nil {
		u.Log.Warn("failed to persist the attempt counter", "product", p.Slug, "error", err.Error())
	}
}
