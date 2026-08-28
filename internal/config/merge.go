package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// BackupPath returns the path where Merge preserves the pre-upgrade config.
func BackupPath() string { return filepath.Join(DataDir(), "config.prev.ini") }

// Merge reconciles the config file at path with the defaults embedded in this
// build: it starts from config.default.ini - so keys added by this release
// appear with their default and their Italian documentation, and keys this
// release dropped disappear - and writes every value the existing file already
// carries back over it.
//
// Per-machine settings therefore always win: an edit pushed by GPO, a channel
// override, a site's internalManifestURL, or the `primary` the startup source
// policy wrote back all survive an upgrade untouched. The one exception is
// userAgent (see mergeINI).
//
// This is what makes the self-update path safe to run unattended: before it
// existed, the updater's own installer deleted config.ini so that `install`
// could rewrite it from the defaults, which would have silently reset every
// machine in the fleet on the first self-update.
//
// The previous file is copied to config.prev.ini before anything is written,
// so a merge that gets something wrong is recoverable on the machine itself.
// A missing config file is not an error: the defaults are written as-is, which
// is exactly what WriteDefault does on a fresh install.
//
// Returns true when the file on disk changed.
func Merge(path string) (bool, error) {
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return WriteDefault(path)
		}
		return false, err
	}

	merged, err := mergeINI(string(defaultINI), string(oldRaw))
	if err != nil {
		return false, fmt.Errorf("failed to merge %s with the embedded defaults: %w", path, err)
	}
	if merged == string(oldRaw) {
		return false, nil
	}

	if err := os.WriteFile(BackupPath(), oldRaw, 0644); err != nil {
		// Refuse to rewrite a config we could not back up first.
		return false, fmt.Errorf("failed to back up %s to %s: %w", path, BackupPath(), err)
	}
	if err := writeAtomic(path, merged); err != nil {
		return false, err
	}
	return true, nil
}

// shippedUserAgentRe matches the userAgent value this project used to ship
// with the version stamped in by tools/genversion (e.g.
// "EMLy-Updater/1.4.2 (f.fois@3git.eu)").
var shippedUserAgentRe = regexp.MustCompile(`^EMLy-Updater/\d+\.\d+\.\d+\b`)

// mergeINI is the pure half of Merge: it returns defaultText with every
// "[section] key" value that oldText also defines replaced by oldText's value.
//
// Like SetPrimary it works on lines rather than going through ini.SaveTo,
// which would re-serialise the whole document and drop the column alignment
// and the Italian comments that make config.default.ini readable to whoever
// edits it on a machine. Everything the default text says - comments, blank
// lines, key order, CRLF or LF endings - is preserved byte for byte; only the
// text to the right of an '=' ever changes.
//
// userAgent is the single deliberate exception to "the old value wins". The
// version used to be stamped into the shipped default, so carrying the old
// value forward would pin a machine's User-Agent to the version it was first
// installed with, forever - and that header is exactly what lets the API tell
// which build a machine is running (and stage a rollout on it). When the old
// value still looks like the shipped default, this release's default wins; a
// value someone actually customised is preserved like any other.
func mergeINI(defaultText, oldText string) (string, error) {
	old := scanINI(oldText)

	lines := strings.SplitAfter(defaultText, "\n")
	section := ""
	for i, line := range lines {
		body, eol := splitEOL(line)
		trimmed := strings.TrimSpace(body)

		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = normalizeININame(trimmed[1 : len(trimmed)-1])
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, ";") || strings.HasPrefix(trimmed, "#") {
			continue
		}

		eq := strings.Index(body, "=")
		if eq < 0 {
			continue
		}
		key := normalizeININame(body[:eq])
		value, ok := old[section+"."+key]
		if !ok {
			continue
		}
		if section == "source" && key == "useragent" && shippedUserAgentRe.MatchString(value) {
			continue // let this release's default through
		}

		// Keep everything up to and including the '=' so the column alignment
		// of the surrounding keys survives.
		lines[i] = body[:eq+1] + " " + value + eol
	}
	return strings.Join(lines, ""), nil
}

// scanINI reads content into a "section.key" -> value map, with section and
// key normalized for lookup and the value taken verbatim (only surrounding
// whitespace trimmed).
//
// Nothing after a value is treated as a comment, matching the
// IgnoreInlineComment: true that Load uses - defaultMappingDCSubnets separates
// its entries with ';', and stripping from the first one would quietly drop
// every site but the first.
//
// A key repeated in the same section keeps the last occurrence, which is what
// ini.v1 resolves to as well.
func scanINI(content string) map[string]string {
	out := make(map[string]string)
	section := ""
	for line := range strings.SplitSeq(content, "\n") {
		trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\r"))

		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = normalizeININame(trimmed[1 : len(trimmed)-1])
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, ";") || strings.HasPrefix(trimmed, "#") {
			continue
		}

		rawKey, rawValue, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		key := normalizeININame(rawKey)
		if key == "" {
			continue
		}
		out[section+"."+key] = strings.TrimSpace(rawValue)
	}
	return out
}

// normalizeININame lower-cases and trims a section or key name, so lookups
// match ini.v1's case-insensitive behaviour.
func normalizeININame(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// writeAtomic replaces the file at path with content via a temp file in the
// same directory and a rename. os.Rename maps to
// MoveFileEx(MOVEFILE_REPLACE_EXISTING), which replaces the destination
// atomically on NTFS - the same guarantee state.Store relies on, so a crash
// mid-write cannot leave a truncated config behind for the next service start
// to fail on. Shared by Merge and SetPrimary, the only two writers of
// config.ini.
func writeAtomic(path, content string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
