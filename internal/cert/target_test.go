package cert

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestStoreName(t *testing.T) {
	tests := []struct {
		name   string
		target Target
		want   string
	}{
		{
			name:   "machine store uses the bare name",
			target: Target{Location: windows.CERT_SYSTEM_STORE_LOCAL_MACHINE, Name: "Root"},
			want:   "Root",
		},
		{
			name: "user store is prefixed with the SID",
			target: Target{
				Location: windows.CERT_SYSTEM_STORE_USERS,
				SID:      "S-1-5-21-1111111111-2222222222-3333333333-1001",
				Name:     "TrustedPublisher",
			},
			want: `S-1-5-21-1111111111-2222222222-3333333333-1001\TrustedPublisher`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.target.StoreName(); got != tc.want {
				t.Errorf("StoreName() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMachineTargetsCoverBothStores(t *testing.T) {
	targets := MachineTargets()
	if len(targets) != 2 {
		t.Fatalf("MachineTargets() returned %d targets, want 2", len(targets))
	}
	seen := map[string]bool{}
	for _, tg := range targets {
		if tg.Location != windows.CERT_SYSTEM_STORE_LOCAL_MACHINE {
			t.Errorf("target %s has location %#x, want CERT_SYSTEM_STORE_LOCAL_MACHINE",
				tg.Name, tg.Location)
		}
		if tg.SID != "" {
			t.Errorf("machine target %s must not carry a SID, got %q", tg.Name, tg.SID)
		}
		seen[tg.Name] = true
	}
	// Both are required: Root closes the one-element chain of a self-signed
	// certificate, TrustedPublisher makes the publisher trusted. Neither
	// alone produces a verified-publisher UAC prompt.
	for _, want := range []string{"Root", "TrustedPublisher"} {
		if !seen[want] {
			t.Errorf("MachineTargets() is missing the %s store", want)
		}
	}
}

func TestUserTargetsCarryTheSID(t *testing.T) {
	const sid = "S-1-5-21-1111111111-2222222222-3333333333-1001"
	targets := UserTargets(sid)
	if len(targets) != 2 {
		t.Fatalf("UserTargets() returned %d targets, want 2", len(targets))
	}
	for _, tg := range targets {
		if tg.Location != windows.CERT_SYSTEM_STORE_USERS {
			t.Errorf("target %s has location %#x, want CERT_SYSTEM_STORE_USERS",
				tg.Name, tg.Location)
		}
		if tg.SID != sid {
			t.Errorf("target %s has SID %q, want %q", tg.Name, tg.SID, sid)
		}
		if !strings.HasPrefix(tg.StoreName(), sid+`\`) {
			t.Errorf("StoreName() = %q, want it prefixed with %q", tg.StoreName(), sid+`\`)
		}
	}
}

func TestUserTargetsAreUnprotected(t *testing.T) {
	// The per-user Root store is a protected root: adding to it through the
	// ordinary path is meant to raise an interactive confirmation dialog, and
	// session 0 has no desktop to draw one on. The flag is mandatory.
	for _, tg := range UserTargets("S-1-5-21-1-2-3-1001") {
		if tg.openFlags()&windows.CERT_SYSTEM_STORE_UNPROTECTED_FLAG == 0 {
			t.Errorf("user target %s must set CERT_SYSTEM_STORE_UNPROTECTED_FLAG", tg.Name)
		}
		if tg.openFlags()&windows.CERT_SYSTEM_STORE_USERS == 0 {
			t.Errorf("user target %s lost its location bits", tg.Name)
		}
	}
}

func TestMachineTargetsAreNotUnprotected(t *testing.T) {
	// The machine Root store is gated by administrator rights, not by an
	// interactive prompt, so the flag is unnecessary there.
	for _, tg := range MachineTargets() {
		if tg.openFlags() != windows.CERT_SYSTEM_STORE_LOCAL_MACHINE {
			t.Errorf("machine target %s openFlags() = %#x, want %#x",
				tg.Name, tg.openFlags(), uint32(windows.CERT_SYSTEM_STORE_LOCAL_MACHINE))
		}
	}
}

func TestStringIsReadable(t *testing.T) {
	machine := Target{Location: windows.CERT_SYSTEM_STORE_LOCAL_MACHINE, Name: "Root"}
	if got := machine.String(); got != `LocalMachine\Root` {
		t.Errorf("String() = %q, want %q", got, `LocalMachine\Root`)
	}
	user := Target{
		Location: windows.CERT_SYSTEM_STORE_USERS,
		SID:      "S-1-5-21-1-2-3-1001",
		Name:     "Root",
	}
	if got := user.String(); !strings.Contains(got, "S-1-5-21-1-2-3-1001") ||
		!strings.Contains(got, "Root") {
		t.Errorf("String() = %q, want it to name both the SID and the store", got)
	}
}
