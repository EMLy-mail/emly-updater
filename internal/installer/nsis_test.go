package installer

import (
	"os"
	"path/filepath"
	"testing"
)

// NSIS reads /D= and _?= raw to the end of the command line: they must come
// last and must not be quoted, even with spaces - exec.Command would quote
// them, and NSIS would then install into a directory literally named with
// the quotes.
func TestNSISCommandLines(t *testing.T) {
	dir := `C:\Program Files\3gIT\3g-RocketChat`
	if got, want := nsisCmdLine(`C:\dl\3g-rocketchat-1.1.0-setup.exe`, "/S /D="+dir),
		`C:\dl\3g-rocketchat-1.1.0-setup.exe /S /D=C:\Program Files\3gIT\3g-RocketChat`; got != want {
		t.Errorf("install = %q\nwant      %q", got, want)
	}
	if got, want := nsisCmdLine(dir+`\uninstall.exe`, "/S _?="+dir),
		`"C:\Program Files\3gIT\3g-RocketChat\uninstall.exe" /S _?=C:\Program Files\3gIT\3g-RocketChat`; got != want {
		t.Errorf("uninstall = %q\nwant        %q", got, want)
	}
}

func TestForNSIS(t *testing.T) {
	if _, ok := For(Spec{Type: "nsis"}).(nsis); !ok {
		t.Fatal(`For(nsis) is not the NSIS driver`)
	}
}

func TestNSISUninstallWithoutUninstallerIsNoop(t *testing.T) {
	if err := For(Spec{Type: "nsis", InstallDir: t.TempDir()}).Uninstall(); err != nil {
		t.Fatalf("Uninstall = %v", err)
	}
}

// An uninstaller in a directory users can write to is never run as SYSTEM.
func TestNSISUninstallRefusesAUserWritableDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "uninstall.exe"), []byte("not really"), 0o644); err != nil {
		t.Fatal(err)
	}
	grantEveryoneWrite(t, dir)
	err := For(Spec{Type: "nsis", InstallDir: dir}).Uninstall()
	if !errorsIs(err, ErrUserWritable) {
		t.Fatalf("Uninstall = %v, want ErrUserWritable", err)
	}
}
