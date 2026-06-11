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
