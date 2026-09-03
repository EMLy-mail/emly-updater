package machineinfo

import "testing"

func TestNormalizeUUID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"wmic value", "36CC511A-F0DE-EA11-8106-842AFDCE34D0", "36CC511A-F0DE-EA11-8106-842AFDCE34D0"},
		{"padded", "  36CC511A-F0DE-EA11-8106-842AFDCE34D0  \r\n", "36CC511A-F0DE-EA11-8106-842AFDCE34D0"},
		{"lower-cased", "36cc511a-f0de-ea11-8106-842afdce34d0", "36CC511A-F0DE-EA11-8106-842AFDCE34D0"},
		{"empty", "", ""},
		{"whitespace only", "  \r\n", ""},
		{"all zeroes", "00000000-0000-0000-0000-000000000000", ""},
		{"all Fs", "FFFFFFFF-FFFF-FFFF-FFFF-FFFFFFFFFFFF", ""},
		{"all Fs lower-cased", "ffffffff-ffff-ffff-ffff-ffffffffffff", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeUUID(c.in); got != c.want {
				t.Errorf("normalizeUUID(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestParseWMICUUID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"real output",
			"UUID                                  \r\r\n36CC511A-F0DE-EA11-8106-842AFDCE34D0  \r\r\n\r\r\n",
			"36CC511A-F0DE-EA11-8106-842AFDCE34D0",
		},
		{"header only", "UUID  \r\r\n\r\r\n", ""},
		{"empty", "", ""},
		{"placeholder value", "UUID\r\r\n00000000-0000-0000-0000-000000000000\r\r\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseWMICUUID(c.in); got != c.want {
				t.Errorf("parseWMICUUID(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestDomainJoined(t *testing.T) {
	cases := []struct {
		name     string
		adDomain string
		hostname string
		want     bool
	}{
		{"empty", "", "WKS01", false},
		{"whitespace only", "   ", "WKS01", false},
		{"workgroup", "WORKGROUP", "WKS01", false},
		{"workgroup case-insensitive", "workgroup", "WKS01", false},
		{"equals hostname", "WKS01", "WKS01", false},
		{"equals hostname case-insensitive", "wks01", "WKS01", false},
		{"real domain", "corp.example.com", "WKS01", true},
		{"real domain case-insensitive host", "CORP.EXAMPLE.COM", "wks01", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DomainJoined(c.adDomain, c.hostname); got != c.want {
				t.Errorf("DomainJoined(%q, %q) = %v, want %v", c.adDomain, c.hostname, got, c.want)
			}
		})
	}
}
