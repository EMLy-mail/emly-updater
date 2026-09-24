// Package state persists the pending-update queue to
// %ProgramData%\EMLyUpdater\state.json so a queued update survives service
// restarts, reboots, and EMLy uninstall/reinstall.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
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

// PendingCommand is a service.restart or machine.reboot this service
// accepted over the client channel. Those verbs kill the process that
// accepted them, so the ID is written here first and reported in
// service.started by the next start (CLIENT_WS_PROTOCOL.md §8.6).
type PendingCommand struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	AcceptedAt time.Time `json:"acceptedAt"`
}

// State is the on-disk document. Kept as a struct (not a bare Pending) so
// future fields can be added without a format break.
type State struct {
	Pending         *Pending         `json:"pending,omitempty"`
	SelfUpdate      *SelfUpdate      `json:"selfUpdate,omitempty"`
	PendingCommands []PendingCommand `json:"pendingCommands,omitempty"`
}

// Store reads and writes the state file.
//
// state.json holds three independent lifecycles - EMLy's pending update, the
// updater's own self-update record, and the client channel's pending
// destructive commands - each written by a different part of a cycle: the
// welcome-burst goroutine (TakePendingCommands), the command goroutines
// (Add/RemovePendingCommand) and the poll goroutine (SetPending/
// SetSelfUpdate/ClearPending/ClearSelfUpdate) can all be in flight at once.
// mu serialises every read-modify-write (and a bare Load/Save) so two calls
// racing on the same read-modify-write cycle cannot each read the same
// starting state and have one write silently overwrite the other's.
type Store struct {
	Path string

	// OnCorrupt, if set, is called when a write finds a state file that does
	// not parse and moves it aside to backupPath before rebuilding (see
	// update). It runs with mu held, so it must not call back into the Store.
	OnCorrupt func(backupPath string, err error)

	mu sync.Mutex
}

// ErrCorrupt is wrapped by the error Load returns for a state file that
// exists but does not parse as JSON.
var ErrCorrupt = errors.New("corrupt state file")

// Load reads the state file. A missing file is an empty state, not an error;
// a corrupt file is reported so the caller can log it and start fresh.
func (s *Store) Load() (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

// loadLocked is Load without acquiring mu - callers that already hold it
// (update, below) call this instead of Load to avoid deadlocking on a
// non-reentrant mutex.
func (s *Store) loadLocked() (*State, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{}, nil
		}
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%w %s: %v", ErrCorrupt, s.Path, err)
	}
	return &st, nil
}

// Save writes the state atomically: temp file in the same directory, then
// rename, so a crash mid-write can never leave a truncated state.json.
func (s *Store) Save(st *State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(st)
}

// saveLocked is Save without acquiring mu - see loadLocked.
func (s *Store) saveLocked(st *State) error {
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

// AddPendingCommand appends a pending command to the state.
func (s *Store) AddPendingCommand(c PendingCommand) error {
	return s.update(func(st *State) { st.PendingCommands = append(st.PendingCommands, c) })
}

// RemovePendingCommand removes a pending command by ID.
func (s *Store) RemovePendingCommand(id string) error {
	return s.update(func(st *State) {
		st.PendingCommands = slices.DeleteFunc(st.PendingCommands, func(c PendingCommand) bool { return c.ID == id })
	})
}

// TakePendingCommands returns the recorded commands and clears them.
func (s *Store) TakePendingCommands() ([]PendingCommand, error) {
	var taken []PendingCommand
	err := s.update(func(st *State) {
		taken, st.PendingCommands = st.PendingCommands, nil
	})
	return taken, err
}

// update applies mutate to the current state and saves the result, holding
// mu for the whole read-modify-write so no other Load/Save/update call can
// interleave with it (see the Store doc comment).
//
// Read-modify-write, not a wholesale overwrite: the document holds three
// independent lifecycles - EMLy's queued update, the updater's own
// self-update record, and the client channel's pending destructive commands
// (PendingCommands) - and each is touched by a different part of a cycle.
// Saving a freshly built State from any one side would silently drop the
// others' entries, losing a queued EMLy install, the record that tells the
// next start whether a self-update landed, or a service.restart/
// machine.reboot id a redelivered command still needs to be recognised
// against.
//
// A missing file is treated as empty (loadLocked already reports that case
// as State{}, nil error). A file that exists but does not parse is moved
// aside to state.json.corrupt and the write proceeds on an empty State: a
// document that no read will ever parse again must not block every later
// write forever - self-update refuses to launch a setup it cannot record,
// and service.restart/machine.reboot refuse to run without their pending
// record. Nothing is lost that a read could still recover; the original
// bytes stay in the backup for diagnosis. Any other Load error - a sharing
// violation, access denied, a disk error - may be transient, so it is
// returned as-is and nothing is written.
func (s *Store) update(mutate func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadLocked()
	if errors.Is(err, ErrCorrupt) {
		backup := s.Path + ".corrupt"
		if rerr := os.Rename(s.Path, backup); rerr != nil {
			return fmt.Errorf("%w (backup failed: %v)", err, rerr)
		}
		if s.OnCorrupt != nil {
			s.OnCorrupt(backup, err)
		}
		st, err = &State{}, nil
	}
	if err != nil {
		return err
	}
	mutate(st)
	return s.saveLocked(st)
}
