// Package selfupdate holds the rules and the mechanics of the updater
// updating itself: when a release should be applied, and how a setup that will
// stop this very service gets launched.
//
// The orchestration around it - fetching the manifest, downloading, verifying,
// logging - lives in internal/service alongside the rest of the update cycle,
// the same way sourcepolicy.go does. What is here is the part worth testing on
// its own: everything in this file except Launch is pure.
package selfupdate

import (
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"emlyupdater/internal/manifest"
	"emlyupdater/internal/state"
)

const (
	// MaxAttempts bounds how many times one target version is launched before
	// it is abandoned. Without it, a release that installs cleanly but does
	// not leave the new binary running - a bad build, a setup that fails under
	// SYSTEM, a machine where the service cannot restart - would have every
	// poll cycle stop and restart the service, fleet-wide, forever.
	MaxAttempts = 3

	// Cooldown is how long a launched setup is given to finish before another
	// launch is considered. Reaching a cycle with the record still in place
	// and this service still running normally means the setup failed before it
	// could stop us; give it room rather than racing it.
	Cooldown = 10 * time.Minute
)

// Outcome is what the record left by a previous launch says happened.
type Outcome int

const (
	// OutcomeNone: no self-update was ever launched, nothing to reconcile.
	OutcomeNone Outcome = iota
	// OutcomeLanded: the running binary is the version that was launched (or
	// newer). The record has served its purpose.
	OutcomeLanded
	// OutcomeMissed: the launch did not produce the version it promised. The
	// record stays so the attempt is counted.
	OutcomeMissed
)

// Reconcile compares the version now running against the record a previous
// launch left behind.
//
// This is the only way to learn how a self-update went: the setup stops this
// service to replace its executable, so the process that launched it never
// sees the result. Whatever is running at the next start is the answer.
//
// "Newer than promised" counts as landed too - somebody may have pushed a
// later installer by GPO in the meantime, and treating that as a failure would
// have the machine try to install a version it has already moved past.
func Reconcile(running string, rec *state.SelfUpdate) (Outcome, error) {
	if rec == nil || rec.Version == "" {
		return OutcomeNone, nil
	}
	behind, err := manifest.Less(running, rec.Version)
	if err != nil {
		return OutcomeNone, err
	}
	if behind {
		return OutcomeMissed, nil
	}
	return OutcomeLanded, nil
}

// Decision is what a cycle should do about the updater's own version.
type Decision struct {
	// Install is true when the target should be fetched and launched.
	Install bool
	// Attempt is the attempt number to record when launching (1 for a target
	// never tried before).
	Attempt int
	// GiveUp is true on the cycle where a target is abandoned for having
	// failed too many times - reported once, then remembered on the record.
	GiveUp bool
	// Reason explains the decision in the log, for both branches.
	Reason string
}

// Decide applies the self-update rules to a fetched manifest.
//
// running is this binary's own version, rec the record of a launch already in
// flight or failed (nil when there is none, and nil is what the caller passes
// once Reconcile has reported OutcomeLanded).
func Decide(running string, m *manifest.UpdaterManifest, rec *state.SelfUpdate, now time.Time) (Decision, error) {
	if !m.Offered() {
		return Decision{Reason: "no updater release is published"}, nil
	}

	newer, err := manifest.Less(running, m.Version)
	if err != nil {
		return Decision{}, err
	}
	if !newer {
		// Never downgrade: rolling back is done by publishing a higher
		// version, not by pointing the manifest at an older one.
		return Decision{Reason: fmt.Sprintf("already running %s, offered %s", running, m.Version)}, nil
	}

	// A record for a different target is stale the moment the manifest moves
	// on: the new release may be the fix for whatever kept the old one from
	// landing, so it starts with a clean count.
	if rec == nil || rec.Version != m.Version {
		return Decision{Install: true, Attempt: 1, Reason: "new updater release available"}, nil
	}

	if rec.GaveUp {
		return Decision{Reason: fmt.Sprintf("%s was abandoned after %d failed attempts, waiting for a different release",
			rec.Version, rec.Attempts)}, nil
	}
	if rec.Attempts >= MaxAttempts {
		return Decision{
			GiveUp: true,
			Reason: fmt.Sprintf("%s did not install in %d attempts, giving up until a different release is published",
				rec.Version, rec.Attempts),
		}, nil
	}
	if elapsed := now.Sub(rec.LaunchedAt); elapsed < Cooldown {
		return Decision{Reason: fmt.Sprintf("a setup for %s was launched %s ago, still within the %s cooldown",
			rec.Version, elapsed.Round(time.Second), Cooldown)}, nil
	}

	return Decision{
		Install: true,
		Attempt: rec.Attempts + 1,
		Reason:  fmt.Sprintf("retrying %s, attempt %d of %d", m.Version, rec.Attempts+1, MaxAttempts),
	}, nil
}

// Launch starts the updater's own setup and returns without waiting for it.
//
// Not waiting is a correctness requirement, not an optimisation. The setup's
// first act is to stop this service and wait for the SCM to report it stopped;
// the service's stop handler cancels the update loop and waits for it to
// return - which, if this call blocked, would be waiting for the setup. The
// two would deadlock until the setup's 60-second stop timeout expired and the
// install failed.
//
// The setup outlives this process: a Windows service is not run inside a job
// object that kills its children, and the shutdown arrives through the SCM
// rather than as a kill of the process tree.
func Launch(setupPath, logPath string) error {
	cmd := exec.Command(setupPath,
		"/VERYSILENT",
		"/SUPPRESSMSGBOXES",
		"/NORESTART",
		"/LOG="+logPath,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start %s: %w", setupPath, err)
	}
	// Release rather than Wait: nothing here will ever observe the exit code,
	// and holding the handle would keep a zombie around for the few seconds
	// this process has left.
	return cmd.Process.Release()
}
