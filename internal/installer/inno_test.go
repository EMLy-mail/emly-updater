package installer

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// The arguments EMLy's setup is run with: /FORCEUPGRADE (its installer asks
// Yes/No on upgrade even under /VERYSILENT without it) and /ALLUSERS (an
// HKLM registration rather than SYSTEM's HKCU) are for EMLy-style setups only.
func TestInnoInstallArgs(t *testing.T) {
	want := []string{"/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/FORCEUPGRADE", "/ALLUSERS", `/LOG=C:\logs\emly-install-2.3.0.log`}
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

// waitRemoved returns once the uninstaller's exe is gone (the old
// uninstaller's last step), and fails when it stays.
func TestWaitRemoved(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "unins000.exe")
	if err := os.WriteFile(exe, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		os.Remove(exe)
	}()
	if err := waitRemoved(exe, 5*time.Second); err != nil {
		t.Fatalf("removed after 300ms: %v", err)
	}

	if err := os.WriteFile(exe, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := waitRemoved(exe, 300*time.Millisecond); err == nil {
		t.Fatal("exe never removed: want an error")
	}
}
