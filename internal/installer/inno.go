package installer

import (
	"fmt"
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
	return runSilent(exec.Command(matches[0], innoUninstallArgs(logPath)...), logPath)
}

// innoInstallArgs: /FORCEUPGRADE is EMLy-specific - without it EMLy's
// InitializeSetup shows a Yes/No upgrade dialog even under /VERYSILENT (see
// installer.iss in the emly repo).
func innoInstallArgs(logPath string, force bool) []string {
	args := []string{"/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART"}
	if force {
		args = append(args, "/FORCEUPGRADE")
	}
	return append(args, "/LOG="+logPath)
}

func innoUninstallArgs(logPath string) []string {
	return []string{"/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/LOG=" + logPath}
}
