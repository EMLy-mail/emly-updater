package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"emlyupdater/internal/config"
	"emlyupdater/internal/logging"
	"emlyupdater/internal/machineinfo"
)

// internalCfg mirrors a real deployed config: two office DCs, each with its
// own subnets, and both manifest URLs populated so either source is
// selectable.
func internalCfg(t *testing.T, primary string) *config.Config {
	t.Helper()
	dcSubnets, err := config.ParseDCSubnetMap("DC-RM2:172.16.96.0/24;DC-CB:172.16.33.0/24,172.16.34.0/24")
	if err != nil {
		t.Fatalf("ParseDCSubnetMap: %v", err)
	}
	return &config.Config{
		Primary:             primary,
		ExternalManifestURL: "https://api.emly.ffois.it/v2/updates/manifest",
		InternalManifestURL: "http://172.16.96.73:8080/v2/updates/manifest",
		DCSubnetMap:         dcSubnets,
	}
}

func TestDecideSource(t *testing.T) {
	cases := []struct {
		name       string
		dc         *machineinfo.DomainControllerInfo
		dcErr      error
		localIPs   []string
		ipsErr     error
		want       string
		reasonHint string
	}{
		{
			name:     "office DC, DNS name, matching local IP",
			dc:       &machineinfo.DomainControllerInfo{Name: "DC-RM2.tregcc.local", Site: "RM2"},
			localIPs: []string{"172.16.96.50"},
			want:     config.SourceInternal,
		},
		{
			name:     "office DC, NetBIOS name, matching local IP",
			dc:       &machineinfo.DomainControllerInfo{Name: "DC-RM2", Site: "RM2"},
			localIPs: []string{"172.16.96.50"},
			want:     config.SourceInternal,
		},
		{
			name:     "second site's DC and subnet",
			dc:       &machineinfo.DomainControllerInfo{Name: "DC-CB.tregcc.local", Site: "CB"},
			localIPs: []string{"172.16.34.10"},
			want:     config.SourceInternal,
		},
		{
			name:     "machine has multiple local IPs, only one matches",
			dc:       &machineinfo.DomainControllerInfo{Name: "DC-RM2", Site: "RM2"},
			localIPs: []string{"10.0.0.5", "172.16.96.50"},
			want:     config.SourceInternal,
		},
		{
			name:       "office DC seen, but no local IP on its subnets",
			dc:         &machineinfo.DomainControllerInfo{Name: "DC-RM2.tregcc.local", Site: "RM2"},
			localIPs:   []string{"10.12.8.253"},
			want:       config.SourceExternal,
			reasonHint: "no local IP is on its mapped subnets",
		},
		{
			name:       "a domain controller with no configured mapping",
			dc:         &machineinfo.DomainControllerInfo{Name: "DC-MI1.tregcc.local", Site: "MI1"},
			localIPs:   []string{"172.16.96.50"},
			want:       config.SourceExternal,
			reasonHint: "no configured subnet mapping",
		},
		{
			name:       "machine off the domain",
			dcErr:      errors.New("DsGetDcName failed: Il dominio specificato non esiste o è impossibile contattarlo."),
			want:       config.SourceExternal,
			reasonHint: "lookup failed",
		},
		{
			name:       "local IP enumeration fails",
			dc:         &machineinfo.DomainControllerInfo{Name: "DC-RM2", Site: "RM2"},
			ipsErr:     errors.New("net.Interfaces: access denied"),
			want:       config.SourceExternal,
			reasonHint: "could not enumerate local IP addresses",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := decideSource(internalCfg(t, config.SourceInternal), c.dc, c.dcErr, c.localIPs, c.ipsErr)
			if got != c.want {
				t.Errorf("decideSource() = %q, want %q (reason: %s)", got, c.want, reason)
			}
			if c.reasonHint != "" && !strings.Contains(reason, c.reasonHint) {
				t.Errorf("reason = %q, want it to mention %q", reason, c.reasonHint)
			}
		})
	}
}

// An empty mapping must leave Primary alone rather than defaulting every
// machine to one source: an unconfigured policy is not an instruction.
func TestDecideSourceDisabledWhenUnconfigured(t *testing.T) {
	cfg := internalCfg(t, config.SourceInternal)
	cfg.DCSubnetMap = nil
	dc := &machineinfo.DomainControllerInfo{Name: "DC-RM2"}
	if got, reason := decideSource(cfg, dc, nil, []string{"10.12.8.253"}, nil); got != "" {
		t.Errorf("decideSource() = %q, want no opinion (reason: %s)", got, reason)
	}
}

func TestSameDCName(t *testing.T) {
	cases := []struct {
		resolved, want string
		match          bool
	}{
		{"DC-RM2.tregcc.local", "DC-RM2", true},
		{"DC-RM2", "DC-RM2", true},
		{"dc-rm2.tregcc.local", "DC-RM2", true},
		{"DC-RM2", "dc-rm2.tregcc.local", true},
		{"DC-RM20", "DC-RM2", false},
		{"DC-MI1.tregcc.local", "DC-RM2", false},
		{"", "DC-RM2", false},
	}
	for _, c := range cases {
		if got := sameDCName(c.resolved, c.want); got != c.match {
			t.Errorf("sameDCName(%q, %q) = %v, want %v", c.resolved, c.want, got, c.match)
		}
	}
}

// A switch is only safe if the target source has a manifest URL: writing
// primary=external with an empty externalManifestURL produces a config that
// config.Load refuses on the next start, which would take the service down.
func TestApplySourcePolicyKeepsPrimaryWhenTargetURLMissing(t *testing.T) {
	cfg := internalCfg(t, config.SourceInternal)
	cfg.ExternalManifestURL = ""

	lookup := func(string) (*machineinfo.DomainControllerInfo, error) {
		return &machineinfo.DomainControllerInfo{Name: "DC-RM2"}, nil
	}
	ips := func() ([]string, error) { return []string{"10.12.8.253"}, nil }
	// An empty path would fail the write; the URL guard must return first.
	applySourcePolicy(cfg, testLogger(t), "", lookup, ips)

	if cfg.Primary != config.SourceInternal {
		t.Errorf("Primary = %q, want it kept at %q", cfg.Primary, config.SourceInternal)
	}
}

// testLogger writes to a throwaway directory and never attaches the Windows
// Event Log, so the *Event calls exercise their nil-event-source path.
//
// The log directory is not t.TempDir(): the rolling file sink keeps
// updater.log open for the life of the process, and Windows refuses to
// unlink an open file, so t.TempDir() cleanup would fail the test for a
// reason that has nothing to do with what it asserts.
func testLogger(t *testing.T) *logging.Logger {
	t.Helper()
	dir, err := os.MkdirTemp("", "emlyupdater-log-*")
	if err != nil {
		t.Fatalf("creating log dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return logging.New(dir, "", false)
}

const testConfigINI = `[source]
; Sorgente primaria per il manifest degli aggiornamenti.
primary              = internal

; URL del manifest usato quando primary = external.
externalManifestURL  = https://api.emly.ffois.it/v2/updates/manifest
defaultMappingDCSubnets = DC-RM2:172.16.96.0/24
`

// The office DC seen from a remote subnet must switch the source to external,
// both in memory (used by this run) and in the config file (used by the next).
func TestApplySourcePolicySwitchesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.ini")
	if err := os.WriteFile(path, []byte(testConfigINI), 0644); err != nil {
		t.Fatalf("seeding config: %v", err)
	}

	cfg := internalCfg(t, config.SourceInternal)
	lookup := func(string) (*machineinfo.DomainControllerInfo, error) {
		return &machineinfo.DomainControllerInfo{Name: "DC-RM2.tregcc.local", Site: "RM2"}, nil
	}
	ips := func() ([]string, error) { return []string{"10.12.8.253"}, nil }

	applySourcePolicy(cfg, testLogger(t), path, lookup, ips)

	if cfg.Primary != config.SourceExternal {
		t.Errorf("in-memory Primary = %q, want %q", cfg.Primary, config.SourceExternal)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading config back: %v", err)
	}
	want := strings.Replace(testConfigINI,
		"primary              = internal",
		"primary              = external", 1)
	if string(got) != want {
		t.Errorf("config file after switch = %q, want %q", got, want)
	}
}

// Being in the office with a config left on external must switch it back:
// the policy is bidirectional, not a one-way escape hatch.
func TestApplySourcePolicySwitchesBackToInternal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.ini")
	seed := strings.Replace(testConfigINI,
		"primary              = internal",
		"primary              = external", 1)
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatalf("seeding config: %v", err)
	}

	cfg := internalCfg(t, config.SourceExternal)
	lookup := func(string) (*machineinfo.DomainControllerInfo, error) {
		return &machineinfo.DomainControllerInfo{Name: "DC-RM2.tregcc.local", Site: "RM2"}, nil
	}
	ips := func() ([]string, error) { return []string{"172.16.96.50"}, nil }

	applySourcePolicy(cfg, testLogger(t), path, lookup, ips)

	if cfg.Primary != config.SourceInternal {
		t.Errorf("in-memory Primary = %q, want %q", cfg.Primary, config.SourceInternal)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading config back: %v", err)
	}
	if string(got) != testConfigINI {
		t.Errorf("config file after switch back = %q, want %q", got, testConfigINI)
	}
}

// A config file that cannot be written must not cost the run its decision:
// the in-memory override is what this cycle actually uses.
func TestApplySourcePolicyKeepsOverrideWhenWriteFails(t *testing.T) {
	cfg := internalCfg(t, config.SourceInternal)
	lookup := func(string) (*machineinfo.DomainControllerInfo, error) {
		return &machineinfo.DomainControllerInfo{Name: "DC-RM2"}, nil
	}
	ips := func() ([]string, error) { return []string{"10.12.8.253"}, nil }

	applySourcePolicy(cfg, testLogger(t), filepath.Join(t.TempDir(), "missing", "config.ini"), lookup, ips)

	if cfg.Primary != config.SourceExternal {
		t.Errorf("in-memory Primary = %q, want %q despite the failed write", cfg.Primary, config.SourceExternal)
	}
}
