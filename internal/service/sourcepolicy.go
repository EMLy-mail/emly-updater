package service

import (
	"context"
	"slices"
	"strings"
	"time"

	"emlyupdater/internal/logging"
	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/policy"
)

// dcLookup is machineinfo.NearestDomainController's signature, taken as a
// parameter so the policy can be exercised without a reachable domain.
type dcLookup func(domain string) (*machineinfo.DomainControllerInfo, error)

// localIPsLookup is machineinfo.LocalIPv4Addresses's signature, taken as a
// parameter for the same reason.
type localIPsLookup func() ([]string, error)

// dcCacheTTL bounds how long a resolved domain controller is trusted without
// asking again when the local addresses did not change.
const dcCacheTTL = time.Hour

// siteState is what the source policy remembers between cycles: the last DC
// answer (so the lookup runs only when the network facts changed), and the
// last decision it logged (so event 700 fires on change, not on every poll).
type siteState struct {
	dc      *machineinfo.DomainControllerInfo
	dcErr   error
	dcAt    time.Time
	ips     []string
	lastKey string
	// dcFailedLogged keeps event 701 to one per failure streak.
	dcFailedLogged bool
}

// resolveDC runs the domain controller lookup, retrying a failure on the
// cadence the effective policy configures (updater.dcLookupRetry).
//
// The retry exists for one specific failure: at boot the service can start
// before the network stack, DNS and netlogon are ready, and DsGetDcName then
// reports the domain as non-existent. The site match cannot tell that apart
// from "this machine really is off the domain", so without a retry a machine
// that booted a second too early is pinned to the default server until the
// service restarts. domainJoined comes from the locally cached
// Win32_ComputerSystem.Domain, which answers with no network at all, so a
// genuine workgroup machine skips the wait entirely.
//
// ctx cuts the wait short when the service is asked to stop mid-retry.
func resolveDC(ctx context.Context, retry policy.DCLookupRetry, log *logging.Logger, dcFn dcLookup, domainJoined bool) (*machineinfo.DomainControllerInfo, error) {
	dc, err := dcFn("")
	if err == nil {
		return dc, nil
	}

	attempts, delay := retry.Attempts, retry.Delay()
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

// resolveHost gathers this cycle's facts about the machine. The local
// addresses are read every time (no network involved); the domain
// controller is re-queried only on the first call - with the boot-time retry
// window - and afterwards when the address set changed or the cached answer
// is older than dcCacheTTL, a single attempt each time.
func (u *Updater) resolveHost(ctx context.Context, first bool) policy.Host {
	now := u.clock()
	ips, ipsErr := u.ipsFn()
	if ipsErr != nil {
		u.Log.Warn("could not enumerate local IP addresses", "error", ipsErr.Error())
		ips = nil
	}

	joined := machineinfo.DomainJoined(u.Machine.ADDomain, u.Machine.Hostname)
	needDC := first || !slices.Equal(ips, u.site.ips) || now.Sub(u.site.dcAt) > dcCacheTTL
	if needDC {
		retry := u.Policy.Current().Parsed.Global.Updater.DCLookupRetry
		if !first {
			retry = policy.DCLookupRetry{}
		}
		u.site.dc, u.site.dcErr = resolveDC(ctx, retry, u.Log, u.dcFn, joined)
		u.site.dcAt = now
		u.site.ips = ips
	}

	host := policy.Host{
		HWID:     u.Machine.HWID,
		Hostname: u.Machine.Hostname,
		IPs:      ips,
		Now:      now,
	}
	if joined {
		host.Domain = u.Machine.ADDomain
	}
	if u.site.dcErr == nil && u.site.dc != nil {
		host.DC = u.site.dc.Name
	}
	return host
}

// beginCycle takes the policy snapshot for this cycle, evaluates it for this
// host and records the result where the rest of the cycle (and the IPC
// server) read it. The source decision is logged as event 700 at startup
// and whenever it changes; a failed DC lookup on a domain-joined machine is
// event 701, once per failure streak.
func (u *Updater) beginCycle(ctx context.Context, first bool) *cycleState {
	snap := u.Policy.Current()
	host := u.resolveHost(ctx, first)

	eff, err := snap.Parsed.Effective(host)
	if err != nil {
		u.Log.Warn("effective policy fell back to the global document", "error", err.Error())
	}
	site, chain := eff.Chain(host)

	cyc := &cycleState{snap: snap, host: host, eff: eff, site: site, chain: chain}
	u.cur.Store(cyc)

	u.applyLogging(eff.Doc.Logging)
	u.warnIfStale(snap, host.Now)

	joined := machineinfo.DomainJoined(u.Machine.ADDomain, u.Machine.Hostname)
	if u.site.dcErr != nil && joined {
		if !u.site.dcFailedLogged {
			u.Log.ErrorEvent(logging.EventSourcePolicyFailed,
				"domain controller lookup failed, using the default server",
				"error", u.site.dcErr.Error(), "defaultServer", eff.Doc.DefaultServer)
			u.site.dcFailedLogged = true
		}
	} else {
		u.site.dcFailedLogged = false
	}

	key := site + "|" + strings.Join(chain, ",") + "|" + strings.Join(eff.Applied, ",")
	if key != u.site.lastKey {
		fields := []any{
			"site", site, "chain", describeChain(eff, chain),
			"policyRevision", snap.Revision(), "policySource", snap.Source.String(),
			"overrides", strings.Join(eff.Applied, ","),
		}
		if host.DC != "" {
			fields = append(fields, "dc", host.DC)
		}
		if len(host.IPs) > 0 {
			fields = append(fields, "localIPs", strings.Join(host.IPs, ","))
		}
		reason := "not at a mapped site, using defaultServer"
		if site != "" {
			reason = "domain controller and local address matched a mapped site"
		}
		u.Log.InfoEvent(logging.EventSourcePolicy, "update source decided", append([]any{"reason", reason}, fields...)...)
		u.site.lastKey = key
	}
	return cyc
}

// current returns the latest cycle state, building one from the current
// snapshot when no cycle has run yet (foreground helpers, tests).
func (u *Updater) current() *cycleState {
	if cyc := u.cur.Load(); cyc != nil {
		return cyc
	}
	snap := u.Policy.Current()
	host := policy.Host{HWID: u.Machine.HWID, Hostname: u.Machine.Hostname, Now: u.clock()}
	eff, _ := snap.Parsed.Effective(host)
	site, chain := eff.Chain(host)
	return &cycleState{snap: snap, host: host, eff: eff, site: site, chain: chain}
}
