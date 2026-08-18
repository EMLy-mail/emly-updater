package cert

import "golang.org/x/sys/windows"

// storeNames are the two stores a self-signed code-signing certificate has to
// occupy. It is an end-entity certificate, not a CA, so its chain is one
// element long and terminates at itself: Root is what lets that chain
// validate, TrustedPublisher is what makes the publisher trusted. Installing
// into only one of them does not produce a verified-publisher UAC prompt.
var storeNames = []string{"Root", "TrustedPublisher"}

// Target identifies one Windows certificate store.
type Target struct {
	Location uint32 // CERT_SYSTEM_STORE_LOCAL_MACHINE or CERT_SYSTEM_STORE_USERS
	SID      string // required when Location is CERT_SYSTEM_STORE_USERS, empty otherwise
	Name     string // "Root" or "TrustedPublisher"
}

// StoreName is the store name CertOpenStore expects. A store belonging to
// another user is addressed as "<SID>\<store>"; machine stores by bare name.
func (t Target) StoreName() string {
	if t.Location == windows.CERT_SYSTEM_STORE_USERS {
		return t.SID + `\` + t.Name
	}
	return t.Name
}

// String is a stable, human-readable label for logs and Event Log entries.
func (t Target) String() string {
	if t.Location == windows.CERT_SYSTEM_STORE_USERS {
		return `User\` + t.Name + ` (` + t.SID + `)`
	}
	return `LocalMachine\` + t.Name
}

// openFlags is the flags word for CertOpenStore.
//
// Per-user stores add CERT_SYSTEM_STORE_UNPROTECTED_FLAG. The user Root store
// is a "protected root": adding to it through the ordinary path is designed to
// raise an interactive confirmation dialog, and session 0 - where this service
// lives - has no desktop to draw one on. The flag writes the underlying store
// directly and bypasses that machinery. The machine Root store is gated by
// administrator rights instead of by a prompt, so it needs no such flag.
func (t Target) openFlags() uint32 {
	if t.Location == windows.CERT_SYSTEM_STORE_USERS {
		return t.Location | windows.CERT_SYSTEM_STORE_UNPROTECTED_FLAG
	}
	return t.Location
}

// MachineTargets returns the two LocalMachine stores. These cover UAC for
// every user of the machine.
func MachineTargets() []Target {
	out := make([]Target, 0, len(storeNames))
	for _, n := range storeNames {
		out = append(out, Target{Location: windows.CERT_SYSTEM_STORE_LOCAL_MACHINE, Name: n})
	}
	return out
}

// UserTargets returns the two per-user stores belonging to sid. Reaching them
// requires that user's registry hive to be loaded under HKU, which holds while
// they are logged on.
func UserTargets(sid string) []Target {
	out := make([]Target, 0, len(storeNames))
	for _, n := range storeNames {
		out = append(out, Target{Location: windows.CERT_SYSTEM_STORE_USERS, SID: sid, Name: n})
	}
	return out
}
