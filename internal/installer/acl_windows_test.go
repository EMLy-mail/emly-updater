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

// A t.TempDir() is owned by the (non-admin) test user: its owner could
// rewrite the DACL, so it must be refused.
func TestCheckNotUserWritableRejectsAUserOwnedDirectory(t *testing.T) {
	if err := CheckNotUserWritable(t.TempDir()); !errors.Is(err, ErrUserWritable) {
		t.Fatalf("CheckNotUserWritable = %v, want ErrUserWritable", err)
	}
}

func TestCheckNotUserWritableRejectsEveryoneWrite(t *testing.T) {
	dir := t.TempDir()
	grantEveryoneWrite(t, dir)
	if err := CheckNotUserWritable(dir); !errors.Is(err, ErrUserWritable) {
		t.Fatalf("CheckNotUserWritable = %v, want ErrUserWritable", err)
	}
}

func TestCheckSecurityDescriptor(t *testing.T) {
	cases := []struct {
		name, sddl string
		refused    bool
	}{
		{"system owner, users read only", "O:SYG:SYD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;BU)", false},
		{"admin owner", "O:BAG:SYD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;BU)", false},
		{"user owner", "O:S-1-5-21-1-2-3-1001G:SYD:P(A;;FA;;;SY)", true},
		{"single user write grant", "O:SYG:SYD:P(A;;FA;;;SY)(A;;FA;;;S-1-5-21-1-2-3-1001)", true},
		{"authenticated users write", "O:SYG:SYD:P(A;;FA;;;SY)(A;;0x1301bf;;;AU)", true},
		{"inherit-only creator owner", "O:SYG:SYD:P(A;;FA;;;SY)(A;OICIIO;GA;;;CO)", false},
		{"null dacl", "O:SYG:SYD:NO_ACCESS_CONTROL", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(c.sddl)
			if err != nil {
				t.Fatal(err)
			}
			err = checkSecurityDescriptor(sd)
			if c.refused != errors.Is(err, ErrUserWritable) || (!c.refused && err != nil) {
				t.Fatalf("checkSecurityDescriptor = %v, refused want %v", err, c.refused)
			}
		})
	}
}
