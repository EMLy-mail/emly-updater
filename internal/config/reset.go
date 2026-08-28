package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// BackupPath returns the path where Reset preserves the pre-upgrade config.
func BackupPath() string { return filepath.Join(DataDir(), "config.prev.ini") }

// Reset replaces the config file at path with the defaults embedded in this
// build. Every `install` (including a self-update) therefore starts from this
// release's config.default.ini: keys added by this release appear with their
// default and their Italian documentation, keys it dropped disappear, and any
// per-machine edit is discarded rather than merged forward.
//
// The previous file is copied to config.prev.ini before anything is written,
// so a setting someone actually needed is recoverable on the machine itself.
// A missing config file is not an error: the defaults are written as-is, which
// is exactly what WriteDefault does on a fresh install.
//
// Returns true when the file on disk changed.
func Reset(path string) (bool, error) {
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return WriteDefault(path)
		}
		return false, err
	}
	if string(oldRaw) == string(defaultINI) {
		return false, nil
	}

	if err := os.WriteFile(BackupPath(), oldRaw, 0644); err != nil {
		// Refuse to discard a config we could not back up first.
		return false, fmt.Errorf("failed to back up %s to %s: %w", path, BackupPath(), err)
	}
	if err := writeAtomic(path, string(defaultINI)); err != nil {
		return false, err
	}
	return true, nil
}

// writeAtomic replaces the file at path with content via a temp file in the
// same directory and a rename. os.Rename maps to
// MoveFileEx(MOVEFILE_REPLACE_EXISTING), which replaces the destination
// atomically on NTFS - the same guarantee state.Store relies on, so a crash
// mid-write cannot leave a truncated config behind for the next service start
// to fail on. Shared by Reset and SetPrimary, the only two writers of
// config.ini.
func writeAtomic(path, content string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
