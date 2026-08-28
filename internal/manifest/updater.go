package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// UpdaterManifest mirrors the JSON served by /v2/updates/manifest/updater: the
// updater's own release, used by the self-update path.
//
// It is deliberately much thinner than Manifest. There is no channel and no
// forced flag: a self-update has no user to interrupt, so it is always silent
// and always applied as soon as it is seen, and a per-machine staged rollout is
// done server-side (the API sees this machine's User-Agent, hostname and HWID
// on every request) by simply not offering the release yet.
//
// Version empty means "no updater release published" - a normal, silent no-op
// rather than an error, and the API's kill switch for a bad release.
//
// Unknown fields are ignored, so the API can grow the document without
// breaking updaters already in the field.
type UpdaterManifest struct {
	Version      string            `json:"version"`
	Download     string            `json:"download"`
	SHA256       string            `json:"sha256"`
	Size         int64             `json:"size,omitempty"`
	PublishedAt  string            `json:"publishedAt,omitempty"`
	ReleaseNotes map[string]string `json:"releaseNotes,omitempty"`
}

// ParseUpdater decodes the updater manifest, tolerating a UTF-8 BOM the same
// way Parse does.
//
// A document offering a release is validated here rather than at the point of
// use: an entry missing its download URL or checksum can never lead to an
// install (a setup is never executed without a matching SHA256), so it is a
// server-side mistake worth reporting loudly instead of a silent no-op.
func ParseUpdater(data []byte) (*UpdaterManifest, error) {
	data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))

	var m UpdaterManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("failed to parse updater manifest JSON: %w", err)
	}

	m.Version = strings.TrimSpace(m.Version)
	m.Download = strings.TrimSpace(m.Download)
	m.SHA256 = strings.TrimSpace(m.SHA256)

	if m.Version == "" {
		return &m, nil // nothing published; not an error
	}
	if m.Download == "" {
		return nil, fmt.Errorf("updater manifest offers version %s with no download URL", m.Version)
	}
	if m.SHA256 == "" {
		return nil, fmt.Errorf("updater manifest offers version %s with no SHA256 checksum", m.Version)
	}
	return &m, nil
}

// Offered reports whether the manifest actually carries a release.
func (m *UpdaterManifest) Offered() bool { return m != nil && m.Version != "" }

// Target converts the manifest into the same Target the EMLy download path
// uses, so download.Manager can fetch and verify it unchanged.
func (m *UpdaterManifest) Target() Target {
	return Target{Version: m.Version, DownloadRef: m.Download, SHA256: m.SHA256}
}

// Notes returns the release note for lang, falling back to English and then to
// any note present. Purely informational: it is logged, never shown to a user.
func (m *UpdaterManifest) Notes(lang string) string {
	if len(m.ReleaseNotes) == 0 {
		return ""
	}
	if note, ok := m.ReleaseNotes[lang]; ok && note != "" {
		return note
	}
	if note, ok := m.ReleaseNotes["en"]; ok && note != "" {
		return note
	}
	for _, note := range m.ReleaseNotes {
		if note != "" {
			return note
		}
	}
	return ""
}
