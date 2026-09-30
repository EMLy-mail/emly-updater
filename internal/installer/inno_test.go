package installer

import (
	"slices"
	"testing"
)

// The arguments EMLy's setup has always been run with: /FORCEUPGRADE is
// EMLy-specific (its installer asks Yes/No on upgrade even under /VERYSILENT
// without it), so only the legacy definition passes it.
func TestInnoInstallArgs(t *testing.T) {
	want := []string{"/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/FORCEUPGRADE", `/LOG=C:\logs\emly-install-2.3.0.log`}
	if got := innoInstallArgs(`C:\logs\emly-install-2.3.0.log`, true); !slices.Equal(got, want) {
		t.Errorf("EMLy args = %q, want %q", got, want)
	}
	want = []string{"/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", `/LOG=C:\logs\x-install-1.0.0.log`}
	if got := innoInstallArgs(`C:\logs\x-install-1.0.0.log`, false); !slices.Equal(got, want) {
		t.Errorf("generic args = %q, want %q", got, want)
	}
}

func TestInnoUninstallArgs(t *testing.T) {
	want := []string{"/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", `/LOG=C:\logs\u.log`}
	if got := innoUninstallArgs(`C:\logs\u.log`); !slices.Equal(got, want) {
		t.Errorf("args = %q, want %q", got, want)
	}
}

// EMLy's log names do not change: "emly" is its slug.
func TestInnoLogNamesFollowTheSlug(t *testing.T) {
	d := For(Spec{Slug: "emly", Type: "inno", LogsDir: `C:\logs`}).(inno)
	if got := d.installLog("2.3.0"); got != `C:\logs\emly-install-2.3.0.log` {
		t.Errorf("install log = %q", got)
	}
}

// An install directory with no uninstaller is not an error: there is nothing
// to clean up.
func TestInnoUninstallWithoutUninstallerIsNoop(t *testing.T) {
	if err := For(Spec{Slug: "emly", Type: "inno", InstallDir: t.TempDir(), LogsDir: t.TempDir()}).Uninstall(); err != nil {
		t.Fatalf("Uninstall = %v, want nil", err)
	}
}
