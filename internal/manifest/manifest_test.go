package manifest

import "testing"

const sampleJSON = `{
  "stableVersion": "1.7.3",
  "betaVersion": "1.7.5",
  "stableDownload": "https://api.example/releases/1.7.3/download",
  "betaDownload": "https://api.example/releases/1.7.5/download",
  "isCritical": false,
  "minRequiredVersion": "1.7.0",
  "sha256Checksums": {
    "1.7.3": "aaa",
    "1.7.5": "bbb"
  }
}`

func TestParseWithBOM(t *testing.T) {
	data := append([]byte("\xEF\xBB\xBF"), []byte(sampleJSON)...)
	m, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse with BOM failed: %v", err)
	}
	if m.StableVersion != "1.7.3" || m.BetaVersion != "1.7.5" {
		t.Fatalf("unexpected versions: %+v", m)
	}
}

func TestParseRejectsIncomplete(t *testing.T) {
	if _, err := Parse([]byte(`{"betaVersion":"1.0.0"}`)); err == nil {
		t.Fatal("expected error for manifest without stable version/download")
	}
	if _, err := Parse([]byte(`not json`)); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestChannelVersion(t *testing.T) {
	m, err := Parse([]byte(sampleJSON))
	if err != nil {
		t.Fatal(err)
	}

	v, ref, err := m.ChannelVersion("beta")
	if err != nil || v != "1.7.5" || ref != "https://api.example/releases/1.7.5/download" {
		t.Fatalf("beta resolution wrong: %s %s %v", v, ref, err)
	}

	// Anything that is not "beta" resolves to stable.
	for _, channel := range []string{"stable", "", "weird"} {
		v, _, err := m.ChannelVersion(channel)
		if err != nil || v != "1.7.3" {
			t.Fatalf("channel %q: expected stable 1.7.3, got %s (%v)", channel, v, err)
		}
	}
}

func TestChannelVersionMissingBeta(t *testing.T) {
	m := &Manifest{StableVersion: "1.0.0", StableDownload: "x"}
	if _, _, err := m.ChannelVersion("beta"); err == nil {
		t.Fatal("expected error when beta target is missing")
	}
}

func TestLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.7.4", "1.7.5", true},
		{"1.7.5", "1.7.5", false},
		{"1.7.10", "1.7.9", false}, // numeric, not lexicographic
		{"0.0.0", "1.0.0", true},   // fresh-install sentinel
		{"1.8.0", "1.7.5", false},
	}
	for _, c := range cases {
		got, err := Less(c.a, c.b)
		if err != nil {
			t.Fatalf("Less(%s,%s): %v", c.a, c.b, err)
		}
		if got != c.want {
			t.Errorf("Less(%s,%s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}

	if _, err := Less("garbage", "1.0.0"); err == nil {
		t.Fatal("expected error for invalid version")
	}
}

func TestForced(t *testing.T) {
	m := &Manifest{IsCritical: true}
	if forced, _ := m.Forced("1.7.5"); !forced {
		t.Fatal("isCritical must force")
	}

	m = &Manifest{MinRequiredVersion: "1.7.0"}
	if forced, _ := m.Forced("1.6.9"); !forced {
		t.Fatal("below minRequiredVersion must force")
	}
	if forced, _ := m.Forced("1.7.0"); forced {
		t.Fatal("at minRequiredVersion must not force")
	}

	// Legacy UNC manifests have no minRequiredVersion at all.
	m = &Manifest{}
	if forced, _ := m.Forced("0.0.1"); forced {
		t.Fatal("empty minRequiredVersion must not force")
	}
}
