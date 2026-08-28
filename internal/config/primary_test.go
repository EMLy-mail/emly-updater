package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sampleINI keeps the shape that matters for the line surgery: aligned keys,
// Italian comments, a commented-out decoy `primary` line, and a same-named key
// in another section that must not be touched.
const sampleINI = "[updater]\r\n" +
	"; La chiave qui sotto ha lo stesso nome ma vive in un'altra sezione.\r\n" +
	"primary              = never-touch-me\r\n" +
	"\r\n" +
	"[source]\r\n" +
	"; Sorgente primaria per il manifest degli aggiornamenti.\r\n" +
	"; primary              = external\r\n" +
	"primary              = internal\r\n" +
	"\r\n" +
	"; URL del manifest usato quando primary = external.\r\n" +
	"externalManifestURL  = https://api.emly.ffois.it/v2/updates/manifest\r\n"

func TestSetPrimaryInRewritesOnlyTheSourceKey(t *testing.T) {
	got, err := setPrimaryIn(sampleINI, SourceExternal)
	if err != nil {
		t.Fatalf("setPrimaryIn: %v", err)
	}

	want := strings.Replace(sampleINI,
		"primary              = internal",
		"primary              = external", 1)
	if got != want {
		t.Errorf("setPrimaryIn = %q, want %q", got, want)
	}
	if !strings.Contains(got, "primary              = never-touch-me") {
		t.Error("the [updater] key with the same name was rewritten")
	}
	if !strings.Contains(got, "; primary              = external\r\n") {
		t.Error("the commented-out decoy line was rewritten")
	}
	if strings.Count(got, "\r\n") != strings.Count(sampleINI, "\r\n") {
		t.Error("CRLF line endings were not preserved")
	}
}

func TestSetPrimaryInPreservesLFEndings(t *testing.T) {
	lf := strings.ReplaceAll(sampleINI, "\r\n", "\n")
	got, err := setPrimaryIn(lf, SourceExternal)
	if err != nil {
		t.Fatalf("setPrimaryIn: %v", err)
	}
	if strings.Contains(got, "\r") {
		t.Error("an LF file came back with CR characters in it")
	}
}

// A [source] section without the key at all is a legal config (Load defaults
// it), so the key has to be created rather than silently dropped.
func TestSetPrimaryInInsertsMissingKey(t *testing.T) {
	got, err := setPrimaryIn("[source]\r\nuserAgent = x\r\n", SourceInternal)
	if err != nil {
		t.Fatalf("setPrimaryIn: %v", err)
	}
	want := "[source]\r\nprimary = internal\r\nuserAgent = x\r\n"
	if got != want {
		t.Errorf("setPrimaryIn = %q, want %q", got, want)
	}
}

func TestSetPrimaryInWithoutSourceSection(t *testing.T) {
	if _, err := setPrimaryIn("[updater]\r\nprimary = internal\r\n", SourceExternal); err == nil {
		t.Error("expected an error for a file with no [source] section")
	}
}

// The written file must survive a round trip through Load: this is the
// guarantee the startup source policy leans on when it persists a switch.
func TestSetPrimaryRoundTripsThroughLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.ini")
	if _, err := Load(path); err != nil { // seeds the embedded defaults
		t.Fatalf("seeding config: %v", err)
	}

	if err := SetPrimary(path, SourceExternal); err != nil {
		t.Fatalf("SetPrimary: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load after SetPrimary: %v", err)
	}
	if cfg.Primary != SourceExternal {
		t.Errorf("Primary after round trip = %q, want %q", cfg.Primary, SourceExternal)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "; Sorgente primaria per il manifest") {
		t.Error("the Italian comments did not survive the rewrite")
	}
}

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

// A malformed internalDCSubnets must stop Load rather than quietly disabling
// the policy: a typo'd subnet would otherwise push the whole fleet external.
func TestLoadRejectsMalformedSubnets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.ini")
	if _, err := Load(path); err != nil {
		t.Fatalf("seeding config: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	broken := strings.Replace(string(raw), "internalDCSubnets    = 172.16.96.0/24",
		"internalDCSubnets    = 172.16.96.0/33", 1)
	if broken == string(raw) {
		t.Fatal("could not find internalDCSubnets in the seeded default config")
	}
	if err := os.WriteFile(path, []byte(broken), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("Load accepted a malformed CIDR block")
	}
}
