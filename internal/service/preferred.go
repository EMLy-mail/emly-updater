package service

import (
	"errors"
	"slices"

	"emlyupdater/internal/source"
)

// The preferred server is the one this service session has found reachable
// when the head of the policy chain was not.
//
// Without it every cycle starts over at chain[0]: a machine whose base server
// is unreachable for the whole session sits out the primary's retries and
// backoff on every single poll before reaching the backup that works, and
// the presence channel - which only ever follows the head of the chain -
// never connects at all. Once a backup has answered, it moves to the front
// of the chain for everything that reads it (the EMLy manifest, the
// updater's own manifest, the presence channel) until either the service
// restarts, the policy stops listing it for this machine, or a later
// resolution is served by another server of the chain.
//
// It lives only in memory on purpose: a restart is the operator's way of
// saying "try the configured order again", and the next boot may well be on
// a network where the base server is fine.

// preferredServer returns the server this session currently prefers, "" for
// none. Read from the poll loop and the presence supervisor alike, hence the
// atomic.
func (u *Updater) preferredServer() string {
	if p := u.preferred.Load(); p != nil {
		return *p
	}
	return ""
}

// preferredChain is cyc.chain with the preferred server moved to its head,
// or cyc.chain itself when there is no preference or it already leads.
//
// A preference the chain no longer lists - the machine moved site, or a new
// policy revision dropped that backup - is forgotten here, so it cannot
// resurface if the policy lists it again later for unrelated reasons.
func (u *Updater) preferredChain(cyc *cycleState) []string {
	p := u.preferred.Load()
	if p == nil || len(cyc.chain) == 0 || *p == cyc.chain[0] {
		return cyc.chain
	}
	i := slices.Index(cyc.chain, *p)
	if i < 0 {
		if u.preferred.CompareAndSwap(p, nil) {
			u.Log.Debug("preferred server is no longer in this machine's server chain, back to the policy order",
				"preferred", *p, "chain", describeChain(cyc.eff, cyc.chain))
		}
		return cyc.chain
	}
	chain := make([]string, 0, len(cyc.chain))
	chain = append(chain, *p)
	chain = append(chain, cyc.chain[:i]...)
	chain = append(chain, cyc.chain[i+1:]...)
	return chain
}

// serverFor maps the source a resolver answered with back to its server name
// in cyc's chain, "" if it matches none.
func serverFor(cyc *cycleState, src source.Source) string {
	http, ok := src.(*source.HTTPSource)
	if !ok {
		return ""
	}
	for _, name := range cyc.chain {
		if cyc.eff.ManifestURL(name) == http.ManifestURL {
			return name
		}
	}
	return ""
}

// notePreferredServer records which server of the chain served a document
// this cycle, after a successful resolution through r.
//
// A primary that answered 404 is not a reachability failure - the server is
// up, it just does not publish that document (typically a mirror without an
// updater manifest) - so a fallback that answers after one does not become
// preferred: pinning it would drag EMLy's manifest and the presence channel
// off a server that serves them fine.
func (u *Updater) notePreferredServer(cyc *cycleState, r *source.Resolver, src source.Source) {
	if len(cyc.chain) == 0 {
		return
	}
	if r.PrimaryErr != nil && errors.Is(r.PrimaryErr, source.ErrNotFound) {
		return
	}
	served := serverFor(cyc, src)
	if served == "" {
		return
	}

	prev := u.preferredServer()
	head := cyc.chain[0]
	if served == head {
		if prev != "" {
			u.preferred.Store(nil)
			u.Log.Debug("policy head server answered again, dropping the preferred server for this session",
				"server", head, "previouslyPreferred", prev)
			u.wakeClientWS()
		}
		return
	}
	if served == prev {
		return
	}
	u.preferred.Store(&served)
	fields := []any{"server", served, "url", cyc.eff.BaseURL(served), "policyHead", head,
		"previouslyPreferred", prev}
	if r.PrimaryErr != nil {
		fields = append(fields, "primaryError", r.PrimaryErr.Error())
	}
	u.Log.Debug("using a backup server for the rest of this service session, for the manifests and the presence channel",
		fields...)
	u.wakeClientWS()
}

// wakeClientWS nudges the presence supervisor to re-read its target now
// rather than at the end of its current backoff or watch interval. It never
// blocks: a wake already pending covers this one too.
func (u *Updater) wakeClientWS() {
	select {
	case u.clientWSWake <- struct{}{}:
	default:
	}
}
