package machineinfo

import "testing"

func TestSanitizeHeaderValue(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", `CONTOSO\mario.rossi`, `CONTOSO\mario.rossi`},
		{"accented name survives", `CONTOSO\niccolò`, `CONTOSO\niccolò`},
		{"embedded CRLF dropped", "CONTOSO\r\nX-Evil: 1", "CONTOSOX-Evil: 1"},
		{"tab dropped", "CONTOSO\tmario", "CONTOSOmario"},
		{"DEL dropped", "mario\x7f", "mario"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeHeaderValue(c.in); got != c.want {
				t.Errorf("sanitizeHeaderValue(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// LoggedUser must be safe to call from a test process in whatever session it
// happens to run in: no panic, and either an empty string or a value that can
// travel in a header untouched. Which user it returns depends on who is
// logged on to the machine running the suite, so that is not asserted.
func TestLoggedUserIsHeaderSafe(t *testing.T) {
	got := LoggedUser()
	if got != sanitizeHeaderValue(got) {
		t.Errorf("LoggedUser() = %q, which is not a valid header value", got)
	}
}
