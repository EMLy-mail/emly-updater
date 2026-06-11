package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/ini.v1"
)

// ErrEMLyNotInstalled is returned by ReadEMLyConfig when EMLy's config.ini is
// missing or unreadable, which on these machines means EMLy is not installed.
var ErrEMLyNotInstalled = errors.New("EMLy config.ini not found (EMLy not installed)")

// FreshInstallVersion is the installed-version sentinel used when EMLy is not
// installed: every manifest target compares greater, so the channel target is
// installed from scratch.
const FreshInstallVersion = "0.0.0"

// EMLyInfo is the subset of EMLy's config.ini the updater cares about.
type EMLyInfo struct {
	InstalledVersion string // [EMLy].GUI_SEMVER
	Channel          string // [EMLy].GUI_RELEASE_CHANNEL ("stable" or "beta")
	Language         string // [EMLy].LANGUAGE ("it"/"en", fallback "en")
	FreshInstall     bool   // true when config.ini was missing (EMLy not installed)
}

// ReadEMLyConfig parses EMLy's own config.ini. The SDK_DECODER_* keys describe
// a separate component not covered by the update manifest and are ignored.
func ReadEMLyConfig(path string) (EMLyInfo, error) {
	if _, err := os.Stat(path); err != nil {
		return EMLyInfo{}, fmt.Errorf("%w: %v", ErrEMLyNotInstalled, err)
	}

	f, err := ini.Load(path)
	if err != nil {
		return EMLyInfo{}, fmt.Errorf("%w: failed to parse %s: %v", ErrEMLyNotInstalled, path, err)
	}

	sec := f.Section("EMLy")
	info := EMLyInfo{
		InstalledVersion: strings.TrimSpace(sec.Key("GUI_SEMVER").String()),
		Channel:          strings.ToLower(strings.TrimSpace(sec.Key("GUI_RELEASE_CHANNEL").String())),
		Language:         strings.ToLower(strings.TrimSpace(sec.Key("LANGUAGE").String())),
	}

	if info.InstalledVersion == "" {
		return EMLyInfo{}, fmt.Errorf("%w: GUI_SEMVER missing in %s", ErrEMLyNotInstalled, path)
	}
	if info.Channel != "beta" {
		info.Channel = "stable"
	}
	if info.Language == "" {
		info.Language = "en"
	}
	return info, nil
}

// ResolveEMLy reads EMLy's config and applies the updater's policy:
//   - channelOverride, when set, always wins over EMLy's own channel.
//   - a missing/unreadable EMLy config means fresh-install: version 0.0.0,
//     channel = channelOverride or "stable", language "en".
func (c *Config) ResolveEMLy() EMLyInfo {
	info, err := ReadEMLyConfig(c.EMLyConfigFile)
	if err != nil {
		info = EMLyInfo{
			InstalledVersion: FreshInstallVersion,
			Channel:          "stable",
			Language:         "en",
			FreshInstall:     true,
		}
	}
	if c.ChannelOverride != "" {
		info.Channel = c.ChannelOverride
	}
	return info
}
