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

// installTimeout bounds a hung setup process; a normal silent install takes
// seconds.
const installTimeout = 15 * time.Minute

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

	cmd := exec.Command(setupPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start setup %s: %w", setupPath, err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				// InnoSetup exit codes 1-8 indicate distinct failures; the log
				// file written via /LOG holds the detail.
				return fmt.Errorf("setup exited with code %d (see %s)", exitErr.ExitCode(), logPath)
			}
			return fmt.Errorf("setup failed: %w", err)
		}
		return nil
	case <-time.After(installTimeout):
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("setup did not finish within %s, killed (see %s)", installTimeout, logPath)
	}
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
