// Package config loads and validates the updater's own INI configuration and
// reads EMLy's config.ini for the installed version, release channel, and UI
// language.
//
// Everything the updater owns lives under %ProgramData%\EMLyUpdater so the
// service is fully independent of EMLy's install directory and survives EMLy
// uninstall/reinstall.
package config

import (
	"os"
	"path/filepath"
)

// DataDir returns the updater's root data directory
// (C:\ProgramData\EMLyUpdater on a standard system).
func DataDir() string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	return filepath.Join(pd, "EMLyUpdater")
}

// ConfigPath returns the path of the updater's own config.ini.
func ConfigPath() string { return filepath.Join(DataDir(), "config.ini") }

// StatePath returns the path of the pending-update state file.
func StatePath() string { return filepath.Join(DataDir(), "state.json") }

// DownloadsDir returns the directory where setup executables are cached.
func DownloadsDir() string { return filepath.Join(DataDir(), "downloads") }

// LogsDir returns the directory for the rolling file log and installer logs.
func LogsDir() string { return filepath.Join(DataDir(), "logs") }

// ExeDir returns the directory that contains the running executable.
func ExeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// ExeLogPath returns the path of the local log file written next to the exe.
func ExeLogPath() string { return filepath.Join(ExeDir(), "updater.log") }

// EnsureDirs creates the ProgramData directory tree if it does not exist.
func EnsureDirs() error {
	for _, dir := range []string{DataDir(), DownloadsDir(), LogsDir()} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	return nil
}
