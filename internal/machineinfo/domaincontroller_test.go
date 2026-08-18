package machineinfo

import "testing"

// TestNetapi32ProcsResolve guards against typo'd Win32 export names: lazy
// DLL procs only fail at Call() time (a panic, not a compile error), so a
// wrong name silently breaks NearestDomainController's cleanup path while
// still returning a resolved DC (see NetApiBufFree/NetApiBufferFree).
// Find() only loads netapi32.dll and looks the exports up — no domain, no
// network and no admin rights needed, so it is safe in CI.
func TestNetapi32ProcsResolve(t *testing.T) {
	if err := procDsGetDcNameW.Find(); err != nil {
		t.Errorf("DsGetDcNameW not found in netapi32.dll: %v", err)
	}
	if err := procNetApiBufferFree.Find(); err != nil {
		t.Errorf("NetApiBufferFree not found in netapi32.dll: %v", err)
	}
}

// TestTrimUNCPrefix pins down that BOTH leading backslashes go: DsGetDcName
// returns "\\\\DC-RM2.tregcc.local", and stripping just one leaves a
// name that fails silently wherever it is used.
func TestTrimUNCPrefix(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"dns name", "\\\\DC-RM2.tregcc.local", "DC-RM2.tregcc.local"},
		{"netbios name", "\\\\DC-RM2", "DC-RM2"},
		{"already trimmed", "DC-RM2.tregcc.local", "DC-RM2.tregcc.local"},
		{"single backslash", "\\DC-RM2", "\\DC-RM2"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := trimUNCPrefix(c.in); got != c.want {
				t.Errorf("trimUNCPrefix(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
