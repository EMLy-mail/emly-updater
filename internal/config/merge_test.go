package config

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"emlyupdater/internal/version"
)

// The shape that matters: an aligned '=' column, Italian comments above each
// key, and a value containing ';' (the separator defaultMappingDCSubnets uses).
const mergeDefaultText = `[updater]
; Cartella di installazione di EMLy.
emlyInstallDir       = C:\3gIT\EMLy

; Intervallo di controllo, in minuti.
pollIntervalMinutes  = 30

; Canale forzato.
channelOverride      =

[source]
primary              = external

; User-Agent.
userAgent            = EMLy-Updater/{{VERSION}} (f.fois@3git.eu)

xApiKey              = default_key

defaultMappingDCSubnets = DC-RM2:172.16.96.0/24;DC-CB:172.16.33.0/24

[selfUpdate]
; Nuova sezione introdotta da questa release.
enabled = true
`

func TestMergeINIKeepsExistingValues(t *testing.T) {
	old := `[updater]
emlyInstallDir       = D:\Custom\EMLy
pollIntervalMinutes  = 5
channelOverride      = beta

[source]
primary              = internal
xApiKey              = machine_key
defaultMappingDCSubnets = DC-TS:10.0.0.0/8;DC-MI:10.1.0.0/16
`
	got, err := mergeINI(mergeDefaultText, old)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		`emlyInstallDir       = D:\Custom\EMLy`,
		`pollIntervalMinutes  = 5`,
		`channelOverride      = beta`,
		`primary              = internal`,
		`xApiKey              = machine_key`,
		// The ';' separator must survive: truncating it would silently drop
		// every site but the first.
		`defaultMappingDCSubnets = DC-TS:10.0.0.0/8;DC-MI:10.1.0.0/16`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("merged config is missing %q\n---\n%s", want, got)
		}
	}

	// A key this release added arrives with its default and its comment.
	if !strings.Contains(got, "[selfUpdate]") ||
		!strings.Contains(got, "; Nuova sezione introdotta da questa release.") ||
		!strings.Contains(got, "enabled = true") {
		t.Errorf("new section not carried over from the defaults\n---\n%s", got)
	}

	// Every comment of the default file survives.
	for _, comment := range []string{
		"; Cartella di installazione di EMLy.",
		"; Intervallo di controllo, in minuti.",
		"; Canale forzato.",
	} {
		if !strings.Contains(got, comment) {
			t.Errorf("comment %q was dropped", comment)
		}
	}
}

// Keys the old file carries that this release no longer ships must disappear,
// and sections it no longer has must not reappear.
func TestMergeINIDropsRemovedKeys(t *testing.T) {
	old := `[updater]
emlyInstallDir       = D:\Custom\EMLy
legacyRetryCount     = 7

[obsolete]
somethingOld = yes
`
	got, err := mergeINI(mergeDefaultText, old)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "legacyRetryCount") {
		t.Error("a key no longer shipped survived the merge")
	}
	if strings.Contains(got, "[obsolete]") || strings.Contains(got, "somethingOld") {
		t.Error("a section no longer shipped survived the merge")
	}
	if !strings.Contains(got, `emlyInstallDir       = D:\Custom\EMLy`) {
		t.Error("a still-shipped key lost its machine value")
	}
}

// The version used to be stamped into the shipped userAgent. Carrying that
// value forward would pin a machine's reported version to the build it was
// first installed with.
func TestMergeINIUserAgentMigration(t *testing.T) {
	old := `[source]
userAgent            = EMLy-Updater/1.4.2 (f.fois@3git.eu)
`
	got, err := mergeINI(mergeDefaultText, old)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "EMLy-Updater/1.4.2") {
		t.Errorf("the stale shipped userAgent survived the merge\n---\n%s", got)
	}
	if !strings.Contains(got, "userAgent            = EMLy-Updater/{{VERSION}} (f.fois@3git.eu)") {
		t.Errorf("userAgent was not reset to this release's default\n---\n%s", got)
	}
}

// A userAgent someone actually customised is a machine setting like any other.
func TestMergeINIUserAgentCustomPreserved(t *testing.T) {
	old := `[source]
userAgent            = 3gIT-Fleet/2.0 (helpdesk@3git.eu)
`
	got, err := mergeINI(mergeDefaultText, old)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "userAgent            = 3gIT-Fleet/2.0 (helpdesk@3git.eu)") {
		t.Errorf("a customised userAgent was overwritten\n---\n%s", got)
	}
}

// A value already carrying the placeholder must round-trip untouched.
func TestMergeINIUserAgentPlaceholderPreserved(t *testing.T) {
	old := `[source]
userAgent            = EMLy-Updater/{{VERSION}} (custom@3git.eu)
`
	got, err := mergeINI(mergeDefaultText, old)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "userAgent            = EMLy-Updater/{{VERSION}} (custom@3git.eu)") {
		t.Errorf("a placeholder userAgent was not preserved\n---\n%s", got)
	}
}

// The default file's line endings decide the merged file's, whatever the old
// one used - the output is the default text with values swapped in.
func TestMergeINIPreservesLineEndings(t *testing.T) {
	crlfDefault := strings.ReplaceAll(mergeDefaultText, "\n", "\r\n")
	old := "[updater]\npollIntervalMinutes = 5\n"

	got, err := mergeINI(crlfDefault, old)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "pollIntervalMinutes  = 5\r\n") {
		t.Errorf("CRLF endings were not preserved\n---\n%q", got)
	}
	if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Error("a bare LF leaked into a CRLF file")
	}
}

// Comments and blank lines must never be mistaken for keys, and a commented-out
// key in the old file must not override the shipped default.
func TestMergeINIIgnoresComments(t *testing.T) {
	old := `[updater]
; pollIntervalMinutes  = 999
# channelOverride      = beta

emlyInstallDir       = D:\Custom\EMLy
`
	got, err := mergeINI(mergeDefaultText, old)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "pollIntervalMinutes  = 30") {
		t.Error("a commented-out key overrode the shipped default")
	}
	if !strings.Contains(got, "channelOverride      =\n") {
		t.Error("a commented-out key overrode the shipped empty default")
	}
}

// An empty value in the old file is a real setting ("follow EMLy's channel"),
// not a missing one.
func TestMergeINIPreservesEmptyValues(t *testing.T) {
	defaults := "[updater]\nchannelOverride      = stable\n"
	old := "[updater]\nchannelOverride      =\n"

	got, err := mergeINI(defaults, old)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "channelOverride      = \n") {
		t.Errorf("an explicitly empty value was not preserved\n---\n%q", got)
	}
}

// Section and key names are matched the way ini.v1 resolves them.
func TestMergeINICaseInsensitive(t *testing.T) {
	old := "[UPDATER]\nEMLYINSTALLDIR = D:\\Shouty\n"

	got, err := mergeINI(mergeDefaultText, old)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `emlyInstallDir       = D:\Shouty`) {
		t.Errorf("case-insensitive lookup failed\n---\n%s", got)
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

// ...and it must still hold after an upgrade merged an old machine's config.
func TestMergedUserAgentMatchesTheAPIContract(t *testing.T) {
	merged, err := mergeINI(string(defaultINI), "[source]\nuserAgent = EMLy-Updater/1.0.0 (f.fois@3git.eu)\n")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(writeConfig(t, merged))
	if err != nil {
		t.Fatal(err)
	}

	m := apiUserAgentPattern.FindStringSubmatch(cfg.UserAgent)
	if m == nil {
		t.Fatalf("merged User-Agent %q is not parseable by the API", cfg.UserAgent)
	}
	if m[1] != version.Version {
		t.Errorf("merged User-Agent reports version %q, want the running %q", m[1], version.Version)
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

// The whole point of the merge: a config that has been through it still loads,
// with the machine's values intact.
func TestMergedConfigStillLoads(t *testing.T) {
	old := `[updater]
pollIntervalMinutes  = 5
channelOverride      = beta

[source]
primary              = external
externalManifestURL  = https://api.example/v2/updates/manifest
userAgent            = EMLy-Updater/1.0.0 (f.fois@3git.eu)
`
	merged, err := mergeINI(string(defaultINI), old)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(merged, "{{VERSION}}") {
		t.Fatalf("the stale shipped userAgent was not migrated to the placeholder\n---\n%s", merged)
	}

	path := writeConfig(t, merged)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("merged config does not load: %v", err)
	}

	if cfg.ChannelOverride != "beta" {
		t.Errorf("channelOverride = %q, want beta", cfg.ChannelOverride)
	}
	if cfg.PollInterval.Minutes() != 5 {
		t.Errorf("pollInterval = %v, want 5m", cfg.PollInterval)
	}
	if want := "EMLy-Updater/" + version.Version + " (f.fois@3git.eu)"; cfg.UserAgent != want {
		t.Errorf("userAgent = %q, want %q (the running version, not the one the machine was installed with)",
			cfg.UserAgent, want)
	}
	// The self-update section this release added must be live with its default.
	if !cfg.SelfUpdateEnabled {
		t.Error("selfUpdate.enabled did not default to true after a merge")
	}
}
