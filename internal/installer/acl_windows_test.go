package installer

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

func errorsIs(err, target error) bool { return errors.Is(err, target) }

// grantEveryoneWrite adds an explicit "Everyone: full control" ACE to path.
// No elevation needed: the test owns its temp directory.
func grantEveryoneWrite(t *testing.T, path string) {
	t.Helper()
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	current, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(everyone),
		},
	}}, current)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}

// A per-user temp directory grants write only to its owner, SYSTEM and
// Administrators.
func TestCheckNotUserWritableAcceptsAPrivateDirectory(t *testing.T) {
	if err := CheckNotUserWritable(t.TempDir()); err != nil {
		t.Fatalf("CheckNotUserWritable = %v", err)
	}
}

func TestCheckNotUserWritableRejectsEveryoneWrite(t *testing.T) {
	dir := t.TempDir()
	grantEveryoneWrite(t, dir)
	if err := CheckNotUserWritable(dir); !errors.Is(err, ErrUserWritable) {
		t.Fatalf("CheckNotUserWritable = %v, want ErrUserWritable", err)
	}
}
