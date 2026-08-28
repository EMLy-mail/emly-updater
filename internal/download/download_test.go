package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"emlyupdater/internal/manifest"
)

func sha(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// EMLy's setups and the updater's own installers are cached by two managers.
// Neither cleanup may reach the other's files, or a self-update would delete
// a queued EMLy setup - or, worse, the installer it is about to run.
func TestPrefixIsolatesTwoManagers(t *testing.T) {
	dir := t.TempDir()
	emly := &Manager{Dir: dir}
	updater := &Manager{Dir: dir, Prefix: "EMLyUpdater-"}

	emlySetup := emly.SetupPath("1.7.5")
	updaterSetup := updater.SetupPath("1.5.0")
	if emlySetup == updaterSetup {
		t.Fatal("the two managers cache to the same filename")
	}
	for _, p := range []string{emlySetup, updaterSetup} {
		if err := os.WriteFile(p, []byte("setup"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Clearing EMLy's whole cache must leave the updater's installer alone -
	// note "EMLyUpdater-..." does start with "EMLy-"'s letters but not with
	// the prefix itself, which is what makes this worth asserting.
	if err := emly.CleanupExcept(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(updaterSetup); err != nil {
		t.Fatalf("clearing EMLy's cache deleted the updater installer: %v", err)
	}
	if _, err := os.Stat(emlySetup); !os.IsNotExist(err) {
		t.Errorf("EMLy's own setup survived its cleanup: %v", err)
	}

	// ...and the other way round.
	if err := os.WriteFile(emlySetup, []byte("setup"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := updater.CleanupExcept(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(emlySetup); err != nil {
		t.Fatalf("clearing the updater's cache deleted EMLy's setup: %v", err)
	}
	if _, err := os.Stat(updaterSetup); !os.IsNotExist(err) {
		t.Errorf("the updater installer survived its cleanup: %v", err)
	}
}

// fakeSource serves fixed payload bytes and counts fetches.
type fakeSource struct {
	payload []byte
	fetches int
	fail    bool
}

func (f *fakeSource) Name() string { return "fake" }
func (f *fakeSource) FetchManifest(context.Context) (*manifest.Manifest, error) {
	return nil, errors.New("not used")
}
func (f *fakeSource) ResolveTarget(*manifest.Manifest, string) (manifest.Target, error) {
	return manifest.Target{}, errors.New("not used")
}
func (f *fakeSource) FetchSetup(_ context.Context, _ manifest.Target, destPath string) error {
	f.fetches++
	if f.fail {
		return errors.New("network down")
	}
	return os.WriteFile(destPath, f.payload, 0644)
}

func TestVerifyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.bin")
	data := []byte("hello update")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	if err := VerifyFile(path, sha(data)); err != nil {
		t.Fatalf("matching checksum rejected: %v", err)
	}
	// Hex digests must match case-insensitively (manifests vary).
	upper := func(s string) string {
		b := []byte(s)
		for i, c := range b {
			if c >= 'a' && c <= 'f' {
				b[i] = c - 32
			}
		}
		return string(b)
	}
	if err := VerifyFile(path, upper(sha(data))); err != nil {
		t.Fatalf("uppercase checksum rejected: %v", err)
	}
	if err := VerifyFile(path, sha([]byte("tampered"))); err == nil {
		t.Fatal("mismatching checksum accepted")
	}
	if err := VerifyFile(path, ""); err == nil {
		t.Fatal("empty expected checksum accepted")
	}
	if err := VerifyFile(filepath.Join(t.TempDir(), "missing"), "ab"); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestEnsureDownloadsVerifiesAndCaches(t *testing.T) {
	payload := []byte("installer bytes")
	src := &fakeSource{payload: payload}
	m := &Manager{Dir: t.TempDir()}
	target := manifest.Target{Version: "1.7.5", SHA256: sha(payload)}

	path, err := m.Ensure(context.Background(), src, target)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "EMLy-1.7.5-setup.exe" {
		t.Fatalf("unexpected cache name: %s", path)
	}
	if src.fetches != 1 {
		t.Fatalf("expected 1 fetch, got %d", src.fetches)
	}

	// Second call must reuse the verified cache without fetching again.
	if _, err := m.Ensure(context.Background(), src, target); err != nil {
		t.Fatal(err)
	}
	if src.fetches != 1 {
		t.Fatalf("cache not reused, fetches = %d", src.fetches)
	}
}

func TestEnsureRedownloadsCorruptCache(t *testing.T) {
	payload := []byte("installer bytes")
	src := &fakeSource{payload: payload}
	m := &Manager{Dir: t.TempDir()}
	target := manifest.Target{Version: "1.7.5", SHA256: sha(payload)}

	// Pre-seed a corrupt cache entry.
	if err := os.WriteFile(m.SetupPath("1.7.5"), []byte("corrupt"), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Ensure(context.Background(), src, target); err != nil {
		t.Fatal(err)
	}
	if src.fetches != 1 {
		t.Fatalf("corrupt cache should trigger re-download, fetches = %d", src.fetches)
	}
}

func TestEnsureRejectsTamperedDownload(t *testing.T) {
	src := &fakeSource{payload: []byte("evil bytes")}
	m := &Manager{Dir: t.TempDir()}
	target := manifest.Target{Version: "1.7.5", SHA256: sha([]byte("legit bytes"))}

	if _, err := m.Ensure(context.Background(), src, target); err == nil {
		t.Fatal("tampered download accepted")
	}
	// Neither a final file nor a .partial may remain.
	entries, err := os.ReadDir(m.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("leftover files after failed download: %v", entries)
	}
}

func TestCleanupExcept(t *testing.T) {
	m := &Manager{Dir: t.TempDir()}
	for _, name := range []string{
		"EMLy-1.7.3-setup.exe",
		"EMLy-1.7.4-setup.exe.partial",
		"EMLy-1.7.5-setup.exe",
		"unrelated.txt", // never touched: not created by the updater
	} {
		if err := os.WriteFile(filepath.Join(m.Dir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	if err := m.CleanupExcept("1.7.5"); err != nil {
		t.Fatal(err)
	}

	entries, _ := os.ReadDir(m.Dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 || names[0] != "EMLy-1.7.5-setup.exe" || names[1] != "unrelated.txt" {
		t.Fatalf("unexpected survivors: %v", names)
	}
}
