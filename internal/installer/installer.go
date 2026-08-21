// Package installer runs EMLy's InnoSetup setup silently and confirms the
// result by re-reading GUI_SEMVER from EMLy's config.ini (the setup ships a
// fresh config.ini carrying the new version, so a successful install is
// directly observable there).
package installer

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"emlyupdater/internal/config"
	"emlyupdater/internal/manifest"
)

// installTimeout bounds a hung setup/uninstaller process; a normal silent
// run takes seconds.
const installTimeout = 15 * time.Minute

// runSilent starts exePath with args, waits for it to exit, and translates
// the result into an error. Shared by Run and Uninstall - both are "launch an
// InnoSetup-generated exe silently and wait" with the same failure modes
// (non-zero exit, hang).
func runSilent(exePath string, args []string, logPath string) error {
	cmd := exec.Command(exePath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start %s: %w", exePath, err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				// InnoSetup exit codes 1-8 indicate distinct failures; the log
				// file written via /LOG holds the detail.
				return fmt.Errorf("%s exited with code %d (see %s)", filepath.Base(exePath), exitErr.ExitCode(), logPath)
			}
			return fmt.Errorf("%s failed: %w", filepath.Base(exePath), err)
		}
		return nil
	case <-time.After(installTimeout):
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("%s did not finish within %s, killed (see %s)", filepath.Base(exePath), installTimeout, logPath)
	}
}

// Run executes the setup silently as SYSTEM and waits for completion.
//
// /FORCEUPGRADE is EMLy-specific: without it the installer's InitializeSetup
// shows a Yes/No upgrade dialog even under /VERYSILENT (see installer.iss in
// the emly repo). /LOG writes the InnoSetup log next to the updater's own logs
// for post-mortems.
func Run(setupPath, version, logsDir string) error {
	logPath := filepath.Join(logsDir, fmt.Sprintf("emly-install-%s.log", version))
	args := []string{
		"/VERYSILENT",
		"/SUPPRESSMSGBOXES",
		"/NORESTART",
		"/FORCEUPGRADE",
		"/LOG=" + logPath,
	}
	return runSilent(setupPath, args, logPath)
}

// Uninstall runs EMLy's own InnoSetup-generated uninstaller silently, if one
// is present in installDir, to wipe a stale or inconsistent prior install
// before a clean reinstall.
//
// Called only as remediation when a normal Run + VerifyInstalled leaves
// config.ini not reporting the target version - the Updater's own view of the
// correct version always wins over whatever is already on disk, so instead of
// trusting the existing install's state, the next Run happens against a
// clean slate.
//
// A missing uninstaller (fresh machine, or EMLy was already removed by hand)
// is not an error: there is nothing to clean up, and the subsequent Run
// proceeds normally against an empty install dir.
func Uninstall(installDir, logsDir string) error {
	matches, err := filepath.Glob(filepath.Join(installDir, "unins*.exe"))
	if err != nil {
		return fmt.Errorf("failed to look for EMLy's uninstaller: %w", err)
	}
	if len(matches) == 0 {
		return nil
	}

	logPath := filepath.Join(logsDir, fmt.Sprintf("emly-uninstall-%d.log", time.Now().Unix()))
	args := []string{
		"/VERYSILENT",
		"/SUPPRESSMSGBOXES",
		"/NORESTART",
		"/LOG=" + logPath,
	}
	return runSilent(matches[0], args, logPath)
}

// VerifyInstalled re-reads EMLy's config.ini and confirms GUI_SEMVER now
// equals the expected version. Comparison goes through go-version so
// formatting differences ("1.7.50" padding etc.) cannot cause false negatives.
func VerifyInstalled(emlyConfigFile, expectedVersion string) error {
	info, err := config.ReadEMLyConfig(emlyConfigFile)
	if err != nil {
		return fmt.Errorf("post-install verification failed: %w", err)
	}

	older, err := manifest.Less(info.InstalledVersion, expectedVersion)
	if err != nil {
		return fmt.Errorf("post-install verification failed: %w", err)
	}
	if older {
		return fmt.Errorf("post-install verification failed: config.ini reports %s, expected %s",
			info.InstalledVersion, expectedVersion)
	}
	return nil
}
