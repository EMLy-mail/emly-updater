package product

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	version "github.com/hashicorp/go-version"
	"gopkg.in/ini.v1"
)

// Outcome is what detection concluded about a product on this machine.
type Outcome int

const (
	// Absent: no source of the chain exists. The product is not installed.
	Absent Outcome = iota
	// Installed: a source answered with a parsable version.
	Installed
	// Unknown: a source exists but could not be read, and no later source
	// answered. Never acted on: not installed over, not reported as absent.
	Unknown
)

// Result is Detect's answer.
type Result struct {
	Outcome Outcome
	Version string // Installed only
	Source  string // "type:path" of the source that decided, "" for Absent
	Err     error  // Unknown only
}

func (r Result) String() string {
	switch r.Outcome {
	case Installed:
		return fmt.Sprintf("installed %s (%s)", r.Version, r.Source)
	case Unknown:
		return fmt.Sprintf("unknown (%s: %v)", r.Source, r.Err)
	default:
		return "absent"
	}
}

// Detect walks p's chain in order. A source whose file does not exist passes
// to the next; the first source that yields a version wins. A source that
// exists but is broken is remembered: if nothing after it answers, the
// outcome is Unknown rather than Absent.
func Detect(p *Product) Result {
	var broken *Result
	for _, src := range p.Detect {
		label := src.Type + ":" + src.Path
		v, err := readSource(src, p.resolve(src.Path))
		switch {
		case err == nil:
			return Result{Outcome: Installed, Version: v, Source: label}
		case errors.Is(err, fs.ErrNotExist):
			continue
		case broken == nil:
			broken = &Result{Outcome: Unknown, Source: label, Err: err}
		}
	}
	if broken != nil {
		return *broken
	}
	return Result{Outcome: Absent}
}

func (p *Product) resolve(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(p.InstallDir, path)
}

func readSource(src VersionSource, path string) (string, error) {
	switch src.Type {
	case SourceINI:
		return readINI(path, src.Section, src.Key)
	case SourceFile:
		return readVersionFile(path)
	case SourceExe:
		return readExeVersion(path)
	}
	return "", fmt.Errorf("unknown version source type %q", src.Type)
}

// readINI returns section/key of the INI file at path. A missing file comes
// back wrapping fs.ErrNotExist, which is what lets the chain fall through.
func readINI(path, section, key string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	f, err := ini.Load(path)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	v := strings.TrimSpace(f.Section(section).Key(key).String())
	if v == "" {
		return "", fmt.Errorf("%s: [%s] %s missing", path, section, key)
	}
	return checkVersion(v)
}

func checkVersion(v string) (string, error) {
	if _, err := version.NewVersion(v); err != nil {
		return "", fmt.Errorf("version %q is not parsable: %w", v, err)
	}
	return v, nil
}

// readVersionFile returns the first line of a plain-text version file,
// without a UTF-8 BOM and surrounding whitespace.
func readVersionFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	text := strings.TrimPrefix(string(data), "\xEF\xBB\xBF")
	line, _, _ := strings.Cut(text, "\n")
	v := strings.TrimSpace(line)
	if v == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return checkVersion(v)
}
