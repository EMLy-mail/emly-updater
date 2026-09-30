package installer

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ErrUserWritable is wrapped when an uninstaller, or the directory it lives
// in, can be written by non-administrators: running it as SYSTEM would let
// any user who replaced it run code as SYSTEM.
var ErrUserWritable = errors.New("writable by non-administrators")

// writeRights are the rights that let a principal replace or alter a file.
const writeRights = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.WRITE_DAC |
	windows.WRITE_OWNER | windows.GENERIC_WRITE | windows.GENERIC_ALL | windows.DELETE

var untrustedSIDs = []struct {
	kind windows.WELL_KNOWN_SID_TYPE
	name string
}{
	{windows.WinBuiltinUsersSid, "Users"},
	{windows.WinAuthenticatedUserSid, "Authenticated Users"},
	{windows.WinWorldSid, "Everyone"},
}

// CheckNotUserWritable reports ErrUserWritable when any path's DACL grants a
// write right to Users, Authenticated Users or Everyone, or has no DACL at all.
func CheckNotUserWritable(paths ...string) error {
	for _, path := range paths {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return fmt.Errorf("read the ACL of %s: %w", path, err)
		}
		dacl, _, err := sd.DACL()
		if err != nil {
			return fmt.Errorf("read the DACL of %s: %w", path, err)
		}
		if dacl == nil {
			return fmt.Errorf("%s has a NULL DACL: %w", path, ErrUserWritable)
		}
		for i := uint32(0); i < uint32(dacl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, i, &ace); err != nil {
				return fmt.Errorf("read ACE %d of %s: %w", i, path, err)
			}
			if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
				continue
			}
			if ace.Mask&writeRights == 0 {
				continue
			}
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			for _, u := range untrustedSIDs {
				want, err := windows.CreateWellKnownSid(u.kind)
				if err != nil {
					return err
				}
				if sid.Equals(want) {
					return fmt.Errorf("%s grants write access to %s: %w", path, u.name, ErrUserWritable)
				}
			}
		}
	}
	return nil
}
