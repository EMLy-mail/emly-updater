package installer

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// nsis drives an NSIS setup and its uninstall.exe.
type nsis struct{ s Spec }

// Install runs "setup.exe /S /D=<InstallDir>". /D is passed every time so the
// setup installs exactly where detection looks. NSIS writes no log of its
// own; the exit code is all there is, and the verification that follows is
// what decides.
func (d nsis) Install(setupPath, _ string) error {
	return runSilent(rawCommand(setupPath, "/S /D="+d.s.InstallDir), "")
}

// Uninstall runs "uninstall.exe /S _?=<InstallDir>". Without _?= the NSIS
// uninstaller copies itself to %TEMP%, relaunches from there and exits at
// once, so the caller would start the reinstall while the uninstall is still
// running. With it, the uninstaller runs in place and is not deleted - the
// reinstall overwrites it.
func (d nsis) Uninstall() error {
	exe := filepath.Join(d.s.InstallDir, "uninstall.exe")
	if _, err := os.Stat(exe); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := CheckNotUserWritable(d.s.InstallDir, exe); err != nil {
		return fmt.Errorf("not running %s: %w", exe, err)
	}
	return runSilent(rawCommand(exe, "/S _?="+d.s.InstallDir), "")
}

// rawCommand builds a command whose command line is exe (quoted when needed)
// followed by tail verbatim - see nsisCmdLine.
func rawCommand(exe, tail string) *exec.Cmd {
	cmd := exec.Command(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: nsisCmdLine(exe, tail)}
	return cmd
}

// nsisCmdLine is exe, quoted only if it needs it, then tail untouched: NSIS
// takes /D= and _?= raw up to the end of the line, quotes included.
func nsisCmdLine(exe, tail string) string {
	return syscall.EscapeArg(exe) + " " + tail
}
