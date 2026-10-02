package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestApplyEditsKeepsLayout(t *testing.T) {
	in := "[updater]\r\n; comment = not a key\r\npollIntervalMinutes  = 15\r\nchannelOverride      = \r\n\r\n[ipc]\r\nenabled = true\r\n"
	out, err := ApplyEdits([]byte(in), []Edit{
		{Key: "updater.pollIntervalMinutes", Value: "30"},
		{Key: "updater.channelOverride", Value: "beta"},
		{Key: "ipc.enabled", Value: "false"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "[updater]\r\n; comment = not a key\r\npollIntervalMinutes  = 30\r\nchannelOverride      = beta\r\n\r\n[ipc]\r\nenabled = false\r\n"
	if string(out) != want {
		t.Fatalf("got\n%q\nwant\n%q", out, want)
	}
}

func TestApplyEditsClearsValue(t *testing.T) {
	out, err := ApplyEdits([]byte("[updater]\nchannelOverride = beta\n"),
		[]Edit{{Key: "updater.channelOverride", Value: ""}})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != "[updater]\nchannelOverride =\n" {
		t.Fatalf("got %q", got)
	}
}

func TestApplyEditsAddsMissingKeyAndSection(t *testing.T) {
	in := "[updater]\npollIntervalMinutes = 15\n\n[ipc]\nenabled = true\n"
	out, err := ApplyEdits([]byte(in), []Edit{
		{Key: "updater.channelOverride", Value: "stable"},
		{Key: "progressWindow.enabled", Value: "false"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "[updater]\npollIntervalMinutes = 15\nchannelOverride = stable\n\n[ipc]\nenabled = true\n\n[progressWindow]\nenabled = false\n"
	if string(out) != want {
		t.Fatalf("got\n%q\nwant\n%q", out, want)
	}
}

func TestApplyEditsSameKeyOtherSection(t *testing.T) {
	// "enabled" exists in several sections: only the named one changes.
	in := "[selfUpdate]\nenabled = true\n[ipc]\nenabled = true\n[certificate]\nenabled = true\n"
	out, err := ApplyEdits([]byte(in), []Edit{{Key: "ipc.enabled", Value: "false"}})
	if err != nil {
		t.Fatal(err)
	}
	want := "[selfUpdate]\nenabled = true\n[ipc]\nenabled = false\n[certificate]\nenabled = true\n"
	if string(out) != want {
		t.Fatalf("got\n%q\nwant\n%q", out, want)
	}
}

func TestParseEditRejects(t *testing.T) {
	for _, arg := range []string{
		"source.xApiKey=abc",                 // not editable
		"remoteConfig.endpoints=http://evil", // not editable
		"updater.pollIntervalMinutes",        // no '='
		"updater.channelOverride=beta\nx=1",  // multi-line
	} {
		if _, err := ParseEdit(arg); err == nil {
			t.Errorf("ParseEdit(%q) accepted", arg)
		}
	}
	e, err := ParseEdit(" updater.pollIntervalMinutes = 20 ")
	if err != nil || e.Key != "updater.pollIntervalMinutes" || e.Value != "20" {
		t.Fatalf("got %+v, %v", e, err)
	}
}

func TestWriteEditsOnDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.ini")
	if err := os.WriteFile(path, defaultINI, 0644); err != nil {
		t.Fatal(err)
	}
	err := WriteEdits(path, []Edit{
		{Key: "updater.pollIntervalMinutes", Value: "45"},
		{Key: "updater.channelOverride", Value: "beta"},
		{Key: "source.dcLookupRetryAttempts", Value: "2"},
		{Key: "progressWindow.enabled", Value: "false"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollInterval != 45*time.Minute || cfg.ChannelOverride != "beta" ||
		cfg.DCLookupRetryAttempts != 2 || cfg.ProgressWindowEnabled {
		t.Fatalf("edits not applied: %+v", cfg)
	}
	// Everything else is the default, comments included.
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "; Intervallo di controllo aggiornamenti") {
		t.Fatal("comments lost")
	}
}

func TestWriteEditsRejectsInvalidAndKeepsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.ini")
	if err := os.WriteFile(path, defaultINI, 0644); err != nil {
		t.Fatal(err)
	}
	for _, e := range []Edit{
		{Key: "updater.pollIntervalMinutes", Value: "0"},
		{Key: "updater.channelOverride", Value: "nightly"},
		{Key: "source.primary", Value: "both"},
		{Key: "source.dcLookupRetryDelaySeconds", Value: "301"},
	} {
		if err := WriteEdits(path, []Edit{e}); err == nil {
			t.Errorf("%s=%s accepted", e.Key, e.Value)
		}
	}
	data, _ := os.ReadFile(path)
	if string(data) != string(defaultINI) {
		t.Fatal("a rejected edit changed the file")
	}
}
