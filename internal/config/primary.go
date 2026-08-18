package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// ParseSubnets parses a comma-separated list of CIDR blocks (the
// internalDCSubnets key). An empty or blank string yields no subnets, which
// disables the startup source policy rather than being an error. A malformed
// entry is an error: silently ignoring it would leave every machine on the
// "not internal" branch with nothing in the log to explain why.
func ParseSubnets(raw string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for field := range strings.SplitSeq(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		_, n, err := net.ParseCIDR(field)
		if err != nil {
			return nil, fmt.Errorf("internalDCSubnets: %q is not a valid CIDR block: %w", field, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// SubnetsContain reports whether ip (as text) falls inside any of subnets.
// A malformed or non-IP string is never contained.
func SubnetsContain(subnets []*net.IPNet, ip string) bool {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return false
	}
	for _, n := range subnets {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

// SetPrimary rewrites the `primary` key of the [source] section in the config
// file at path, leaving every other byte of the file untouched.
//
// It deliberately does not go through ini.SaveTo: that re-serialises the whole
// file and would drop the column alignment and the Italian comments that make
// config.default.ini readable to whoever edits it on a machine.
func SetPrimary(path, value string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	updated, err := setPrimaryIn(string(raw), value)
	if err != nil {
		return err
	}
	if updated == string(raw) {
		return nil
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(updated); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}

	// os.Rename maps to MoveFileEx(MOVEFILE_REPLACE_EXISTING), which replaces
	// the destination atomically on NTFS - the same guarantee state.Store
	// relies on, so a crash mid-write cannot leave a truncated config.
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// setPrimaryIn is the pure half of SetPrimary: it returns content with the
// [source] primary key set to value. Split out so the line surgery can be
// tested without touching the disk. When the key is absent it is inserted
// right below the [source] header; a file with no [source] section at all is
// an error rather than a guess.
func setPrimaryIn(content, value string) (string, error) {
	lines := strings.SplitAfter(content, "\n")

	section := ""
	inserted := -1
	for i, line := range lines {
		body, eol := splitEOL(line)
		trimmed := strings.TrimSpace(body)

		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.ToLower(strings.TrimSpace(trimmed[1 : len(trimmed)-1]))
			if section == "source" {
				inserted = i
			}
			continue
		}
		if section != "source" || strings.HasPrefix(trimmed, ";") || strings.HasPrefix(trimmed, "#") {
			continue
		}

		eq := strings.Index(body, "=")
		if eq < 0 || !strings.EqualFold(strings.TrimSpace(body[:eq]), "primary") {
			continue
		}
		// Keep everything up to and including the '=' so the column
		// alignment of the surrounding keys survives.
		lines[i] = body[:eq+1] + " " + value + eol
		return strings.Join(lines, ""), nil
	}

	if inserted < 0 {
		return "", fmt.Errorf("no [source] section found")
	}
	_, eol := splitEOL(lines[inserted])
	if eol == "" {
		// [source] was the last line and had no terminator: give it one.
		eol = "\n"
		lines[inserted] += eol
	}
	lines[inserted] += "primary = " + value + eol
	return strings.Join(lines, ""), nil
}

// splitEOL separates a line's text from its terminator, so a rewritten line
// keeps the file's existing CRLF or LF endings instead of imposing one.
func splitEOL(line string) (body, eol string) {
	switch {
	case strings.HasSuffix(line, "\r\n"):
		return line[:len(line)-2], "\r\n"
	case strings.HasSuffix(line, "\n"):
		return line[:len(line)-1], "\n"
	}
	return line, ""
}
