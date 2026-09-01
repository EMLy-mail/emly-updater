package config

import (
	_ "embed"
	"emlyupdater/internal/version"
	"fmt"
	"net"
	"net/url"
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
	// BackupInternalManifestURL (ini key bkInternManifestURL) is an optional
	// second internal endpoint. When the source policy placed the machine on
	// a mapped internal LAN (DC and subnets matched) but InternalManifestURL
	// does not answer, this URL is tried before falling back to
	// ExternalManifestURL. Empty disables it.
	BackupInternalManifestURL string
	UserAgent                 string // optional User-Agent header for HTTP requests
	APIKey                    string // optional X-Api-Key header for HTTP requests

	// DCSubnetMap drives the startup source policy: keyed by domain
	// controller name, each entry lists the CIDR subnets that count as "in
	// that site's office" for it. When the nearest domain controller matches
	// a key here *and* one of this machine's own local IPs falls inside one
	// of that key's subnets, the machine is on that site's internal LAN and
	// Primary is forced to "internal"; anything else (DC not in the map, no
	// local IP in its subnets, machine off the domain) forces "external". A
	// nil/empty map disables the check entirely and Primary is used as
	// written.
	DCSubnetMap map[string][]*net.IPNet

	// DCLookupRetryAttempts and DCLookupRetryDelay bound the retry the source
	// policy performs when the domain controller lookup fails at startup. At
	// boot the service can win the race against netlogon/DNS by a second or
	// two, and DsGetDcName then reports the domain as non-existent - which
	// the policy would otherwise read as "this machine is off-site" and pin
	// it to the external source for the rest of the run. Either value at zero
	// disables retrying (a single attempt), and the retry is skipped entirely
	// on a machine that is not domain-joined, where the failure is the truth.
	DCLookupRetryAttempts int
	DCLookupRetryDelay    time.Duration

	// [fileAssociations]
	ProgIDEml string
	ProgIDMsg string

	// [criticalUpdate]
	CriticalWarningEnabled bool
	CriticalWarningSeconds int

	// [selfUpdate]
	SelfUpdateEnabled bool
	// SelfUpdateManifestURL overrides the updater manifest URL. Empty - the
	// normal case - means "derive it from whichever manifest URL is in use",
	// see UpdaterManifestURL.
	SelfUpdateManifestURL string

	// [ipc]
	IPCEnabled  bool
	IPCPipeName string

	// [certificate]
	CertificateEnabled bool
}

// PrimaryManifestURL returns the manifest URL selected by Primary.
func (c *Config) PrimaryManifestURL() string {
	if c.Primary == SourceInternal {
		return c.InternalManifestURL
	}
	return c.ExternalManifestURL
}

// UpdaterManifestURL returns the URL of the updater's own release manifest for
// a given EMLy manifest URL, by appending the "updater" path segment:
// ".../v2/updates/manifest" becomes ".../v2/updates/manifest/updater".
//
// Deriving it means every source keeps working without a second URL to
// configure and keep in sync: a machine on a site's internal mirror asks that
// mirror for the updater release too, and the external fallback derives its own
// URL the same way. An explicit selfUpdate.manifestURL overrides the derivation
// for every source - it names one specific host, so it cannot be re-pointed at
// a fallback.
func (c *Config) UpdaterManifestURL(manifestURL string) (string, error) {
	if c.SelfUpdateManifestURL != "" {
		return c.SelfUpdateManifestURL, nil
	}
	if manifestURL == "" {
		return "", fmt.Errorf("cannot derive the updater manifest URL from an empty manifest URL")
	}
	u, err := url.Parse(manifestURL)
	if err != nil {
		return "", fmt.Errorf("cannot derive the updater manifest URL from %q: %w", manifestURL, err)
	}
	// JoinPath escapes the segment and normalises a trailing slash, and keeps
	// any query string the manifest URL carries.
	return u.JoinPath("updater").String(), nil
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

// BuildUserAgent replaces the {{VERSION}} placeholder in rawUAString with ver.
func BuildUserAgent(rawUAString, ver string) string {
	return strings.ReplaceAll(rawUAString, "{{VERSION}}", ver)
}

// Load reads, defaults, and validates the updater configuration at path.
// A missing file is created from the embedded defaults first.
func Load(path string) (*Config, error) {
	if _, err := WriteDefault(path); err != nil {
		return nil, err
	}

	// IgnoreInlineComment: without it, ini.v1 treats a bare ';' anywhere in a
	// value as the start of an inline comment and silently truncates
	// everything after it - which is exactly the separator
	// defaultMappingDCSubnets uses between DC entries. A config with two
	// mapped sites would otherwise lose every site after the first with no
	// error and no hint why the DC just isn't "found".
	f, err := ini.LoadSources(ini.LoadOptions{IgnoreInlineComment: true}, path)
	if err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", path, err)
	}

	upd := f.Section("updater")
	src := f.Section("source")
	fa := f.Section("fileAssociations")
	crit := f.Section("criticalUpdate")
	ipcSec := f.Section("ipc")
	certSec := f.Section("certificate")
	selfSec := f.Section("selfUpdate")

	cfg := &Config{
		EMLyInstallDir:  upd.Key("emlyInstallDir").MustString(`C:\3gIT\EMLy`),
		EMLyExeName:     upd.Key("emlyExeName").MustString("EMLy.exe"),
		EMLyConfigFile:  upd.Key("emlyConfigFile").MustString(`C:\3gIT\EMLy\config.ini`),
		ChannelOverride: strings.ToLower(strings.TrimSpace(upd.Key("channelOverride").String())),

		Primary:             strings.ToLower(strings.TrimSpace(src.Key("primary").MustString(SourceExternal))),
		ExternalManifestURL: strings.TrimSpace(src.Key("externalManifestURL").String()),
		InternalManifestURL: strings.TrimSpace(src.Key("internalManifestURL").String()),

		BackupInternalManifestURL: strings.TrimSpace(src.Key("bkInternManifestURL").String()),
		UserAgent:                 BuildUserAgent(strings.TrimSpace(src.Key("userAgent").String()), version.Version),
		APIKey:                    strings.TrimSpace(src.Key("xApiKey").String()),

		ProgIDEml: fa.Key("progIdEml").MustString("EMLy.EML"),
		ProgIDMsg: fa.Key("progIdMsg").MustString("EMLy.MSG"),

		CriticalWarningEnabled: crit.Key("criticalWarningEnabled").MustBool(true),
		CriticalWarningSeconds: crit.Key("criticalWarningSeconds").MustInt(30),

		IPCEnabled:  ipcSec.Key("enabled").MustBool(true),
		IPCPipeName: strings.TrimSpace(ipcSec.Key("pipeName").MustString("EMLyUpdater")),

		CertificateEnabled: certSec.Key("enabled").MustBool(true),

		SelfUpdateEnabled:     selfSec.Key("enabled").MustBool(true),
		SelfUpdateManifestURL: strings.TrimSpace(selfSec.Key("manifestURL").String()),
	}

	retryAttempts := src.Key("dcLookupRetryAttempts").MustInt(6)
	if retryAttempts < 0 || retryAttempts > 60 {
		return nil, fmt.Errorf("dcLookupRetryAttempts must be between 0 and 60, got %d", retryAttempts)
	}
	cfg.DCLookupRetryAttempts = retryAttempts

	retrySeconds := src.Key("dcLookupRetryDelaySeconds").MustInt(5)
	if retrySeconds < 0 || retrySeconds > 300 {
		return nil, fmt.Errorf("dcLookupRetryDelaySeconds must be between 0 and 300, got %d", retrySeconds)
	}
	cfg.DCLookupRetryDelay = time.Duration(retrySeconds) * time.Second

	minutes := upd.Key("pollIntervalMinutes").MustInt(30)
	if minutes < 1 {
		return nil, fmt.Errorf("pollIntervalMinutes must be >= 1, got %d", minutes)
	}
	cfg.PollInterval = time.Duration(minutes) * time.Minute

	dcSubnets, err := ParseDCSubnetMap(src.Key("defaultMappingDCSubnets").String())
	if err != nil {
		return nil, err
	}
	cfg.DCSubnetMap = dcSubnets

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

	if c.IPCEnabled {
		if c.IPCPipeName == "" || strings.ContainsAny(c.IPCPipeName, `\/`) {
			return fmt.Errorf("ipc.pipeName must be non-empty and must not contain '\\' or '/', got %q", c.IPCPipeName)
		}
	}
	return nil
}
