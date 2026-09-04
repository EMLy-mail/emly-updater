package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSubnets(t *testing.T) {
	if got, err := ParseSubnets("  "); err != nil || got != nil {
		t.Errorf("ParseSubnets(blank) = %v, %v; want nil, nil", got, err)
	}
	got, err := ParseSubnets("172.16.96.0/24, 10.12.8.0/24")
	if err != nil {
		t.Fatalf("ParseSubnets: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("parsed %d subnets, want 2", len(got))
	}
	if _, err := ParseSubnets("172.16.96.0/24, nonsense"); err == nil {
		t.Error("expected an error for a malformed CIDR block")
	}
}

func TestSubnetsContain(t *testing.T) {
	subnets, err := ParseSubnets("172.16.96.0/24")
	if err != nil {
		t.Fatalf("ParseSubnets: %v", err)
	}
	cases := []struct {
		ip   string
		want bool
	}{
		{"172.16.96.50", true},
		{" 172.16.96.50 ", true},
		{"172.16.97.50", false},
		{"10.12.8.253", false},
		{"DC-RM2", false},
		{"", false},
	}
	for _, c := range cases {
		if got := SubnetsContain(subnets, c.ip); got != c.want {
			t.Errorf("SubnetsContain(%q) = %v, want %v", c.ip, got, c.want)
		}
	}
}

// A malformed defaultMappingDCSubnets must stop Load rather than quietly
// disabling the policy: a typo'd subnet would otherwise push the whole fleet
// external.
func TestLoadRejectsMalformedSubnets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.ini")
	if _, err := Load(path); err != nil {
		t.Fatalf("seeding config: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	broken := strings.Replace(string(raw), "defaultMappingDCSubnets = DC-RM2:172.16.96.0/24",
		"defaultMappingDCSubnets = DC-RM2:172.16.96.0/33", 1)
	if broken == string(raw) {
		t.Fatal("could not find defaultMappingDCSubnets in the seeded default config")
	}
	if err := os.WriteFile(path, []byte(broken), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("Load accepted a malformed CIDR block")
	}
}

func TestParseDCSubnetMap(t *testing.T) {
	if got, err := ParseDCSubnetMap("  "); err != nil || got != nil {
		t.Errorf("ParseDCSubnetMap(blank) = %v, %v; want nil, nil", got, err)
	}

	got, err := ParseDCSubnetMap("DC-RM2:172.16.96.0/24,10.12.8.0/24|DC-CB:172.16.33.0/24")
	if err != nil {
		t.Fatalf("ParseDCSubnetMap: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("parsed %d DCs, want 2", len(got))
	}
	if len(got["DC-RM2"]) != 2 {
		t.Errorf("DC-RM2 subnets = %v, want 2 entries", got["DC-RM2"])
	}
	if len(got["DC-CB"]) != 1 {
		t.Errorf("DC-CB subnets = %v, want 1 entry", got["DC-CB"])
	}

	// The ';' delimiter written by previous releases must keep parsing, or a
	// config carried over from an old install would fail Load and take the
	// service down.
	legacy, err := ParseDCSubnetMap("DC-RM2:172.16.96.0/24;DC-CB:172.16.33.0/24")
	if err != nil {
		t.Fatalf("ParseDCSubnetMap(legacy ';'): %v", err)
	}
	if len(legacy) != 2 {
		t.Errorf("legacy ';' delimiter parsed %d DCs, want 2", len(legacy))
	}

	if _, err := ParseDCSubnetMap("DC-RM2 172.16.96.0/24"); err == nil {
		t.Error("expected an error for an entry missing the ':' separator")
	}
	if _, err := ParseDCSubnetMap("DC-RM2:nonsense"); err == nil {
		t.Error("expected an error for a malformed CIDR block")
	}
	if _, err := ParseDCSubnetMap("DC-RM2:172.16.96.0/24|dc-rm2:172.16.97.0/24"); err == nil {
		t.Error("expected an error for a DC name repeated (case-insensitively)")
	}
}
