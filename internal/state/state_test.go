package state

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLoadMissingFile(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "state.json")}
	st, err := s.Load()
	if err != nil {
		t.Fatalf("missing file must load as empty state, got %v", err)
	}
	if st.Pending != nil {
		t.Fatal("expected no pending update")
	}
}

func TestRoundTrip(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "state.json")}

	p := &Pending{
		Version:      "1.7.5",
		SetupPath:    `C:\ProgramData\EMLyUpdater\downloads\EMLy-1.7.5-setup.exe`,
		SHA256:       "e475",
		Forced:       true,
		DownloadedAt: time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC),
	}
	if err := s.SetPending(p); err != nil {
		t.Fatal(err)
	}

	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Pending == nil || *st.Pending != *p {
		t.Fatalf("round trip mismatch: %+v", st.Pending)
	}

	if err := s.ClearPending(); err != nil {
		t.Fatal(err)
	}
	st, err = s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Pending != nil {
		t.Fatal("pending not cleared")
	}
}

// The two lifecycles in the document are independent and are written by
// different parts of a cycle: neither may erase the other.
func TestPendingAndSelfUpdateCoexist(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "state.json")}

	su := &SelfUpdate{
		Version:     "1.5.0",
		FromVersion: "1.4.2",
		SetupPath:   `C:\ProgramData\EMLyUpdater\downloads\selfupdate\EMLyUpdater_Installer_1.5.0.exe`,
		SHA256:      "abcd",
		Attempts:    1,
		LaunchedAt:  time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC),
	}
	if err := s.SetSelfUpdate(su); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPending(&Pending{Version: "1.7.5"}); err != nil {
		t.Fatal(err)
	}

	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.SelfUpdate == nil {
		t.Fatal("queueing an EMLy update erased the self-update record")
	}
	if *st.SelfUpdate != *su {
		t.Fatalf("self-update round trip mismatch: %+v", st.SelfUpdate)
	}
	if st.Pending == nil || st.Pending.Version != "1.7.5" {
		t.Fatalf("pending update lost: %+v", st.Pending)
	}

	// ...and clearing one leaves the other alone, in both directions.
	if err := s.ClearSelfUpdate(); err != nil {
		t.Fatal(err)
	}
	st, err = s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.SelfUpdate != nil {
		t.Fatal("self-update not cleared")
	}
	if st.Pending == nil {
		t.Fatal("clearing the self-update erased the pending EMLy update")
	}

	if err := s.ClearPending(); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSelfUpdate(su); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearPending(); err != nil {
		t.Fatal(err)
	}
	st, err = s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.SelfUpdate == nil {
		t.Fatal("clearing the pending EMLy update erased the self-update record")
	}
}

// A state file that exists but does not parse must not block writes forever
// (self-update and the destructive commands all need to record state before
// acting): the write moves it aside to state.json.corrupt, keeping the
// original bytes for diagnosis, and rebuilds from an empty State.
func TestSetBacksUpCorruptStateAndRebuilds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	original := []byte("{truncated")
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatal(err)
	}
	var gotBackup string
	var gotErr error
	s := &Store{Path: path, OnCorrupt: func(backup string, err error) { gotBackup, gotErr = backup, err }}

	if err := s.SetPending(&Pending{Version: "1.7.5"}); err != nil {
		t.Fatalf("SetPending over a corrupt state file: %v", err)
	}

	backup := path + ".corrupt"
	if gotBackup != backup || !errors.Is(gotErr, ErrCorrupt) {
		t.Fatalf("OnCorrupt(%q, %v), want (%q, ErrCorrupt)", gotBackup, gotErr, backup)
	}
	got, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("backup = %q, want the original bytes %q", got, original)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Pending == nil || st.Pending.Version != "1.7.5" || st.SelfUpdate != nil || len(st.PendingCommands) != 0 {
		t.Fatalf("rebuilt state = %+v, want only the new pending update", st)
	}
}

// Any read error other than a missing or unparseable file (sharing
// violation, access denied, disk error) may be transient: the write must
// fail and leave whatever is at the path alone. A directory in place of the
// file stands in for such an error here.
func TestSetReturnsErrorOnUnreadableStateAndLeavesItUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	s := &Store{Path: path}

	err := s.SetPending(&Pending{Version: "1.7.5"})
	if err == nil {
		t.Fatal("SetPending over an unreadable state file must return an error")
	}
	if errors.Is(err, ErrCorrupt) {
		t.Fatalf("an I/O error was classified as corrupt: %v", err)
	}
	if fi, statErr := os.Stat(path); statErr != nil || !fi.IsDir() {
		t.Fatalf("unreadable state path was touched: %v", statErr)
	}
	if _, statErr := os.Stat(path + ".corrupt"); !os.IsNotExist(statErr) {
		t.Fatalf("an I/O error produced a .corrupt backup: %v", statErr)
	}
}

func TestLoadCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{truncated"), 0644); err != nil {
		t.Fatal(err)
	}
	s := &Store{Path: path}
	if _, err := s.Load(); err == nil {
		t.Fatal("expected error for corrupt state file")
	}
}

func TestSaveLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Path: filepath.Join(dir, "state.json")}
	if err := s.SetPending(&Pending{Version: "1.0.0"}); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Fatalf("unexpected files after save: %v", entries)
	}
}

func TestPendingCommandsSurviveAndAreTakenOnce(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "state.json")}
	if err := s.SetPending(&Pending{Version: "3.5.0"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)
	_ = s.AddPendingCommand(PendingCommand{ID: "A", Name: "machine.reboot", AcceptedAt: now})
	_ = s.AddPendingCommand(PendingCommand{ID: "B", Name: "service.restart", AcceptedAt: now})
	_ = s.RemovePendingCommand("B")

	got, err := s.TakePendingCommands()
	if err != nil || len(got) != 1 || got[0].ID != "A" {
		t.Fatalf("take = %+v, %v", got, err)
	}
	if again, _ := s.TakePendingCommands(); len(again) != 0 {
		t.Fatalf("second take = %+v", again)
	}
	st, _ := s.Load()
	if st.Pending == nil || st.Pending.Version != "3.5.0" {
		t.Fatalf("pending update lost: %+v", st.Pending)
	}
}

// TestConcurrentWritesUnderRace exercises Store the way the service actually
// drives it: the welcome-burst goroutine (TakePendingCommands), several
// command goroutines (Add/RemovePendingCommand) and the poll goroutine
// (SetPending/SetSelfUpdate/ClearSelfUpdate) all writing state.json at once.
// Run with -race: without Store's mutex, two read-modify-write calls racing
// on the same Load can each start from the same snapshot and one write is
// silently lost - this both catches the data race and asserts nothing was
// actually dropped.
func TestConcurrentWritesUnderRace(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "state.json")}

	const commands = 20
	var wg sync.WaitGroup

	// Add N distinct pending commands concurrently...
	for i := 0; i < commands; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "cmd-" + string(rune('A'+i))
			_ = s.AddPendingCommand(PendingCommand{ID: id, Name: "machine.reboot", AcceptedAt: time.Now()})
		}(i)
	}
	// ...while the poll goroutine repeatedly sets and clears the two other
	// lifecycles, and one command goroutine removes one of the pending ids.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < commands; i++ {
			_ = s.SetPending(&Pending{Version: "1.7.5"})
			_ = s.SetSelfUpdate(&SelfUpdate{Version: "1.8.0", Attempts: i})
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = s.RemovePendingCommand("cmd-" + string(rune('A')))
	}()
	wg.Wait()

	st, err := s.Load()
	if err != nil {
		t.Fatalf("Load after concurrent writes: %v", err)
	}
	if st.Pending == nil || st.Pending.Version != "1.7.5" {
		t.Fatalf("pending update lost under concurrent writes: %+v", st.Pending)
	}
	if st.SelfUpdate == nil || st.SelfUpdate.Version != "1.8.0" {
		t.Fatalf("self-update record lost under concurrent writes: %+v", st.SelfUpdate)
	}
	// Every Add landed (none silently overwritten by a concurrent one), and
	// the one Remove took effect: commands-1 pending commands remain.
	if len(st.PendingCommands) != commands-1 {
		t.Fatalf("pending commands = %d, want %d: %+v", len(st.PendingCommands), commands-1, st.PendingCommands)
	}
}
