package selfupdate

import (
	"strings"
	"testing"
	"time"

	"emlyupdater/internal/manifest"
	"emlyupdater/internal/state"
)

var now = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

func offered(version string) *manifest.UpdaterManifest {
	return &manifest.UpdaterManifest{
		Version:  version,
		Download: "https://api.example/updater/" + version,
		SHA256:   "abcd",
	}
}

func TestReconcile(t *testing.T) {
	cases := []struct {
		name    string
		running string
		rec     *state.SelfUpdate
		want    Outcome
	}{
		{"no record", "1.4.2", nil, OutcomeNone},
		{"empty record", "1.4.2", &state.SelfUpdate{}, OutcomeNone},
		{"landed", "1.5.0", &state.SelfUpdate{Version: "1.5.0"}, OutcomeLanded},
		{"still on the old binary", "1.4.2", &state.SelfUpdate{Version: "1.5.0"}, OutcomeMissed},
		// A newer build arriving by other means (GPO) is not a failure.
		{"overtaken by a later build", "1.6.0", &state.SelfUpdate{Version: "1.5.0"}, OutcomeLanded},
	}
	for _, c := range cases {
		got, err := Reconcile(c.running, c.rec)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: Reconcile = %v, want %v", c.name, got, c.want)
		}
	}

	if _, err := Reconcile("garbage", &state.SelfUpdate{Version: "1.5.0"}); err == nil {
		t.Error("expected an error for an unparseable running version")
	}
}

func TestDecideInstallsANewRelease(t *testing.T) {
	d, err := Decide("1.4.2", offered("1.5.0"), nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Install || d.Attempt != 1 {
		t.Fatalf("Decide = %+v, want a first attempt", d)
	}
}

func TestDecideSkipsWhenNothingToDo(t *testing.T) {
	cases := []struct {
		name     string
		running  string
		m        *manifest.UpdaterManifest
		wantWord string
	}{
		{"nothing published", "1.4.2", &manifest.UpdaterManifest{}, "no updater release"},
		{"same version", "1.5.0", offered("1.5.0"), "already running"},
		// The API rolls back by publishing a higher version, never by
		// pointing the manifest at an older one.
		{"older version offered", "1.5.0", offered("1.4.2"), "already running"},
	}
	for _, c := range cases {
		d, err := Decide(c.running, c.m, nil, now)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if d.Install {
			t.Errorf("%s: Decide wanted to install: %+v", c.name, d)
		}
		if !strings.Contains(d.Reason, c.wantWord) {
			t.Errorf("%s: reason = %q, want it to mention %q", c.name, d.Reason, c.wantWord)
		}
	}
}

// A setup launched moments ago is probably still running; a second one would
// race it.
func TestDecideHonoursCooldown(t *testing.T) {
	rec := &state.SelfUpdate{Version: "1.5.0", Attempts: 1, LaunchedAt: now.Add(-time.Minute)}

	d, err := Decide("1.4.2", offered("1.5.0"), rec, now)
	if err != nil {
		t.Fatal(err)
	}
	if d.Install {
		t.Fatalf("Decide relaunched inside the cooldown: %+v", d)
	}
	if !strings.Contains(d.Reason, "cooldown") {
		t.Errorf("reason = %q, want it to mention the cooldown", d.Reason)
	}

	// Past the cooldown, the same record is retried and the attempt counted.
	rec.LaunchedAt = now.Add(-Cooldown - time.Second)
	d, err = Decide("1.4.2", offered("1.5.0"), rec, now)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Install || d.Attempt != 2 {
		t.Fatalf("Decide = %+v, want a second attempt", d)
	}
}

// A release that keeps not landing must not have every poll cycle stop and
// restart the service forever.
func TestDecideGivesUpAfterMaxAttempts(t *testing.T) {
	rec := &state.SelfUpdate{
		Version:    "1.5.0",
		Attempts:   MaxAttempts,
		LaunchedAt: now.Add(-24 * time.Hour),
	}

	d, err := Decide("1.4.2", offered("1.5.0"), rec, now)
	if err != nil {
		t.Fatal(err)
	}
	if d.Install {
		t.Fatalf("Decide kept retrying past %d attempts: %+v", MaxAttempts, d)
	}
	if !d.GiveUp {
		t.Error("giving up was not reported on the cycle it happens")
	}

	// Once remembered, it is not reported again on every later cycle.
	rec.GaveUp = true
	d, err = Decide("1.4.2", offered("1.5.0"), rec, now)
	if err != nil {
		t.Fatal(err)
	}
	if d.Install || d.GiveUp {
		t.Fatalf("Decide = %+v, want a quiet skip once the target is abandoned", d)
	}
}

// Publishing a different release is how an operator recovers a fleet stuck on
// an abandoned target: it must start from a clean count.
func TestDecideNewTargetResetsAttempts(t *testing.T) {
	rec := &state.SelfUpdate{
		Version:    "1.5.0",
		Attempts:   MaxAttempts,
		GaveUp:     true,
		LaunchedAt: now.Add(-24 * time.Hour),
	}

	d, err := Decide("1.4.2", offered("1.5.1"), rec, now)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Install || d.Attempt != 1 {
		t.Fatalf("Decide = %+v, want a fresh first attempt on the new target", d)
	}
}

// The cooldown belongs to the target that was launched, not to the machine: a
// newly published release must not be held back by it.
func TestDecideCooldownDoesNotBlockANewTarget(t *testing.T) {
	rec := &state.SelfUpdate{Version: "1.5.0", Attempts: 1, LaunchedAt: now.Add(-time.Second)}

	d, err := Decide("1.4.2", offered("1.5.1"), rec, now)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Install {
		t.Fatalf("Decide = %+v, want the new target to install", d)
	}
}

func TestDecideRejectsUnparseableVersions(t *testing.T) {
	if _, err := Decide("1.4.2", offered("not-a-version"), nil, now); err == nil {
		t.Error("expected an error for an unparseable manifest version")
	}
	if _, err := Decide("garbage", offered("1.5.0"), nil, now); err == nil {
		t.Error("expected an error for an unparseable running version")
	}
}
