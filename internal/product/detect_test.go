package product

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func iniSource(path string) VersionSource {
	return VersionSource{Type: SourceINI, Path: path, Section: "app", Key: "version"}
}

func TestDetectINIInstalled(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "config.ini", "[app]\nversion = 1.2.0   ; managed\n")
	r := Detect(&Product{InstallDir: dir, Detect: []VersionSource{iniSource("config.ini")}})
	if r.Outcome != Installed || r.Version != "1.2.0" || r.Source != "ini:config.ini" {
		t.Fatalf("got %+v, want installed 1.2.0 from ini:config.ini", r)
	}
}

// Every source's file missing means the product is not on this machine.
func TestDetectAllMissingIsAbsent(t *testing.T) {
	r := Detect(&Product{InstallDir: t.TempDir(), Detect: []VersionSource{iniSource("config.ini")}})
	if r.Outcome != Absent {
		t.Fatalf("got %+v, want absent", r)
	}
}

// A missing file falls through to the next source.
func TestDetectFallsThroughMissingFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "second.ini", "[app]\nversion=2.0.0\n")
	r := Detect(&Product{InstallDir: dir, Detect: []VersionSource{iniSource("first.ini"), iniSource("second.ini")}})
	if r.Outcome != Installed || r.Version != "2.0.0" {
		t.Fatalf("got %+v, want installed 2.0.0", r)
	}
}

// A source whose file exists but is broken, with nothing after it answering,
// is unknown - never absent: absent would drop the product from the
// inventory and could trigger a fresh install over a working copy.
func TestDetectBrokenSourceIsUnknown(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "config.ini", "[app]\nlanguage = it\n")
	r := Detect(&Product{InstallDir: dir, Detect: []VersionSource{iniSource("config.ini"), iniSource("missing.ini")}})
	if r.Outcome != Unknown || r.Err == nil {
		t.Fatalf("got %+v, want unknown with an error", r)
	}
}

// A broken source followed by a working one: the working one wins.
func TestDetectBrokenThenInstalledIsInstalled(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "broken.ini", "[app]\n")
	write(t, dir, "good.ini", "[app]\nversion=3.1.0\n")
	r := Detect(&Product{InstallDir: dir, Detect: []VersionSource{iniSource("broken.ini"), iniSource("good.ini")}})
	if r.Outcome != Installed || r.Version != "3.1.0" {
		t.Fatalf("got %+v, want installed 3.1.0", r)
	}
}

func TestDetectUnparsableVersionIsUnknown(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "config.ini", "[app]\nversion = latest\n")
	if r := Detect(&Product{InstallDir: dir, Detect: []VersionSource{iniSource("config.ini")}}); r.Outcome != Unknown {
		t.Fatalf("got %+v, want unknown", r)
	}
}

// EMLy's config.ini is configured as an absolute path that need not live
// under its install directory: an absolute Path is used as-is.
func TestDetectAbsolutePathIsUsedAsIs(t *testing.T) {
	path := write(t, t.TempDir(), "config.ini", "[EMLy]\nGUI_SEMVER=2.2.5\n")
	p := &Product{InstallDir: t.TempDir(), Detect: []VersionSource{{Type: SourceINI, Path: path, Section: "EMLy", Key: "GUI_SEMVER"}}}
	if r := Detect(p); r.Outcome != Installed || r.Version != "2.2.5" {
		t.Fatalf("got %+v, want installed 2.2.5", r)
	}
}

func TestManifestPath(t *testing.T) {
	if got := (&Product{Slug: EMLySlug}).ManifestPath(); got != "/v2/updates/manifest" {
		t.Errorf("emly manifest path = %q", got)
	}
	if got := (&Product{Slug: "3g-rocketchat"}).ManifestPath(); got != "/v2/updates/3g-rocketchat/manifest" {
		t.Errorf("product manifest path = %q", got)
	}
}

func TestResultString(t *testing.T) {
	r := Result{Outcome: Installed, Version: "1.0.0", Source: "file:version.txt"}
	if !strings.Contains(r.String(), "1.0.0") || !strings.Contains(r.String(), "file:version.txt") {
		t.Errorf("String() = %q", r.String())
	}
}

// version.txt as a Windows setup writes it: UTF-8 BOM, CRLF, trailing newline.
func TestDetectFileStripsBOMAndLineEndings(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "version.txt", "\xEF\xBB\xBF1.0.0\r\n")
	r := Detect(&Product{InstallDir: dir, Detect: []VersionSource{{Type: SourceFile, Path: "version.txt"}}})
	if r.Outcome != Installed || r.Version != "1.0.0" {
		t.Fatalf("got %+v, want installed 1.0.0", r)
	}
}

func TestDetectEmptyFileIsUnknown(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "version.txt", "\r\n")
	if r := Detect(&Product{InstallDir: dir, Detect: []VersionSource{{Type: SourceFile, Path: "version.txt"}}}); r.Outcome != Unknown {
		t.Fatalf("got %+v, want unknown", r)
	}
}

// RocketChat's chain today: version.txt first, the (stale) config.ini
// version only when version.txt is gone.
func TestDetectFileBeforeINI(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "version.txt", "1.1.0\n")
	write(t, dir, "config.ini", "[app]\nversion = 1.0.0\n")
	chain := []VersionSource{{Type: SourceFile, Path: "version.txt"}, iniSource("config.ini")}
	if r := Detect(&Product{InstallDir: dir, Detect: chain}); r.Version != "1.1.0" {
		t.Fatalf("got %+v, want 1.1.0 from version.txt", r)
	}
	_ = os.Remove(filepath.Join(dir, "version.txt"))
	if r := Detect(&Product{InstallDir: dir, Detect: chain}); r.Version != "1.0.0" || r.Source != "ini:config.ini" {
		t.Fatalf("got %+v, want 1.0.0 from the ini once version.txt is gone", r)
	}
}

// notepad.exe carries VERSIONINFO on every Windows install.
func TestDetectExeReadsVersionInfo(t *testing.T) {
	sys := os.Getenv("SystemRoot")
	if sys == "" {
		sys = `C:\Windows`
	}
	p := &Product{InstallDir: filepath.Join(sys, "System32"), Detect: []VersionSource{{Type: SourceExe, Path: "notepad.exe"}}}
	r := Detect(p)
	if r.Outcome != Installed || strings.Count(r.Version, ".") != 3 {
		t.Fatalf("got %+v, want installed a.b.c.d", r)
	}
}

// An exe without VERSIONINFO exists but says nothing: unknown, like the
// current 3g-RocketChat.exe. The test binary itself has none.
func TestDetectExeWithoutVersionInfoIsUnknown(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := &Product{InstallDir: filepath.Dir(self), Detect: []VersionSource{{Type: SourceExe, Path: filepath.Base(self)}}}
	if r := Detect(p); r.Outcome != Unknown {
		t.Fatalf("got %+v, want unknown", r)
	}
}
