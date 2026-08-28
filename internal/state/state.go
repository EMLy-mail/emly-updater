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

// SelfUpdate records an updater release whose setup has been handed off to
// run. The setup stops this very service to replace its executable, so the
// launching process does not survive to observe the outcome: this entry is
// what the next start reads to tell "the new binary is running" from "the
// install did not land", and what keeps a broken release from being retried
// forever.
type SelfUpdate struct {
	Version string `json:"version"`
	// FromVersion is the version that launched the setup. Together with
	// Version it makes the outcome readable from the log of a machine that
	// never came back on the new binary.
	FromVersion string `json:"fromVersion"`
	SetupPath   string `json:"setupPath"`
	SHA256      string `json:"sha256"`
	// Attempts counts the launches of this target version, including the one
	// in flight. Reset when the manifest starts offering a different version.
	Attempts int `json:"attempts"`
	// GaveUp marks a target abandoned after too many attempts, so the give-up
	// is reported once rather than on every poll cycle.
	GaveUp     bool      `json:"gaveUp,omitempty"`
	LaunchedAt time.Time `json:"launchedAt"`
}

// State is the on-disk document. Kept as a struct (not a bare Pending) so
// future fields can be added without a format break.
type State struct {
	Pending    *Pending    `json:"pending,omitempty"`
	SelfUpdate *SelfUpdate `json:"selfUpdate,omitempty"`
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
	return s.update(func(st *State) { st.Pending = p })
}

// ClearPending removes any pending update.
func (s *Store) ClearPending() error {
	return s.update(func(st *State) { st.Pending = nil })
}

// SetSelfUpdate persists su as the self-update in flight.
func (s *Store) SetSelfUpdate(su *SelfUpdate) error {
	return s.update(func(st *State) { st.SelfUpdate = su })
}

// ClearSelfUpdate removes any self-update record.
func (s *Store) ClearSelfUpdate() error {
	return s.update(func(st *State) { st.SelfUpdate = nil })
}

// update applies mutate to the current state and saves the result.
//
// Read-modify-write, not a wholesale overwrite: the document holds two
// independent lifecycles - EMLy's queued update and the updater's own
// self-update - and each is touched by a different part of a cycle. Saving a
// freshly built State from either side would silently drop the other's entry,
// losing a queued EMLy install or the record that tells the next start whether
// a self-update landed.
//
// A state file too corrupt to read is rebuilt rather than propagated: it holds
// only re-derivable bookkeeping, and refusing to write would leave the service
// unable to record anything at all.
func (s *Store) update(mutate func(*State)) error {
	st, err := s.Load()
	if err != nil {
		st = &State{}
	}
	mutate(st)
	return s.Save(st)
}
