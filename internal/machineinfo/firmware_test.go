package machineinfo

import "testing"

func TestNormalizeFirmwareValue(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"hp product number keeps its case and suffix", "8AP52EA#ABZ", "8AP52EA#ABZ"},
		{"serial padded by the shell", "  5CD1234ABC \r\n", "5CD1234ABC"},
		{"oem placeholder", "To Be Filled By O.E.M.", ""},
		{"oem placeholder, other casing", "to be filled by o.e.m.", ""},
		{"default string", "Default string", ""},
		{"system serial number", "System Serial Number", ""},
		{"zero", "0", ""},
		{"empty", "", ""},
		{"whitespace only", "   \r\n", ""},
		{"control characters stripped", "5CD1\x00234", "5CD1234"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeFirmwareValue(c.in); got != c.want {
				t.Errorf("normalizeFirmwareValue(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
