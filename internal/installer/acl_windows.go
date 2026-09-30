package installer

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ErrUserWritable is wrapped when an uninstaller, or the directory it lives
// in, is not exclusively controlled by SYSTEM, Administrators and
// TrustedInstaller: running it as SYSTEM would let anyone who could replace it
// run code as SYSTEM.
var ErrUserWritable = errors.New("writable by non-administrators")

// writeRights are the rights that let a principal replace or alter a file or
// directory entry.
const writeRights = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.WRITE_DAC |
	windows.WRITE_OWNER | windows.GENERIC_WRITE | windows.GENERIC_ALL | windows.DELETE |
	0x40 // FILE_DELETE_CHILD

// trustedInstallerSID is NT SERVICE\TrustedInstaller, the owner of Windows
// and Program Files content.
const trustedInstallerSID = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"

// ACE types (winnt.h) that are not plain ACCESS_ALLOWED_ACE but still grant.
const (
	aceAllowedObject         = 5
	aceAllowedCallback       = 9
	aceAllowedCallbackObject = 11
)

func trustedSIDs() ([]*windows.SID, error) {
	var out []*windows.SID
	for _, kind := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		sid, err := windows.CreateWellKnownSid(kind)
		if err != nil {
			return nil, fmt.Errorf("build a trusted SID: %w", err)
		}
		out = append(out, sid)
	}
	ti, err := windows.StringToSid(trustedInstallerSID)
	if err != nil {
		return nil, fmt.Errorf("build the TrustedInstaller SID: %w", err)
	}
	return append(out, ti), nil
}

func isTrusted(sid *windows.SID, trusted []*windows.SID) bool {
	for _, t := range trusted {
		if sid.Equals(t) {
			return true
		}
	}
	return false
}

// CheckNotUserWritable reports ErrUserWritable unless every path is owned by
// SYSTEM, Administrators or TrustedInstaller and its DACL grants write rights
// only to those same principals. A deny-list of "Users/Everyone" is not enough:
// the owner implicitly holds WRITE_DAC, and a grant to any single user,
// INTERACTIVE or Domain Users is just as exploitable.
func CheckNotUserWritable(paths ...string) error {
	for _, path := range paths {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
			windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return fmt.Errorf("read the security descriptor of %s: %w", path, err)
		}
		if err := checkSecurityDescriptor(sd); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}

// checkSecurityDescriptor applies the owner + allow-list rule to one
// descriptor. Deny ACEs are skipped (ignoring one can only cause a false
// refusal); an allow-type ACE it cannot interpret is refused.
func checkSecurityDescriptor(sd *windows.SECURITY_DESCRIPTOR) error {
	trusted, err := trustedSIDs()
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("read the owner: %w", err)
	}
	if owner == nil || !isTrusted(owner, trusted) {
		return fmt.Errorf("owned by %v: %w", owner, ErrUserWritable)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("read the DACL: %w", err)
	}
	if dacl == nil {
		return fmt.Errorf("NULL DACL: %w", ErrUserWritable)
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return fmt.Errorf("read ACE %d: %w", i, err)
		}
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		case aceAllowedObject, aceAllowedCallback, aceAllowedCallbackObject:
			return fmt.Errorf("ACE %d has an unsupported allow type %d: %w", i, ace.Header.AceType, ErrUserWritable)
		default:
			continue
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || ace.Mask&writeRights == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !isTrusted(sid, trusted) {
			return fmt.Errorf("grants write access to %s: %w", sid.String(), ErrUserWritable)
		}
	}
	return nil
}
