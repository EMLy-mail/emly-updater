package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"emlyupdater/internal/version"
)

func TestLoadCreatesDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.ini")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with missing file failed: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("default config was not written")
	}
	if cfg.Primary != SourceInternal && cfg.Primary != SourceExternal {
		t.Errorf("default primary = %q, want external", cfg.Primary)
	}
	if cfg.ChannelOverride != "" {
		t.Errorf("default channelOverride = %q, want empty", cfg.ChannelOverride)
	}
	if cfg.PollInterval.Minutes() != 30 && cfg.PollInterval.Minutes() != 15 {
		t.Errorf("default poll interval = %v, want 30m", cfg.PollInterval)
	}
	if !cfg.CriticalWarningEnabled || cfg.CriticalWarningSeconds != 30 {
		t.Errorf("default critical warning settings wrong: %+v", cfg)
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.ini")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadIPCDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.ini")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with missing file failed: %v", err)
	}
	if !cfg.IPCEnabled {
		t.Error("default ipc.enabled = false, want true")
	}
	if cfg.IPCPipeName != "EMLyUpdater" {
		t.Errorf("default ipc.pipeName = %q, want EMLyUpdater", cfg.IPCPipeName)
	}
}

func TestValidationRejectsInternalWithoutURL(t *testing.T) {
	path := writeConfig(t, "[source]\nprimary = internal\n")
	if _, err := Load(path); err == nil {
		t.Fatal("expected error: primary=internal with empty internalManifestURL")
	}
}

func TestValidationRejectsBadChannelOverride(t *testing.T) {
	path := writeConfig(t, "[updater]\nchannelOverride = nightly\n[source]\nprimary = external\nexternalManifestURL = https://x\n")
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for channelOverride=nightly")
	}
}

func TestValidationRejectsBadPollInterval(t *testing.T) {
	path := writeConfig(t, "[updater]\npollIntervalMinutes = 0\n[source]\nprimary = external\nexternalManifestURL = https://x\n")
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for pollIntervalMinutes=0")
	}
}

func TestLoadDCLookupRetryDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "config.ini"))
	if err != nil {
		t.Fatalf("Load with missing file failed: %v", err)
	}
	if cfg.DCLookupRetryAttempts != 6 {
		t.Errorf("default dcLookupRetryAttempts = %d, want 6", cfg.DCLookupRetryAttempts)
	}
	if cfg.DCLookupRetryDelay != 5*time.Second {
		t.Errorf("default dcLookupRetryDelaySeconds = %v, want 5s", cfg.DCLookupRetryDelay)
	}
}

// Zero is the documented way to switch the boot-time retry off, so it has to
// load rather than be rejected as out of range.
func TestLoadDCLookupRetryDisabled(t *testing.T) {
	path := writeConfig(t, `[source]
primary = external
externalManifestURL = https://x
dcLookupRetryAttempts = 0
dcLookupRetryDelaySeconds = 0
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DCLookupRetryAttempts != 0 || cfg.DCLookupRetryDelay != 0 {
		t.Errorf("attempts = %d, delay = %v, want both zero", cfg.DCLookupRetryAttempts, cfg.DCLookupRetryDelay)
	}
}

func TestValidationRejectsOutOfRangeDCLookupRetry(t *testing.T) {
	const tmpl = `[source]
primary = external
externalManifestURL = https://x
%s
`

	for _, key := range []string{"dcLookupRetryAttempts = 61", "dcLookupRetryDelaySeconds = 301", "dcLookupRetryDelaySeconds = -1"} {
		if _, err := Load(writeConfig(t, fmt.Sprintf(tmpl, key))); err == nil {
			t.Errorf("expected error for %q", key)
		}
	}
}

func TestValidationRejectsIPCPipeNameWithSlash(t *testing.T) {
	path := writeConfig(t, `[source]
primary = external
externalManifestURL = https://x
[ipc]
enabled = true
pipeName = foo\bar
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for ipc.pipeName containing a backslash")
	}
}

func TestReadEMLyConfig(t *testing.T) {
	path := writeConfig(t, `[EMLy]
GUI_SEMVER = 1.7.5
GUI_RELEASE_CHANNEL = beta
LANGUAGE = it
SDK_DECODER_SEMVER = 1.5.4
`)
	info, err := ReadEMLyConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.InstalledVersion != "1.7.5" || info.Channel != "beta" || info.Language != "it" {
		t.Fatalf("unexpected EMLy info: %+v", info)
	}
}

func TestReadEMLyConfigDefaults(t *testing.T) {
	// Unknown channel collapses to stable, missing language to en.
	path := writeConfig(t, "[EMLy]\nGUI_SEMVER = 1.0.0\nGUI_RELEASE_CHANNEL = canary\n")
	info, err := ReadEMLyConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Channel != "stable" || info.Language != "en" {
		t.Fatalf("unexpected defaults: %+v", info)
	}
}

func TestReadEMLyConfigMissing(t *testing.T) {
	_, err := ReadEMLyConfig(filepath.Join(t.TempDir(), "nope.ini"))
	if err == nil {
		t.Fatal("expected ErrEMLyNotInstalled")
	}
}

func TestResolveEMLyFreshInstall(t *testing.T) {
	cfg := &Config{EMLyConfigFile: filepath.Join(t.TempDir(), "missing.ini")}

	info := cfg.ResolveEMLy()
	if !info.FreshInstall || info.InstalledVersion != FreshInstallVersion {
		t.Fatalf("expected fresh-install 0.0.0, got %+v", info)
	}
	if info.Channel != "stable" || info.Language != "en" {
		t.Fatalf("fresh-install defaults wrong: %+v", info)
	}

	// channelOverride wins even in fresh-install mode.
	cfg.ChannelOverride = "beta"
	if info := cfg.ResolveEMLy(); info.Channel != "beta" {
		t.Fatalf("channelOverride ignored: %+v", info)
	}
}

func TestResolveEMLyOverrideWins(t *testing.T) {
	path := writeConfig(t, "[EMLy]\nGUI_SEMVER = 1.7.5\nGUI_RELEASE_CHANNEL = beta\n")
	cfg := &Config{EMLyConfigFile: path, ChannelOverride: "stable"}
	if info := cfg.ResolveEMLy(); info.Channel != "stable" {
		t.Fatalf("channelOverride did not win: %+v", info)
	}
}

func TestLoadCertificateDefaultsEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.ini")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !cfg.CertificateEnabled {
		t.Error("certificate install should default to enabled")
	}
}

// The ';' separator between DC entries in defaultMappingDCSubnets must
// survive ini.v1's own parsing, not just ParseDCSubnetMap: ini.v1 treats a
// bare ';' in a value as an inline comment by default and would silently
// truncate the line at "DC-RM2:...", dropping every site listed after it.
func TestLoadPreservesEverySiteInDCSubnetMap(t *testing.T) {
	path := writeConfig(t, "[source]\nprimary = external\nexternalManifestURL = https://x\n"+
		"defaultMappingDCSubnets = DC-RM2:172.16.96.0/24;DC-CB:172.16.33.0/24,172.16.34.0/24\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(cfg.DCSubnetMap) != 2 {
		t.Fatalf("DCSubnetMap = %v, want 2 DCs (ini.v1 likely truncated the value at ';')", cfg.DCSubnetMap)
	}
	if len(cfg.DCSubnetMap["DC-CB"]) != 2 {
		t.Errorf("DC-CB subnets = %v, want 2 entries", cfg.DCSubnetMap["DC-CB"])
	}
}

func TestLoadCertificateDisabled(t *testing.T) {
	path := writeConfig(t, `
[source]
primary = internal
internalManifestURL = http://example.invalid/v2/updates/manifest

[certificate]
enabled = false
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.CertificateEnabled {
		t.Error("certificate.enabled = false was not honoured")
	}
}

// apiUserAgentPattern is emly-go-api's updaterUAPattern verbatim
// (internal/handlers/updates.route.go). The API parses the machine's installed
// version out of the User-Agent to record update events and to stage rollouts,
// so a User-Agent this does not match is telemetry silently lost fleet-wide.
//
// Worth pinning here because `{{VERSION}}` itself does *not* match `[\w.\-]+`:
// if the placeholder ever stopped being resolved, every machine would report
// an unknown version and nothing else would fail.
var apiUserAgentPattern = regexp.MustCompile(`^EMLy-Updater/([\w.\-]+)\s*\(([^)]*)\)`)

func TestDefaultUserAgentMatchesTheAPIContract(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "config.ini"))
	if err != nil {
		t.Fatal(err)
	}

	m := apiUserAgentPattern.FindStringSubmatch(cfg.UserAgent)
	if m == nil {
		t.Fatalf("User-Agent %q is not parseable by the API", cfg.UserAgent)
	}
	if m[1] != version.Version {
		t.Errorf("User-Agent reports version %q, want %q", m[1], version.Version)
	}
	if m[2] == "" {
		t.Error("User-Agent carries no contact")
	}
}

// Every source has to be able to name its own updater endpoint, so a machine
// on a site's internal mirror self-updates from that mirror rather than
// reaching for the internet.
func TestUpdaterManifestURL(t *testing.T) {
	cfg := &Config{}
	cases := map[string]string{
		"https://api.emly.ffois.it/v2/updates/manifest":  "https://api.emly.ffois.it/v2/updates/manifest/updater",
		"http://172.16.33.72:8080/v2/updates/manifest":   "http://172.16.33.72:8080/v2/updates/manifest/updater",
		"https://api.example/v2/updates/manifest/":       "https://api.example/v2/updates/manifest/updater",
		"https://api.example/v2/updates/manifest?ring=1": "https://api.example/v2/updates/manifest/updater?ring=1",
	}
	for in, want := range cases {
		got, err := cfg.UpdaterManifestURL(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("UpdaterManifestURL(%s) = %s, want %s", in, got, want)
		}
	}

	if _, err := cfg.UpdaterManifestURL(""); err == nil {
		t.Error("expected an error when there is no manifest URL to derive from")
	}
}

// An explicit override names one host, so it wins for every source rather
// than being re-derived per source.
func TestUpdaterManifestURLOverride(t *testing.T) {
	cfg := &Config{SelfUpdateManifestURL: "http://localhost:8000/updater"}
	for _, base := range []string{"https://api.emly.ffois.it/v2/updates/manifest", ""} {
		got, err := cfg.UpdaterManifestURL(base)
		if err != nil {
			t.Fatal(err)
		}
		if got != "http://localhost:8000/updater" {
			t.Errorf("override ignored for base %q: got %s", base, got)
		}
	}
}

func TestLoadSelfUpdateDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "config.ini"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SelfUpdateEnabled {
		t.Error("selfUpdate.enabled should default to true")
	}
	if cfg.SelfUpdateManifestURL != "" {
		t.Errorf("selfUpdate.manifestURL = %q, want empty (derived)", cfg.SelfUpdateManifestURL)
	}
}
