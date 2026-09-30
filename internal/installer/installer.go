// Package installer runs product setups silently through a driver per
// installer technology, and confirms EMLy's result through its config.ini (the
// setup ships a fresh config.ini carrying the new version, so a successful
// install is directly observable there).
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

// Driver runs one product's setup and uninstaller silently.
type Driver interface {
	// Install runs the setup at setupPath and waits for it.
	Install(setupPath, version string) error
	// Uninstall runs the product's own uninstaller, if one is present in its
	// install directory, and waits for it. No uninstaller is not an error.
	Uninstall() error
}

// Spec selects and configures a driver.
type Spec struct {
	Slug       string // names the log files
	Type       string // "inno" | "nsis"
	InstallDir string
	LogsDir    string
	// ForceUpgrade adds EMLy's /FORCEUPGRADE (Inno only).
	ForceUpgrade bool
}

// For returns the driver for s.Type. The remote-configuration validator only
// lets "inno" and "nsis" through; anything else falls back to Inno, which is
// what EMLy - the only definition not coming from the document - uses.
func For(s Spec) Driver {
	switch s.Type {
	default:
		return inno{s}
	}
}

// runSilent starts cmd hidden, waits for it to exit, and translates the
// result into an error. logPath names where the setup wrote its own log, for
// the error message; "" when it writes none.
func runSilent(cmd *exec.Cmd, logPath string) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	name := filepath.Base(cmd.Path)
	see := ""
	if logPath != "" {
		see = " (see " + logPath + ")"
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start %s: %w", cmd.Path, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				return fmt.Errorf("%s exited with code %d%s", name, exitErr.ExitCode(), see)
			}
			return fmt.Errorf("%s failed: %w", name, err)
		}
		return nil
	case <-time.After(installTimeout):
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("%s did not finish within %s, killed%s", name, installTimeout, see)
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
