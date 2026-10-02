package config

import (
	"fmt"
	"os"
	"slices"
	"strings"
)

// Edit sets one key of config.ini. Key is "section.key", as in EditableKeys.
type Edit struct {
	Key   string
	Value string
}

// EditableKeys are the only keys the tray's settings window may change,
// through the elevated `apply-settings` subcommand. An allowlist and not
// "any key": apply-settings runs as an administrator on whatever command line
// it is given, and the URLs, API key and pipe name are not something a
// settings window should be able to re-point.
var EditableKeys = []string{
	"updater.pollIntervalMinutes",
	"updater.channelOverride",
	"remoteConfig.enabled",
	"source.primary",
	"source.dcLookupRetryAttempts",
	"source.dcLookupRetryDelaySeconds",
	"selfUpdate.enabled",
	"criticalUpdate.criticalWarningEnabled",
	"criticalUpdate.criticalWarningSeconds",
	"ipc.enabled",
	"certificate.enabled",
	"progressWindow.enabled",
}

// ParseEdit parses "section.key=value" and checks the key is editable.
func ParseEdit(arg string) (Edit, error) {
	key, value, ok := strings.Cut(arg, "=")
	if !ok {
		return Edit{}, fmt.Errorf("%q: expected section.key=value", arg)
	}
	e := Edit{Key: strings.TrimSpace(key), Value: strings.TrimSpace(value)}
	if !isEditable(e.Key) {
		return Edit{}, fmt.Errorf("%q is not an editable key", e.Key)
	}
	if strings.ContainsAny(e.Value, "\r\n") {
		return Edit{}, fmt.Errorf("%s: the value must be a single line", e.Key)
	}
	return e, nil
}

func isEditable(key string) bool { return slices.Contains(EditableKeys, key) }

// ApplyEdits returns data with every edit applied, changing only the value
// part of each key's line: comments, ordering, alignment and line endings
// are kept, so the file still reads like config.default.ini afterwards. A key
// missing from its section is appended at the end of that section; a missing
// section is appended at the end of the file.
func ApplyEdits(data []byte, edits []Edit) ([]byte, error) {
	text := string(data)
	eol := "\n"
	if strings.Contains(text, "\r\n") {
		eol = "\r\n"
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")

	for _, e := range edits {
		if !isEditable(e.Key) {
			return nil, fmt.Errorf("%q is not an editable key", e.Key)
		}
		if strings.ContainsAny(e.Value, "\r\n") {
			return nil, fmt.Errorf("%s: the value must be a single line", e.Key)
		}
		section, key, _ := strings.Cut(e.Key, ".")
		lines = setKey(lines, section, key, e.Value)
	}
	return []byte(strings.Join(lines, eol)), nil
}

// setKey sets section.key = value in lines (no line terminators).
func setKey(lines []string, section, key, value string) []string {
	inSection := false
	sectionEnd := -1 // index after the section's last non-blank line
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			if inSection {
				break
			}
			inSection = strings.EqualFold(strings.TrimSpace(trimmed[1:len(trimmed)-1]), section)
			if inSection {
				sectionEnd = i + 1
			}
			continue
		}
		if !inSection {
			continue
		}
		if trimmed != "" {
			sectionEnd = i + 1
		}
		if strings.HasPrefix(trimmed, ";") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		left, _, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(left), key) {
			continue
		}
		// Keep "key   =" exactly as written, alignment included.
		lines[i] = left + "= " + value
		if value == "" {
			lines[i] = left + "="
		}
		return lines
	}

	entry := key + " = " + value
	if sectionEnd < 0 {
		// Section absent: append it, separated by a blank line.
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		return append(lines, "", "["+section+"]", entry, "")
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:sectionEnd]...)
	out = append(out, entry)
	return append(out, lines[sectionEnd:]...)
}

// WriteEdits applies edits to the config file at path, validates the result
// exactly as the service will load it, and replaces the file atomically. On
// any error the file is left untouched.
//
// The service reads config.ini only at startup, so the caller restarts it for
// the edit to take effect - and config.Reset rewrites the file from defaults
// on the next install or self-update, so an edit lasts until then.
func WriteEdits(path string, edits []Edit) error {
	// A missing file is the defaults, exactly as Load sees it.
	if _, err := WriteDefault(path); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := ApplyEdits(data, edits)
	if err != nil {
		return err
	}
	if _, err := Parse(out); err != nil {
		return fmt.Errorf("the edited configuration is not valid: %w", err)
	}
	return writeAtomic(path, string(out))
}
