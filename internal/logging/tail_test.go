package logging

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	tail, err := ReadTail(path, 4)
	if err != nil || string(tail.Data) != "6789" || tail.Offset != 6 || tail.Size != 10 {
		t.Fatalf("tail = %+v, %v", tail, err)
	}
	tail, err = ReadTail(path, 100)
	if err != nil || string(tail.Data) != "0123456789" || tail.Offset != 0 {
		t.Fatalf("whole = %+v, %v", tail, err)
	}
}

// The handle ReadTail holds must not stop lumberjack from renaming the live
// file away on rotation.
func TestReadTailSharesDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := openShared(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Rename(path, filepath.Join(dir, "rotated.log")); err != nil {
		t.Fatalf("rename while open: %v", err)
	}
}

func TestReadTailMissing(t *testing.T) {
	if _, err := ReadTail(filepath.Join(t.TempDir(), FileName), 10); !os.IsNotExist(err) {
		t.Fatalf("err = %v, want not-exist", err)
	}
}
