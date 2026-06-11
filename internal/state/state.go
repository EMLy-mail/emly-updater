// Package state persists the pending-update queue to
// %ProgramData%\EMLyUpdater\state.json so a queued update survives service
// restarts, reboots, and EMLy uninstall/reinstall.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Pending describes a downloaded, checksum-verified update waiting to be
// installed (typically because EMLy was running when it was downloaded).
type Pending struct {
	Version      string    `json:"version"`
	SetupPath    string    `json:"setupPath"`
	SHA256       string    `json:"sha256"`
	Forced       bool      `json:"forced"`
	DownloadedAt time.Time `json:"downloadedAt"`
}

// State is the on-disk document. Kept as a struct (not a bare Pending) so
// future fields can be added without a format break.
type State struct {
	Pending *Pending `json:"pending,omitempty"`
}

// Store reads and writes the state file.
type Store struct {
	Path string
}

// Load reads the state file. A missing file is an empty state, not an error;
// a corrupt file is reported so the caller can log it and start fresh.
func (s *Store) Load() (*State, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{}, nil
		}
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("corrupt state file %s: %w", s.Path, err)
	}
	return &st, nil
}

// Save writes the state atomically: temp file in the same directory, then
// rename, so a crash mid-write can never leave a truncated state.json.
func (s *Store) Save(st *State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, "state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}

	// os.Rename maps to MoveFileEx(MOVEFILE_REPLACE_EXISTING), which replaces
	// the destination atomically on NTFS.
	if err := os.Rename(tmpName, s.Path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// SetPending persists p as the pending update.
func (s *Store) SetPending(p *Pending) error {
	return s.Save(&State{Pending: p})
}

// ClearPending removes any pending update.
func (s *Store) ClearPending() error {
	return s.Save(&State{})
}
