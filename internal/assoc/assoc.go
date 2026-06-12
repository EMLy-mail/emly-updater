// Package assoc self-heals the machine-wide .eml/.msg file associations that
// EMLy's installer writes to HKLM\Software\Classes. The installer (running as
// SYSTEM via this service) normally maintains them; this is the backstop for
// drifted or deleted keys. UserChoice/default-handler manipulation is
// deliberately out of scope - no other app claims these extensions on the
// target machines.
package assoc

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	shell32            = windows.NewLazySystemDLL("shell32.dll")
	procSHChangeNotify = shell32.NewProc("SHChangeNotify")
)

const (
	shcneAssocChanged = 0x08000000 // SHCNE_ASSOCCHANGED
	shcnfIDList       = 0x0000     // SHCNF_IDLIST
)

// Mapping ties one extension to its ProgID and display name, mirroring the
// [Registry] section of EMLy's installer.iss.
type Mapping struct {
	Ext         string // ".eml"
	ProgID      string // "EMLy.EML"
	DisplayName string // "EMLy Email Message"
}

// Repair verifies and, where needed, rewrites the HKLM association keys for
// the given mappings so they point at exePath. When anything was changed it
// notifies Explorer via SHChangeNotify so icons refresh without a reboot.
// Returns whether any key was rewritten.
func Repair(exePath string, mappings []Mapping, logf func(format string, args ...any)) (bool, error) {
	if _, err := os.Stat(exePath); err != nil {
		return false, fmt.Errorf("refusing to register associations: %s not found: %w", exePath, err)
	}

	command := fmt.Sprintf(`"%s" "%%1"`, exePath)
	icon := exePath + ",0"

	changed := false
	for _, m := range mappings {
		c, err := repairOne(m, command, icon)
		if err != nil {
			return changed, fmt.Errorf("failed to repair %s association: %w", m.Ext, err)
		}
		if c {
			logf("repaired %s -> %s (%s)", m.Ext, m.ProgID, command)
			changed = true
		}
	}

	if changed {
		// SHCNE_ASSOCCHANGED with SHCNF_IDLIST and no item: global refresh.
		procSHChangeNotify.Call(shcneAssocChanged, shcnfIDList, 0, 0)
	}
	return changed, nil
}

// repairOne ensures all keys for one extension exist with the right values,
// writing only what differs to keep registry churn (and SHChangeNotify) to a
// minimum.
func repairOne(m Mapping, command, icon string) (bool, error) {
	changed := false

	// HKLM\Software\Classes\<ext> default -> ProgID
	c, err := ensureValue(`Software\Classes\`+m.Ext, "", m.ProgID)
	if err != nil {
		return changed, err
	}
	changed = changed || c

	progRoot := `Software\Classes\` + m.ProgID
	for _, kv := range []struct{ key, name, value string }{
		{progRoot, "", m.DisplayName},
		{progRoot + `\DefaultIcon`, "", icon},
		{progRoot + `\shell\open\command`, "", command},
		{progRoot + `\shell\open`, "FriendlyAppName", "EMLy"},
	} {
		c, err := ensureValue(kv.key, kv.name, kv.value)
		if err != nil {
			return changed, err
		}
		changed = changed || c
	}
	return changed, nil
}

// ensureValue makes sure the named string value under HKLM\<path> equals
// want, creating the key when missing. Returns whether a write happened.
func ensureValue(path, name, want string) (bool, error) {
	key, _, err := registry.CreateKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return false, fmt.Errorf("open/create HKLM\\%s: %w", path, err)
	}
	defer key.Close()

	current, _, err := key.GetStringValue(name)
	if err == nil && current == want {
		return false, nil
	}
	if err := key.SetStringValue(name, want); err != nil {
		return false, fmt.Errorf("set HKLM\\%s [%s]: %w", path, name, err)
	}
	return true, nil
}

// DefaultMappings builds the standard EMLy mappings from configured ProgIDs.
func DefaultMappings(progIDEml, progIDMsg string) []Mapping {
	return []Mapping{
		{Ext: ".eml", ProgID: progIDEml, DisplayName: "EMLy Email Message"},
		{Ext: ".msg", ProgID: progIDMsg, DisplayName: "EMLy Outlook Message"},
	}
}

// ExePath joins the EMLy install dir and exe name (helper for callers).
func ExePath(installDir, exeName string) string {
	return filepath.Join(installDir, exeName)
}
