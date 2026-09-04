package config

import (
	"fmt"
	"net"
	"strings"
)

// ParseSubnets parses a comma-separated list of CIDR blocks (one DC's share
// of the defaultMappingDCSubnets key). An empty or blank string yields no
// subnets. A malformed entry is an error: silently ignoring it would leave
// every machine on that site's "not internal" branch with nothing in the log
// to explain why.
func ParseSubnets(raw string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for field := range strings.SplitSeq(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		_, n, err := net.ParseCIDR(field)
		if err != nil {
			return nil, fmt.Errorf("%q is not a valid CIDR block: %w", field, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// ParseDCSubnetMap parses the defaultMappingDCSubnets key: a '|'-separated
// list of "<dcName>:<cidr>[,<cidr>...]" entries, one per site's domain
// controller (e.g. "DC-RM2:172.16.96.0/24,10.12.8.0/24|DC-CB:172.16.33.0/24").
// The old ';' delimiter is still accepted so a config written by a previous
// release keeps parsing: rejecting it would take the service down on the
// first start after an update.
// An empty or blank string yields a nil map, which disables the startup
// source policy rather than being an error. A malformed entry - a missing
// "dc:cidrs" separator, an invalid CIDR, or a DC name listed more than once -
// is an error: silently ignoring it would leave that site's machines on the
// "not internal" branch with nothing in the log to explain why.
func ParseDCSubnetMap(raw string) (map[string][]*net.IPNet, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	out := make(map[string][]*net.IPNet)
	seen := make(map[string]bool)
	for entry := range strings.SplitSeq(strings.ReplaceAll(raw, ";", "|"), "|") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		dc, cidrs, ok := strings.Cut(entry, ":")
		dc = strings.TrimSpace(dc)
		if !ok || dc == "" || strings.TrimSpace(cidrs) == "" {
			return nil, fmt.Errorf("defaultMappingDCSubnets: %q is not in \"dc:cidr[,cidr...]\" form", entry)
		}

		lower := strings.ToLower(dc)
		if seen[lower] {
			return nil, fmt.Errorf("defaultMappingDCSubnets: domain controller %q is listed more than once", dc)
		}
		seen[lower] = true

		subnets, err := ParseSubnets(cidrs)
		if err != nil {
			return nil, fmt.Errorf("defaultMappingDCSubnets: domain controller %q: %w", dc, err)
		}
		out[dc] = subnets
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// SubnetsContain reports whether ip (as text) falls inside any of subnets.
// A malformed or non-IP string is never contained.
func SubnetsContain(subnets []*net.IPNet, ip string) bool {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return false
	}
	for _, n := range subnets {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}
