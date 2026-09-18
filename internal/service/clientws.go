package service

import (
	"time"

	"emlyupdater/internal/source"
	"emlyupdater/internal/wsclient"
)

// clientWSTarget is everything the presence supervisor needs to know from
// the current cycle: whether the remote document enables the channel at all,
// and which server to hold it open against.
//
// It is deliberately comparable, so the supervisor can watch for a change
// with a plain != while a connection is up.
type clientWSTarget struct {
	enabled bool
	server  string // the server's name in the document, for the log
	url     string // the full ws:// or wss:// endpoint
}

// clientWSTarget reads the current cycle and resolves where the presence
// channel should be pointed.
//
// The channel has no notion of its own about the right server: it reads the
// same chain beginCycle built for the rest of the cycle and takes the head
// of it - the site's base server for a machine on a mapped LAN, the default
// server for everyone else. A machine that moves site therefore moves its
// presence connection with it, with no extra configuration anywhere.
//
// A zero clientWSTarget means "do not connect", which covers every reason
// not to: the document's kill switch is off, no cycle has run yet, or the
// chosen server's base URL is not something a WebSocket can be built from.
// The last case is what a legacy-derived policy produces when config.ini
// carried a hand-edited manifest path, and it is harmless precisely because
// such a machine has no remote document and therefore no kill switch on.
func (u *Updater) clientWSTarget() clientWSTarget {
	cyc := u.cur.Load()
	if cyc == nil || !cyc.eff.Doc.ClientWS.Enabled || len(cyc.chain) == 0 {
		return clientWSTarget{}
	}
	name := cyc.chain[0]
	url, err := wsclient.URLFor(cyc.eff.BaseURL(name))
	if err != nil {
		u.Log.Warn("presence channel has no usable endpoint on the current server, staying closed",
			"server", name, "baseURL", cyc.eff.BaseURL(name), "error", err.Error())
		return clientWSTarget{}
	}
	return clientWSTarget{enabled: true, server: name, url: url}
}

// clientWSIdentity resolves this machine's identity for the channel's first
// message.
//
// It goes through newHTTPSource rather than reading u.Machine directly, so
// the values - and above all the two that are re-resolved on every request,
// the logged-on user and EMLy's installed version - come from the one place
// the X-EMLy-* headers come from. A field added to the headers and not here
// still has to be added twice (see AGENTS.md), but at least the two paths
// cannot disagree about *how* a value is resolved.
//
// The manifest URL passed to newHTTPSource is empty because nothing here
// fetches anything: only the identity fields are read off the result.
//
// X-EMLy-IntIP has no counterpart in the payload on purpose (spec §4): it is
// specific to the manifest/download path, and the API reads this
// connection's address off the connection itself.
func (u *Updater) clientWSIdentity() wsclient.Identity {
	s := u.newHTTPSource("")
	return identityFromSource(s)
}

// identityFromSource maps an HTTPSource's identity fields onto the JSON
// payload. Split out from clientWSIdentity so the mapping itself - the part
// that goes stale when a field is added - is one obvious list.
func identityFromSource(s *source.HTTPSource) wsclient.Identity {
	id := wsclient.Identity{
		HWID:            s.HWID,
		Hostname:        s.Hostname,
		ADDomain:        s.ADDomain,
		LoggedUser:      s.LoggedUser,
		LoggedUserState: s.LoggedUserState,
		Serial:          s.Serial,
		Product:         s.Product,
		OSVersion:       s.OSVersion,
		EMLyVersion:     s.EMLyVersion,
	}
	if !s.LoggedUserDisconnectedAt.IsZero() {
		id.LoggedUserDisconnectedAt = s.LoggedUserDisconnectedAt.UTC().Format(time.RFC3339)
	}
	return id
}
