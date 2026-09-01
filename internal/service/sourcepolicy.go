package service

import (
	"context"
	"net"
	"strings"
	"time"

	"emlyupdater/internal/config"
	"emlyupdater/internal/logging"
	"emlyupdater/internal/machineinfo"
)

// dcLookup is machineinfo.NearestDomainController's signature, taken as a
// parameter so the policy can be exercised without a reachable domain.
type dcLookup func(domain string) (*machineinfo.DomainControllerInfo, error)

// localIPsLookup is machineinfo.LocalIPv4Addresses's signature, taken as a
// parameter for the same reason.
type localIPsLookup func() ([]string, error)

// decideSource picks the manifest source implied by the domain controller the
// machine can currently see, and returns the reason for the choice (logged
// verbatim, so it has to read as an explanation on its own).
//
// The DC identifies which site the machine is at; cfg.DCSubnetMap maps each
// site's DC name to the CIDR subnets its office uses. internalManifestURL
// lives in an office, so a machine whose own IP is not on one of those
// subnets cannot reach it and must go out to the public API instead.
//
// A nil/empty DCSubnetMap means "no opinion" - the configured Primary stands.
func decideSource(cfg *config.Config, dc *machineinfo.DomainControllerInfo, dcErr error, localIPs []string, ipsErr error) (want, reason string) {
	if len(cfg.DCSubnetMap) == 0 {
		return "", "source policy disabled (defaultMappingDCSubnets empty)"
	}
	if dcErr != nil {
		return config.SourceExternal, "domain controller lookup failed: " + dcErr.Error()
	}

	subnets, dcName, found := lookupSubnetsForDC(cfg.DCSubnetMap, dc.Name)
	if !found {
		return config.SourceExternal, "domain controller " + quote(dc.Name) + " has no configured subnet mapping"
	}

	if ipsErr != nil {
		return config.SourceExternal, "could not enumerate local IP addresses: " + ipsErr.Error()
	}

	for _, ip := range localIPs {
		if config.SubnetsContain(subnets, ip) {
			return config.SourceInternal, "domain controller " + quote(dcName) + " matched, local IP " + ip + " is on one of its mapped subnets"
		}
	}
	return config.SourceExternal, "domain controller " + quote(dcName) + " matched, but no local IP is on its mapped subnets"
}

// lookupSubnetsForDC finds the subnet list configured for a resolved DC name,
// matching the way sameDCName does: case-insensitively and ignoring any DNS
// suffix DsGetDcName may have appended to the resolved name.
func lookupSubnetsForDC(dcSubnets map[string][]*net.IPNet, resolved string) (subnets []*net.IPNet, key string, found bool) {
	for k, v := range dcSubnets {
		if sameDCName(resolved, k) {
			return v, k, true
		}
	}
	return nil, "", false
}

// sameDCName compares a resolved DC name against a configured one, ignoring
// case and DNS suffix: DsGetDcName returns "DC-RM2.tregcc.local" with
// DS_RETURN_DNS_NAME and plain "DC-RM2" without it, and both have to match a
// config that names the host either way.
func sameDCName(resolved, want string) bool {
	return strings.EqualFold(hostLabel(resolved), hostLabel(want))
}

// hostLabel keeps the leading label of a host name, dropping any DNS suffix.
func hostLabel(name string) string {
	label, _, _ := strings.Cut(strings.TrimSpace(name), ".")
	return label
}

func quote(s string) string { return `"` + s + `"` }

// resolveDC runs the domain controller lookup, retrying a failure on the
// cadence configured in [source] (dcLookupRetryAttempts/dcLookupRetryDelay).
//
// The retry exists for one specific failure: at boot the service can start
// before the network stack, DNS and netlogon are ready, and DsGetDcName then
// reports the domain as non-existent. decideSource cannot tell that apart from
// "this machine really is off the domain", so without a retry a machine that
// booted a second too early is pinned to the external source until the service
// restarts. domainJoined comes from the locally cached
// Win32_ComputerSystem.Domain, which answers with no network at all, so a
// genuine workgroup machine skips the wait entirely.
//
// ctx cuts the wait short when the service is asked to stop mid-retry.
func resolveDC(ctx context.Context, cfg *config.Config, log *logging.Logger, dcFn dcLookup, domainJoined bool) (*machineinfo.DomainControllerInfo, error) {
	dc, err := dcFn("")
	if err == nil {
		return dc, nil
	}

	attempts, delay := cfg.DCLookupRetryAttempts, cfg.DCLookupRetryDelay
	if attempts <= 0 || delay <= 0 {
		return dc, err
	}
	if !domainJoined {
		log.Info("domain controller lookup failed on a machine that is not domain-joined, not retrying",
			"error", err.Error())
		return dc, err
	}

	for attempt := 1; attempt <= attempts; attempt++ {
		log.Warn("domain controller lookup failed, retrying",
			"attempt", attempt, "attempts", attempts, "delay", delay.String(), "error", err.Error())
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return dc, err
		}

		dc, err = dcFn("")
		if err == nil {
			log.Info("domain controller lookup succeeded after retrying",
				"attempt", attempt, "dc", dc.Name)
			return dc, nil
		}
	}
	return dc, err
}

// applySourcePolicy runs the decision once at startup, applies it to cfg, then
// persists it to the config file at path so the choice is visible to whoever
// inspects the machine later.
//
// Nothing here stops the updater: a machine that cannot resolve its DC, or
// whose config file is read-only, still polls with whatever source it has.
// The outcome is always logged - to the file log and to the Event Log - even
// when nothing changes, so "which source did this machine pick, and why" is
// answerable from Event Viewer alone.
//
// It can block for the whole configured retry window (see resolveDC), so it is
// called from RunLoop rather than from service.New: New runs before svc.Run
// has reported the service started, where blocking would risk the SCM's
// start timeout.
func applySourcePolicy(ctx context.Context, cfg *config.Config, log *logging.Logger, path string, domainJoined bool, dcFn dcLookup, ipsFn localIPsLookup) {
	dc, dcErr := resolveDC(ctx, cfg, log, dcFn, domainJoined)
	localIPs, ipsErr := ipsFn()
	want, reason := decideSource(cfg, dc, dcErr, localIPs, ipsErr)

	// Common fields for every outcome below. logFields returns a fresh slice
	// each time so the appends cannot share a backing array.
	logFields := func() []any {
		f := []any{"reason", reason}
		if dc != nil {
			f = append(f, "dc", dc.Name, "dcIP", dc.Address, "site", dc.Site)
		}
		if len(localIPs) > 0 {
			f = append(f, "localIPs", strings.Join(localIPs, ","))
		}
		return f
	}

	switch {
	case dcErr != nil && want != "":
		log.ErrorEvent(logging.EventSourcePolicyFailed,
			"domain controller lookup failed, selecting the external update source",
			append([]any{"error", dcErr.Error()}, logFields()...)...)
	case ipsErr != nil && want != "":
		log.ErrorEvent(logging.EventSourcePolicyFailed,
			"could not enumerate local IP addresses, selecting the external update source",
			append([]any{"error", ipsErr.Error()}, logFields()...)...)
	}

	if want == "" || want == cfg.Primary {
		log.InfoEvent(logging.EventSourcePolicy, "update source unchanged",
			append([]any{"source", cfg.Primary}, logFields()...)...)
		return
	}

	// Switching to a source with no manifest URL configured would produce a
	// config that config.Load rejects on the next start, taking the service
	// down with it. Keep what we have and say why.
	if manifestURLFor(cfg, want) == "" {
		log.ErrorEvent(logging.EventSourcePolicyFailed,
			"cannot switch update source: the target source has no manifest URL configured",
			append([]any{"wanted", want, "keeping", cfg.Primary}, logFields()...)...)
		return
	}

	previous := cfg.Primary
	cfg.Primary = want
	log.WarnEvent(logging.EventSourcePolicy, "update source switched",
		append([]any{"from", previous, "to", want}, logFields()...)...)

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
