package machineinfo

import "testing"

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
