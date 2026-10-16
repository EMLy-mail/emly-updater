package installer

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// inno drives an Inno Setup setup and its unins*.exe.
type inno struct{ s Spec }

func (d inno) installLog(version string) string {
	return filepath.Join(d.s.LogsDir, fmt.Sprintf("%s-install-%s.log", d.s.Slug, version))
}

// Install runs the setup with /VERYSILENT as SYSTEM. /LOG writes Inno's own
// log next to the agent's for post-mortems.
func (d inno) Install(setupPath, version string) error {
	logPath := d.installLog(version)
	return runSilent(exec.Command(setupPath, innoInstallArgs(logPath, d.s.ForceUpgrade)...), logPath)
}

// Uninstall runs unins*.exe from the install directory, if there is one.
func (d inno) Uninstall() error {
	matches, err := filepath.Glob(filepath.Join(d.s.InstallDir, "unins*.exe"))
	if err != nil {
		return fmt.Errorf("failed to look for %s's uninstaller: %w", d.s.Slug, err)
	}
	if len(matches) == 0 {
		return nil
	}
	if err := CheckNotUserWritable(d.s.InstallDir, matches[0]); err != nil {
		return fmt.Errorf("not running %s: %w", matches[0], err)
	}
	logPath := filepath.Join(d.s.LogsDir, fmt.Sprintf("%s-uninstall-%d.log", d.s.Slug, time.Now().Unix()))
	if err := runSilent(exec.Command(matches[0], innoUninstallArgs(logPath)...), logPath); err != nil {
		return err
	}
	return waitRemoved(matches[0], uninstallSettle)
}

// uninstallSettle bounds the wait for an Inno uninstaller to really finish.
var uninstallSettle = 2 * time.Minute

// waitRemoved waits until the uninstaller exe is gone. An Inno uninstaller
// cannot delete itself while it runs, so unins000.exe copies itself to
// %TEMP% and the copy does the work; the process runSilent waited on exits
// before the end, and the copy's last step is deleting unins000.exe. A setup
// started at that point writes a fresh unins000.exe that the old uninstaller
// then deletes: the install is left without its uninstaller (unins000.dat and
// .msg survive, the .exe does not), which is what every clean reinstall of
// EMLy did. The exe disappearing is the signal that the uninstall is over.
func waitRemoved(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			// The copy may still remove an emptied directory: let it finish.
			time.Sleep(time.Second)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s is still there %s after the uninstaller exited", path, timeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// innoInstallArgs: /FORCEUPGRADE and /ALLUSERS are for setups built from
// EMLy's installer.iss (see the emly repo). Without /FORCEUPGRADE its
// InitializeSetup shows a Yes/No upgrade dialog even under /VERYSILENT.
// That setup is PrivilegesRequired=lowest, so run by the agent as SYSTEM it
// registered itself in SYSTEM's own HKCU - invisible in Programs and
// Features, and one more Inno registration of a folder each user's manual
// update registers again; /ALLUSERS makes it the HKLM, machine-wide one. A
// setup without PrivilegesRequiredOverridesAllowed=commandline ignores it.
func innoInstallArgs(logPath string, force bool) []string {
	args := []string{"/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART"}
	if force {
		args = append(args, "/FORCEUPGRADE", "/ALLUSERS")
	}
	return append(args, "/LOG="+logPath)
}

func innoUninstallArgs(logPath string) []string {
	return []string{"/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/LOG=" + logPath}
}
