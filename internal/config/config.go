package config

import (
	_ "embed"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/ini.v1"
)

// defaultINI is the configuration written to ProgramData on first start (or by
// the `install` subcommand) when no config.ini exists yet. Embedding it keeps a
// single source of truth: the updater's installer does not ship a config file.
//
//go:embed config.default.ini
var defaultINI []byte

// Source selection values for Config.Primary.
const (
	SourceExternal = "external"
	SourceInternal = "internal"
)

// Config holds the validated updater configuration.
type Config struct {
	// [updater]
	EMLyInstallDir  string
	EMLyExeName     string
	EMLyConfigFile  string
	PollInterval    time.Duration
	ChannelOverride string // "", "stable" or "beta"

	// [source]
	Primary             string // "external" or "internal"
	ExternalManifestURL string
	InternalManifestURL string
	UNCRoot             string
	UserAgent           string // optional User-Agent header for HTTP requests
	APIKey              string // optional X-Api-Key header for HTTP requests

	// [fileAssociations]
	ProgIDEml string
	ProgIDMsg string

	// [criticalUpdate]
	CriticalWarningEnabled bool
	CriticalWarningSeconds int
}

// PrimaryManifestURL returns the manifest URL selected by Primary.
func (c *Config) PrimaryManifestURL() string {
	if c.Primary == SourceInternal {
		return c.InternalManifestURL
	}
	return c.ExternalManifestURL
}

// WriteDefault writes the embedded default configuration to path if the file
// does not exist yet. Returns true when a new file was written.
func WriteDefault(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := os.WriteFile(path, defaultINI, 0644); err != nil {
		return false, fmt.Errorf("failed to write default config: %w", err)
	}
	return true, nil
}

// Load reads, defaults, and validates the updater configuration at path.
// A missing file is created from the embedded defaults first.
func Load(path string) (*Config, error) {
	if _, err := WriteDefault(path); err != nil {
		return nil, err
	}

	f, err := ini.Load(path)
	if err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", path, err)
	}

	upd := f.Section("updater")
	src := f.Section("source")
	fa := f.Section("fileAssociations")
	crit := f.Section("criticalUpdate")

	cfg := &Config{
		EMLyInstallDir:  upd.Key("emlyInstallDir").MustString(`C:\3gIT\EMLy`),
		EMLyExeName:     upd.Key("emlyExeName").MustString("EMLy.exe"),
		EMLyConfigFile:  upd.Key("emlyConfigFile").MustString(`C:\3gIT\EMLy\config.ini`),
		ChannelOverride: strings.ToLower(strings.TrimSpace(upd.Key("channelOverride").String())),

		Primary:             strings.ToLower(strings.TrimSpace(src.Key("primary").MustString(SourceExternal))),
		ExternalManifestURL: strings.TrimSpace(src.Key("externalManifestURL").String()),
		InternalManifestURL: strings.TrimSpace(src.Key("internalManifestURL").String()),
		UNCRoot:             strings.TrimSpace(src.Key("uncRoot").String()),
		UserAgent:           strings.TrimSpace(src.Key("userAgent").String()),
		APIKey:              strings.TrimSpace(src.Key("xApiKey").String()),

		ProgIDEml: fa.Key("progIdEml").MustString("EMLy.EML"),
		ProgIDMsg: fa.Key("progIdMsg").MustString("EMLy.MSG"),

		CriticalWarningEnabled: crit.Key("criticalWarningEnabled").MustBool(true),
		CriticalWarningSeconds: crit.Key("criticalWarningSeconds").MustInt(30),
	}

	minutes := upd.Key("pollIntervalMinutes").MustInt(30)
	if minutes < 1 {
		return nil, fmt.Errorf("pollIntervalMinutes must be >= 1, got %d", minutes)
	}
	cfg.PollInterval = time.Duration(minutes) * time.Minute

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	switch c.Primary {
	case SourceExternal:
		if c.ExternalManifestURL == "" {
			return fmt.Errorf("primary is %q but externalManifestURL is empty", c.Primary)
		}
	case SourceInternal:
		if c.InternalManifestURL == "" {
			return fmt.Errorf("primary is %q but internalManifestURL is empty", c.Primary)
		}
	default:
		return fmt.Errorf("primary must be %q or %q, got %q", SourceExternal, SourceInternal, c.Primary)
	}

	switch c.ChannelOverride {
	case "", "stable", "beta":
	default:
		return fmt.Errorf("channelOverride must be empty, \"stable\" or \"beta\", got %q", c.ChannelOverride)
	}

	if c.EMLyInstallDir == "" || c.EMLyExeName == "" || c.EMLyConfigFile == "" {
		return fmt.Errorf("emlyInstallDir, emlyExeName and emlyConfigFile must not be empty")
	}
	if c.CriticalWarningSeconds < 1 {
		return fmt.Errorf("criticalWarningSeconds must be >= 1, got %d", c.CriticalWarningSeconds)
	}
	return nil
}
