// Package manifest defines the update manifest structure shared by the HTTP
// API and the UNC share, plus channel/target resolution and semver helpers.
package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"

	goversion "github.com/hashicorp/go-version"
)

// Manifest mirrors the JSON served by /v2/updates/manifest and by version.json
// on the UNC share. The two differ only in conventions:
//   - API: StableDownload/BetaDownload are full URLs, SHA256Checksums is keyed
//     by version string.
//   - UNC: the download fields are filenames relative to the share root and
//     SHA256Checksums is keyed by filename.
//
// MinRequiredVersion may be absent in legacy UNC manifests (empty string =
// no forced minimum).
type Manifest struct {
	StableVersion        string                  `json:"stableVersion"`
	BetaVersion          string                  `json:"betaVersion"`
	StableDownload       string                  `json:"stableDownload"`
	BetaDownload         string                  `json:"betaDownload"`
	IsCritical           bool                    `json:"isCritical"`
	MinRequiredVersion   string                  `json:"minRequiredVersion"`
	SHA256Checksums      map[string]string       `json:"sha256Checksums"`
	ReleaseNotes         map[string]string       `json:"releaseNotes,omitempty"`
	DetailedReleaseNotes map[string]DetailedNote `json:"detailedReleaseNotes,omitempty"`
}

// DetailedNote holds per-version structured release note data. SeverityType is
// informational only ("bugfix" | "feature") and never drives update behavior.
type DetailedNote struct {
	SeverityType string            `json:"severityType"`
	Description  map[string]string `json:"description"` // "en", "it"
}

// Target is a fully resolved update target for one channel. DownloadRef is a
// full URL for HTTP sources or an absolute share path for the UNC source; the
// resolving source fills SHA256 using its own checksum-key convention.
type Target struct {
	Version     string
	DownloadRef string
	SHA256      string
}

// Parse decodes manifest JSON, tolerating a UTF-8 BOM (files saved by Windows
// editors on the share may include one).
func Parse(data []byte) (*Manifest, error) {
	data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))

	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("failed to parse manifest JSON: %w", err)
	}
	if m.StableVersion == "" || m.StableDownload == "" {
		return nil, fmt.Errorf("invalid manifest: missing stable version or download")
	}
	return &m, nil
}

// ChannelVersion returns the manifest's target version and download reference
// for the given channel. Anything that is not "beta" resolves to stable.
func (m *Manifest) ChannelVersion(channel string) (version, downloadRef string, err error) {
	if channel == "beta" {
		version, downloadRef = m.BetaVersion, m.BetaDownload
	} else {
		version, downloadRef = m.StableVersion, m.StableDownload
	}
	if version == "" || downloadRef == "" {
		return "", "", fmt.Errorf("manifest has no target for channel %q", channel)
	}
	return version, downloadRef, nil
}

// Less reports whether version a is strictly older than version b.
// Manifest versions carry no "v" prefix; hashicorp/go-version handles both.
func Less(a, b string) (bool, error) {
	va, err := goversion.NewVersion(a)
	if err != nil {
		return false, fmt.Errorf("invalid version %q: %w", a, err)
	}
	vb, err := goversion.NewVersion(b)
	if err != nil {
		return false, fmt.Errorf("invalid version %q: %w", b, err)
	}
	return va.LessThan(vb), nil
}

// Forced reports whether the update from installed must be applied even while
// EMLy is running: either the manifest is flagged critical or the installed
// version has fallen below the global minimum. An empty MinRequiredVersion
// (legacy UNC manifests) imposes no minimum.
func (m *Manifest) Forced(installed string) (bool, error) {
	if m.IsCritical {
		return true, nil
	}
	if m.MinRequiredVersion == "" {
		return false, nil
	}
	belowMin, err := Less(installed, m.MinRequiredVersion)
	if err != nil {
		return false, err
	}
	return belowMin, nil
}
