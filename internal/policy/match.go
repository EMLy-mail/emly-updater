package policy

import (
	"fmt"
	"net"
	"strings"
	"time"
)

// Effective is the policy one host actually runs: the global document with
// every matching override applied, plus the ids of those overrides for the
// log.
type Effective struct {
	Doc     *Document
	Applied []string

	parsed *Parsed
}

// Effective evaluates the overrides for h and returns the resulting policy.
//
// The returned error is informational: when the combination of overrides
// that matched produces an invalid document (a case the fetch-time dry-run
// cannot fully cover, since it checks each override alone and all of them
// together but not every subset), the global document is returned instead
// and the error says why, so the caller can log it and carry on.
func (p *Parsed) Effective(h Host) (*Effective, error) {
	cur := p.raw
	var applied []string
	for _, o := range p.Global.Overrides {
		if expired(o.Until, h.Now) {
			continue
		}
		if !o.Match.matches(h) {
			continue
		}
		if o.Except != nil && o.Except.matches(h) {
			continue
		}
		cur = mergePatch(cur, o.Patch).(map[string]any)
		applied = append(applied, o.ID)
	}
	if len(applied) == 0 {
		return &Effective{Doc: p.Global, parsed: p}, nil
	}

	doc, probs := p.build(cur)
	if probs == nil {
		probs = validateDocument(doc, p.Global.Revision > 0 || p.Global.GeneratedAt != "")
	}
	if len(probs) > 0 {
		return &Effective{Doc: p.Global, parsed: p},
			fmt.Errorf("overrides %s combine into an invalid document, using the global one: %v",
				strings.Join(applied, ","), probs)
	}
	doc.Overrides = p.Global.Overrides
	return &Effective{Doc: doc, Applied: applied, parsed: p}, nil
}

// matches reports whether the selector selects h. Keys are ANDed, values
// inside one list are ORed; an `all` selector matches every host.
func (s Selector) matches(h Host) bool {
	if s.All {
		return true
	}
	if len(s.HWIDs) > 0 && !containsFold(s.HWIDs, h.HWID) {
		return false
	}
	if len(s.Hostnames) > 0 && !containsFold(s.Hostnames, h.Hostname) {
		return false
	}
	if len(s.DCs) > 0 {
		if h.DC == "" {
			return false
		}
		found := false
		for _, dc := range s.DCs {
			if SameDCName(dc, h.DC) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(s.Domains) > 0 && !containsFold(s.Domains, h.Domain) {
		return false
	}
	if len(s.Subnets) > 0 && !anyIPInSubnets(h.IPs, s.Subnets) {
		return false
	}
	return true
}

func containsFold(list []string, s string) bool {
	if s == "" {
		return false
	}
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(x), strings.TrimSpace(s)) {
			return true
		}
	}
	return false
}

func anyIPInSubnets(ips, cidrs []string) bool {
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(strings.TrimSpace(c))
		if err != nil {
			continue
		}
		for _, ip := range ips {
			if parsed := net.ParseIP(strings.TrimSpace(ip)); parsed != nil && n.Contains(parsed) {
				return true
			}
		}
	}
	return false
}

// expired reports whether an `until` timestamp is set and in the past by
// now. An unparseable value (which validation already rejects) counts as
// unset, i.e. never expiring - the conservative reading for a kill switch
// but the permissive one for an override; validation is what keeps this
// branch unreachable.
func expired(until *string, now time.Time) bool {
	if until == nil || *until == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, *until)
	if err != nil {
		return false
	}
	return !now.Before(t)
}

// Site finds the dcLookupMap entry this host is at: the nearest DC must be a
// key and one of the host's IPs must be inside one of its subnets. A
// disabled site never matches.
func (e *Effective) Site(h Host) (name string, site Site, ok bool) {
	if h.DC == "" {
		return "", Site{}, false
	}
	for key, s := range e.Doc.DCLookupMap {
		if !SameDCName(key, h.DC) || !s.IsEnabled() {
			continue
		}
		if anyIPInSubnets(h.IPs, s.InternalSubnets) {
			return key, s, true
		}
		// The DC matched but the machine is not on that site's LAN (VPN,
		// guest Wi-Fi, another office reachable through the same DC).
		return "", Site{}, false
	}
	return "", Site{}, false
}

// Chain returns the ordered server names this host should try: the matched
// site's base server then its backups, or just defaultServer when the host
// is at no site. Unknown names cannot appear here - validation guarantees
// every reference resolves.
func (e *Effective) Chain(h Host) (site string, chain []string) {
	if name, s, ok := e.Site(h); ok {
		chain = append(chain, s.BaseServer)
		chain = append(chain, s.BackupServer...)
		return name, chain
	}
	return "", []string{e.Doc.DefaultServer}
}

// BaseURL returns the base URL of a named server, "" if unknown.
func (e *Effective) BaseURL(name string) string { return e.Doc.Servers[name] }

// ManifestURL returns the EMLy manifest URL served by a named server.
func (e *Effective) ManifestURL(name string) string {
	if e.parsed != nil {
		if u, ok := e.parsed.legacyManifestURLs[name]; ok {
			return u
		}
	}
	base := e.Doc.Servers[name]
	if base == "" {
		return ""
	}
	return base + ManifestPath
}

// IsWhitelisted reports whether hostIntegrity lists this host, by hostname
// or by HWID. It says nothing about what EMLy should do with the answer.
func (e *Effective) IsWhitelisted(h Host) bool {
	w := e.Doc.HostIntegrity.Whitelist
	return containsFold(w.Hostnames, h.Hostname) || containsFold(w.HWIDs, h.HWID)
}

// UpdaterEnabled resolves control.updater at now: a block whose `until` has
// passed reads as enabled. The reason is returned for the log.
func (e *Effective) UpdaterEnabled(now time.Time) (enabled bool, reason string) {
	c := e.Doc.Control.Updater
	if c.Enabled || expired(c.Until, now) {
		return true, ""
	}
	if c.Reason != nil {
		reason = *c.Reason
	}
	return false, reason
}

// AppControl resolves control.app at now the same way, for the IPC payload.
func (e *Effective) AppControl(now time.Time) ControlApp {
	c := e.Doc.Control.App
	if !c.Enabled && expired(c.Until, now) {
		c.Enabled = true
	}
	if c.Mode != AppModeNormal && expired(c.Until, now) {
		c.Mode = AppModeNormal
	}
	return c
}
