package service

import (
	"strings"

	"emlyupdater/internal/config"
	"emlyupdater/internal/logging"
	"emlyupdater/internal/machineinfo"
)

// dcLookup is machineinfo.NearestDomainController's signature, taken as a
// parameter so the policy can be exercised without a reachable domain.
type dcLookup func(domain string) (*machineinfo.DomainControllerInfo, error)

// decideSource picks the manifest source implied by the domain controller the
// machine can currently see, and returns the reason for the choice (logged
// verbatim, so it has to read as an explanation on its own).
//
// The DC is a proxy for "am I on the internal LAN?": internalManifestURL
// lives in the office, so a machine that cannot see the expected DC on the
// expected subnet cannot reach it and must go out to the public API instead.
//
// An empty want means "no opinion" - the configured Primary stands.
func decideSource(cfg *config.Config, dc *machineinfo.DomainControllerInfo, err error) (want, reason string) {
	switch {
	case cfg.InternalDCName == "" || len(cfg.InternalDCSubnets) == 0:
		return "", "source policy disabled (internalDCName or internalDCSubnets empty)"
	case err != nil:
		return config.SourceExternal, "domain controller lookup failed: " + err.Error()
	case !sameDCName(dc.Name, cfg.InternalDCName):
		return config.SourceExternal, "domain controller is " + quote(dc.Name) + ", expected " + quote(cfg.InternalDCName)
	case !dc.AddressIsIP:
		return config.SourceExternal, "domain controller " + quote(dc.Name) + " answered from a NetBIOS address, not an IP"
	case !config.SubnetsContain(cfg.InternalDCSubnets, dc.Address):
		return config.SourceExternal, "domain controller " + quote(dc.Name) + " is at " + dc.Address + ", outside the internal subnets"
	}
	return config.SourceInternal, "domain controller " + quote(dc.Name) + " at " + dc.Address + " is on an internal subnet"
}

// sameDCName compares a resolved DC name against the configured one, ignoring
// case and DNS suffix: DsGetDcName returns "DC-RM2.tregcc.local" with
// DS_RETURN_DNS_NAME and plain "DC-RM2" without it, and both have to match a
// config that names the host either way.
func sameDCName(resolved, want string) bool {
	return strings.EqualFold(hostLabel(resolved), hostLabel(want))
}

// hostLabel keeps the leading label of a host name, dropping any DNS suffix.
func hostLabel(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.Index(name, "."); i >= 0 {
		return name[:i]
	}
	return name
}

func quote(s string) string { return `"` + s + `"` }

// applySourcePolicy runs the decision once at startup, applies it to cfg, then
// persists it to the config file at path so the choice is visible to whoever
// inspects the machine later.
//
// Nothing here stops the updater: a machine that cannot resolve its DC, or
// whose config file is read-only, still polls with whatever source it has.
// The outcome is always logged - to the file log and to the Event Log - even
// when nothing changes, so "which source did this machine pick, and why" is
// answerable from Event Viewer alone.
func applySourcePolicy(cfg *config.Config, log *logging.Logger, path string, lookup dcLookup) {
	dc, lookupErr := lookup("")
	want, reason := decideSource(cfg, dc, lookupErr)

	// Common fields for every outcome below. dcFields returns a fresh slice
	// each time so the appends cannot share a backing array.
	dcFields := func() []any {
		f := []any{"reason", reason}
		if dc != nil {
			f = append(f, "dc", dc.Name, "dcIP", dc.Address, "site", dc.Site)
		}
		return f
	}

	if lookupErr != nil && want != "" {
		log.ErrorEvent(logging.EventSourcePolicyFailed,
			"domain controller lookup failed, selecting the external update source",
			append([]any{"error", lookupErr.Error()}, dcFields()...)...)
	}

	if want == "" || want == cfg.Primary {
		log.InfoEvent(logging.EventSourcePolicy, "update source unchanged",
			append([]any{"source", cfg.Primary}, dcFields()...)...)
		return
	}

	// Switching to a source with no manifest URL configured would produce a
	// config that config.Load rejects on the next start, taking the service
	// down with it. Keep what we have and say why.
	if manifestURLFor(cfg, want) == "" {
		log.ErrorEvent(logging.EventSourcePolicyFailed,
			"cannot switch update source: the target source has no manifest URL configured",
			append([]any{"wanted", want, "keeping", cfg.Primary}, dcFields()...)...)
		return
	}

	previous := cfg.Primary
	cfg.Primary = want
	log.WarnEvent(logging.EventSourcePolicy, "update source switched",
		append([]any{"from", previous, "to", want}, dcFields()...)...)

	// The in-memory override above is what this run uses; persisting is for
	// the next start and for anyone reading the config by hand, so a write
	// failure is reported and then tolerated.
	if err := config.SetPrimary(path, want); err != nil {
		log.ErrorEvent(logging.EventSourcePolicyFailed,
			"update source switched in memory but could not be written to the config file",
			"path", path, "to", want, "error", err.Error())
	}
}

// manifestURLFor returns the manifest URL a given source would use. It mirrors
// Config.PrimaryManifestURL, which can only answer for the *current* Primary.
func manifestURLFor(cfg *config.Config, source string) string {
	if source == config.SourceInternal {
		return cfg.InternalManifestURL
	}
	return cfg.ExternalManifestURL
}
