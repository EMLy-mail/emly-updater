package state

import (
	"os"
	"path/filepath"
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

// A state file that cannot be parsed must not wedge the service into being
// unable to record anything: it holds only re-derivable bookkeeping.
func TestSetRebuildsCorruptState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{truncated"), 0644); err != nil {
		t.Fatal(err)
	}
	s := &Store{Path: path}

	if err := s.SetPending(&Pending{Version: "1.7.5"}); err != nil {
		t.Fatalf("writing over a corrupt state file failed: %v", err)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatalf("state file still unreadable: %v", err)
	}
	if st.Pending == nil || st.Pending.Version != "1.7.5" {
		t.Fatalf("unexpected state after rebuild: %+v", st)
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
