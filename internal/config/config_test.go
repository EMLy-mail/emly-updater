package config

import (
	"os"
	"path/filepath"
	"testing"
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
	if cfg.Primary != SourceExternal {
		t.Errorf("default primary = %q, want external", cfg.Primary)
	}
	if cfg.ChannelOverride != "" {
		t.Errorf("default channelOverride = %q, want empty", cfg.ChannelOverride)
	}
	if cfg.PollInterval.Minutes() != 30 {
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
