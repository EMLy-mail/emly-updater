package machineinfo

import (
	"encoding/binary"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

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
// happens to run in: no panic, a user name that can travel in a header
// untouched, and a state that is consistent with it. Which user it returns
// depends on who is logged on to the machine running the suite, so that is
// not asserted.
func TestLoggedUserIsHeaderSafe(t *testing.T) {
	got := LoggedUser()
	if got.User != sanitizeHeaderValue(got.User) {
		t.Errorf("LoggedUser().User = %q, which is not a valid header value", got.User)
	}
	if (got.User == "") != (got.State == "") {
		t.Errorf("LoggedUser() = %+v: user and state must be set together", got)
	}
	if got.State != SessionDisconnected && !got.DisconnectedAt.IsZero() {
		t.Errorf("LoggedUser() = %+v: DisconnectedAt set on a session that is not disconnected", got)
	}
}

func TestPickSession(t *testing.T) {
	const console = 1
	cases := []struct {
		name      string
		entries   []sessionEntry
		console   uint32
		wantUser  string
		wantState SessionState
	}{
		{
			// qwinsta on CB331: services (0, Disc), console at the login
			// screen (1, Conn), bera2 left behind by a closed RDP window
			// (2, Disc), the RDP listener (65536, Listen).
			name: "RDP session left disconnected, console at login screen",
			entries: []sessionEntry{
				{ID: 0, State: windows.WTSDisconnected},
				{ID: 1, State: windows.WTSConnected},
				{ID: 2, State: windows.WTSDisconnected, User: `CONTOSO\bera2`},
				{ID: 65536, State: windows.WTSListen},
			},
			console:   console,
			wantUser:  `CONTOSO\bera2`,
			wantState: SessionDisconnected,
		},
		{
			name: "user at the console",
			entries: []sessionEntry{
				{ID: 0, State: windows.WTSDisconnected},
				{ID: 1, State: windows.WTSActive, User: `CONTOSO\mario.rossi`},
			},
			console:   console,
			wantUser:  `CONTOSO\mario.rossi`,
			wantState: SessionActiveConsole,
		},
		{
			name: "attached RDP client beats a disconnected session",
			entries: []sessionEntry{
				{ID: 1, State: windows.WTSConnected},
				{ID: 2, State: windows.WTSDisconnected, User: `CONTOSO\bera2`},
				{ID: 3, State: windows.WTSActive, User: `CONTOSO\admin`},
			},
			console:   console,
			wantUser:  `CONTOSO\admin`,
			wantState: SessionActiveRDP,
		},
		{
			name: "console beats an attached RDP client",
			entries: []sessionEntry{
				{ID: 3, State: windows.WTSActive, User: `CONTOSO\admin`},
				{ID: 1, State: windows.WTSActive, User: `CONTOSO\mario.rossi`},
			},
			console:   console,
			wantUser:  `CONTOSO\mario.rossi`,
			wantState: SessionActiveConsole,
		},
		{
			name: "no console session at all makes every active session remote",
			entries: []sessionEntry{
				{ID: 1, State: windows.WTSActive, User: `CONTOSO\mario.rossi`},
			},
			console:   noActiveConsole,
			wantUser:  `CONTOSO\mario.rossi`,
			wantState: SessionActiveRDP,
		},
		{
			name: "nobody logged on",
			entries: []sessionEntry{
				{ID: 0, State: windows.WTSDisconnected},
				{ID: 1, State: windows.WTSConnected},
				{ID: 65536, State: windows.WTSListen},
			},
			console: console,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, state, ok := pickSession(c.entries, c.console)
			if ok != (c.wantUser != "") {
				t.Fatalf("pickSession ok = %v, want %v", ok, c.wantUser != "")
			}
			if got.User != c.wantUser || state != c.wantState {
				t.Errorf("pickSession = (%q, %q), want (%q, %q)", got.User, state, c.wantUser, c.wantState)
			}
		})
	}
}

func TestParseSessionInfoEx(t *testing.T) {
	disconnect := time.Date(2026, 9, 12, 18, 4, 31, 0, time.UTC)
	current := time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC)

	buf := make([]byte, 232) // sizeof(WTSINFOEXW)
	binary.LittleEndian.PutUint32(buf, wtsInfoExLevel1)
	binary.LittleEndian.PutUint64(buf[wtsInfoExDisconnectOff:], uint64(toFileTime(disconnect)))
	binary.LittleEndian.PutUint64(buf[wtsInfoExCurrentOff:], uint64(toFileTime(current)))

	got, ok := parseSessionInfoEx(buf)
	if !ok {
		t.Fatal("parseSessionInfoEx rejected a well-formed buffer")
	}
	if !got.Disconnect.Equal(disconnect) || !got.Current.Equal(current) {
		t.Errorf("parseSessionInfoEx = %+v, want disconnect %v current %v", got, disconnect, current)
	}

	// Never disconnected: Windows leaves the field at zero.
	binary.LittleEndian.PutUint64(buf[wtsInfoExDisconnectOff:], 0)
	if got, _ := parseSessionInfoEx(buf); !got.Disconnect.IsZero() {
		t.Errorf("zero DisconnectTime decoded as %v, want the zero time", got.Disconnect)
	}

	if _, ok := parseSessionInfoEx(buf[:100]); ok {
		t.Error("parseSessionInfoEx accepted a truncated buffer")
	}
	binary.LittleEndian.PutUint32(buf, 2)
	if _, ok := parseSessionInfoEx(buf); ok {
		t.Error("parseSessionInfoEx accepted an unknown level")
	}
}

// The offsets in parseSessionInfoEx come from reading wtsapi32.h, not from
// the compiler, so check them against the real API: the record's CurrentTime
// is the machine's clock, and landing near time.Now() is only possible if the
// layout is right. Skipped where the query is unavailable (a CI runner with
// no WTS session to ask about).
func TestSessionInfoExLayoutMatchesWindows(t *testing.T) {
	var session uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session); err != nil {
		t.Skipf("ProcessIdToSessionId: %v", err)
	}
	got, ok := querySessionTimes(session)
	if !ok {
		t.Skip("WTSSessionInfoEx unavailable for this session")
	}
	if d := time.Since(got.Current); d < -time.Minute || d > time.Minute {
		t.Errorf("CurrentTime = %v, %v away from now: WTSINFOEX_LEVEL1 offsets are wrong", got.Current, d)
	}
}

// toFileTime is the inverse of fileTimeToTime: 100 ns ticks since 1601-01-01,
// the Unix epoch being 116444736000000000 ticks after that.
func toFileTime(t time.Time) int64 {
	return t.UnixNano()/100 + 116444736000000000
}
