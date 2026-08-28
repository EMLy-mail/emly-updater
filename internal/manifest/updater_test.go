package manifest

import "testing"

const sampleUpdaterJSON = `{
  "version": "1.5.0",
  "download": "https://api.example/v2/updates/download/updater/1.5.0",
  "sha256": "3f786850e387550fdab836ed7e6dc881de23001b1f8b9a3d6e5c4a2b0f9e8d7c",
  "size": 4812345,
  "publishedAt": "2026-08-28T09:00:00Z",
  "releaseNotes": {"it": "Note italiane", "en": "English notes"}
}`

func TestParseUpdater(t *testing.T) {
	m, err := ParseUpdater([]byte(sampleUpdaterJSON))
	if err != nil {
		t.Fatalf("ParseUpdater failed: %v", err)
	}
	if !m.Offered() {
		t.Fatal("expected the manifest to offer a release")
	}
	if m.Version != "1.5.0" || m.Size != 4812345 {
		t.Fatalf("unexpected manifest: %+v", m)
	}

	target := m.Target()
	if target.Version != "1.5.0" ||
		target.DownloadRef != "https://api.example/v2/updates/download/updater/1.5.0" ||
		target.SHA256 != "3f786850e387550fdab836ed7e6dc881de23001b1f8b9a3d6e5c4a2b0f9e8d7c" {
		t.Fatalf("unexpected target: %+v", target)
	}
}

func TestParseUpdaterWithBOM(t *testing.T) {
	data := append([]byte("\xEF\xBB\xBF"), []byte(sampleUpdaterJSON)...)
	m, err := ParseUpdater(data)
	if err != nil {
		t.Fatalf("ParseUpdater with BOM failed: %v", err)
	}
	if m.Version != "1.5.0" {
		t.Fatalf("unexpected version %q", m.Version)
	}
}

// An empty version is the API saying "nothing published" (and its kill switch
// for a bad release): a silent no-op, never an error.
func TestParseUpdaterNothingPublished(t *testing.T) {
	for _, body := range []string{`{"version": ""}`, `{}`, `{"version": "   "}`} {
		m, err := ParseUpdater([]byte(body))
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", body, err)
		}
		if m.Offered() {
			t.Fatalf("%s: expected no release to be offered", body)
		}
	}
}

// A release offered without the means to verify or fetch it is a server-side
// mistake, not something to silently skip.
func TestParseUpdaterRejectsIncompleteRelease(t *testing.T) {
	cases := map[string]string{
		"no download": `{"version":"1.5.0","sha256":"aaa"}`,
		"no checksum": `{"version":"1.5.0","download":"https://api.example/x"}`,
		"bad JSON":    `not json`,
	}
	for name, body := range cases {
		if _, err := ParseUpdater([]byte(body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// The API must be able to grow the document without breaking deployed updaters.
func TestParseUpdaterIgnoresUnknownFields(t *testing.T) {
	body := `{"version":"1.5.0","download":"https://api.example/x","sha256":"aaa","ring":"pilot"}`
	m, err := ParseUpdater([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Version != "1.5.0" {
		t.Fatalf("unexpected version %q", m.Version)
	}
}

// The exact payloads the API serves, pinned here so a change on either side
// breaks a test rather than a fleet. Source of truth:
// docs/superpowers/specs/2026-08-28-updater-manifest-api.md, implemented by
// emly-go-api's models.UpdaterManifest - every field but version is omitempty,
// so "nothing to distribute" is exactly {"version":""}.
func TestParseUpdaterAPIContract(t *testing.T) {
	empty, err := ParseUpdater([]byte(`{"version":""}`))
	if err != nil {
		t.Fatalf("the API's empty manifest was rejected: %v", err)
	}
	if empty.Offered() {
		t.Error(`{"version":""} must read as nothing to distribute`)
	}

	full := `{"version":"1.5.0",` +
		`"download":"https://api.emly.ffois.it/v2/updates/download/updater/1.5.0",` +
		`"sha256":"3f786850e387550fdab836ed7e6dc881de23001b1f8b9a3d6e5c4a2b0f9e8d7c",` +
		`"size":4812345,"publishedAt":"2026-08-28T09:00:00Z",` +
		`"releaseNotes":{"it":"Note","en":"Notes"}}`
	m, err := ParseUpdater([]byte(full))
	if err != nil {
		t.Fatalf("the API's manifest was rejected: %v", err)
	}
	if m.Version != "1.5.0" || m.Size != 4812345 || m.PublishedAt != "2026-08-28T09:00:00Z" {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	if m.Notes("it") != "Note" {
		t.Errorf("release notes not read: %+v", m.ReleaseNotes)
	}

	// A release published with only one language: the API omits the other key
	// rather than sending an empty string.
	oneLang, err := ParseUpdater([]byte(`{"version":"1.5.0","download":"https://x/y","sha256":"aa","releaseNotes":{"it":"Solo IT"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if oneLang.Notes("en") != "Solo IT" {
		t.Errorf("Notes(en) = %q, want the only note present", oneLang.Notes("en"))
	}
}

// The API accepts versions of 2 to 4 numeric segments plus an optional
// prerelease suffix (updaterVersionPattern in emly-go-api). Every shape it can
// publish has to compare correctly against the running version, or a machine
// would either skip a real release or reinstall one it already has.
func TestUpdaterVersionShapesCompare(t *testing.T) {
	cases := []struct {
		running, offered string
		wantNewer        bool
	}{
		{"1.4.2", "1.5.0", true},
		{"1.4.2", "1.4.2", false},
		{"1.4.2", "1.4.2.1", true},  // four segments
		{"1.4.2.1", "1.4.2", false}, // ...and back
		{"1.4.2", "1.5", true},      // two segments
		{"1.5.0-beta.1", "1.5.0", true},
		{"1.5.0", "1.5.0-beta.1", false}, // a prerelease never overtakes its release
	}
	for _, c := range cases {
		got, err := Less(c.running, c.offered)
		if err != nil {
			t.Errorf("Less(%s, %s): %v", c.running, c.offered, err)
			continue
		}
		if got != c.wantNewer {
			t.Errorf("Less(%s, %s) = %v, want %v", c.running, c.offered, got, c.wantNewer)
		}
	}
}

func TestUpdaterNotes(t *testing.T) {
	m := &UpdaterManifest{ReleaseNotes: map[string]string{"it": "Note", "en": "Notes"}}
	if got := m.Notes("it"); got != "Note" {
		t.Errorf("Notes(it) = %q", got)
	}
	// Unknown language falls back to English.
	if got := m.Notes("de"); got != "Notes" {
		t.Errorf("Notes(de) = %q", got)
	}
	// Only Italian available: still better than nothing in the log.
	only := &UpdaterManifest{ReleaseNotes: map[string]string{"it": "Note"}}
	if got := only.Notes("de"); got != "Note" {
		t.Errorf("Notes(de) with only it = %q", got)
	}
	if got := (&UpdaterManifest{}).Notes("it"); got != "" {
		t.Errorf("Notes with no release notes = %q", got)
	}
}
