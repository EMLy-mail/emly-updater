package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"emlyupdater/internal/authenticode"
	"emlyupdater/internal/cert"
	"emlyupdater/internal/config"
	"emlyupdater/internal/logging"
	"emlyupdater/internal/manifest"
	"emlyupdater/internal/selfupdate"
	"emlyupdater/internal/source"
	"emlyupdater/internal/state"
	"emlyupdater/internal/version"
	"emlyupdater/internal/wsclient"
)

// selfUpdate brings the updater itself up to date, and reports whether a setup
// has been launched - in which case this service is about to be stopped and
// replaced, and the caller must return immediately rather than start any work
// it cannot finish.
//
// Nothing here ever fails a cycle. Keeping EMLy updated is this service's job;
// updating itself is how it stays good at it, and an unreachable updater
// manifest, a refused signature or an abandoned release must all leave the
// EMLy path running exactly as before.
//
// The handoff is deliberately one-way: the setup's first act is to stop this
// service, so the process that launches it never learns how it went. What is
// written to state.json beforehand is the only thing the next start has to go
// on.
func (u *Updater) selfUpdate(ctx context.Context, cyc *cycleState) bool {
	// Every cycle that does not act says why, at Info, under this one message:
	// grepping the log for it answers "is it looking for updates, and what did
	// it decide?" without stopping the service to re-run it in the foreground.
	const skipped = "no updater self-update this cycle"
	running := version.Version

	if !cyc.eff.Doc.Updater.SelfUpdate.Enabled {
		u.Log.Info(skipped, "installed", running, "reason", "updater.selfUpdate.enabled is false")
		return false
	}

	rec := u.reconcileSelfUpdate()

	src, m, manifestURL, err := u.resolveUpdaterManifest(ctx, cyc)
	if err != nil {
		if errors.Is(err, source.ErrNotFound) {
			// No source implements the endpoint. Expected on a site whose
			// internal mirror has not been updated yet, and on any deployment
			// that has not published an updater manifest at all. Name the
			// address that was tried: on a 404 that is the one thing worth
			// checking, and no source succeeded so none reported one.
			tried, _ := u.Cfg.UpdaterManifestURL(cyc.eff.ManifestURL(u.preferredChain(cyc)[0]))
			u.Log.Info(skipped, "installed", running, "manifestURL", tried,
				"reason", "no update source serves an updater manifest")
		} else {
			u.Log.Warn("could not fetch the updater manifest, self-update skipped this cycle",
				"error", err.Error())
		}
		return false
	}

	decision, err := selfupdate.Decide(running, m, rec, time.Now())
	if err != nil {
		u.Log.Warn("could not evaluate the updater manifest, self-update skipped this cycle",
			"error", err.Error())
		return false
	}
	if decision.GiveUp {
		u.Log.ErrorEvent(logging.EventSelfUpdateFailed, "giving up on an updater release",
			"target", m.Version, "installed", running, "manifestURL", manifestURL,
			"reason", decision.Reason)
		// Decide only gives up on a target it has a record for, but read that
		// from the record rather than assuming it: a nil here would take the
		// whole service down over a self-update that had already failed.
		attempts := decision.Attempt
		if rec != nil {
			rec.GaveUp = true
			attempts = rec.Attempts
			if err := u.Store.SetSelfUpdate(rec); err != nil {
				u.Log.Warn("failed to record the abandoned updater release", "error", err.Error())
			}
		}
		u.emitUpdateFailed(updateEvent{Target: "updater", FromVersion: running, ToVersion: m.Version,
			Attempt: attempts, WillRetry: false, // WillRetry = !GiveUp (spec §8.5); this branch is always GiveUp.
			Error: &wsclient.ErrorBody{Code: "gave_up", Message: decision.Reason}})
		// reconcileSelfUpdate's OutcomeMissed keeps firing for this same
		// attempt on every later cycle (Reconcile does not consult
		// rec.GaveUp - running stays behind rec.Version forever). The give-up
		// above already told the client channel everything that Missed would;
		// pre-mark it so it stays silent instead of repeating the same
		// attempt under a different code.
		u.markUpdateFailed("updater", m.Version, attempts, "version_mismatch")
		return false
	}
	if !decision.Install {
		u.Log.Info(skipped, "installed", running, "manifestURL", manifestURL,
			"reason", decision.Reason)
		return false
	}

	u.Log.InfoEvent(logging.EventSelfUpdateFound, "updater update available",
		"installed", running, "target", m.Version, "attempt", decision.Attempt,
		"manifestURL", manifestURL, "download", m.Download,
		"notes", m.Notes(u.Cfg.ResolveEMLyWithChannel(cyc.eff.Doc.Updater.Channel()).Language))

	u.announceUpdate(wsclient.ManifestCheck{Target: "updater", InstalledVersion: running, AvailableVersion: m.Version,
		UpdateAvailable: true, Decision: "install_next_cycle", CheckedAt: u.clock().UTC().Format(time.RFC3339)})

	return u.applySelfUpdate(ctx, src, m, decision.Attempt)
}

// reconcileSelfUpdate settles the record left by a previous launch and returns
// what Decide should still take into account: nil once the new binary is
// confirmed running, the record itself while the target has not landed.
func (u *Updater) reconcileSelfUpdate() *state.SelfUpdate {
	st, err := u.Store.Load()
	if err != nil {
		u.Log.Warn("state file unreadable, treating the self-update history as empty", "error", err.Error())
		return nil
	}
	rec := st.SelfUpdate
	if rec == nil {
		return nil
	}

	running := version.Version
	outcome, err := selfupdate.Reconcile(running, rec)
	if err != nil {
		u.Log.Warn("self-update record carries an invalid version, discarding",
			"version", rec.Version, "error", err.Error())
		_ = u.Store.ClearSelfUpdate()
		return nil
	}

	switch outcome {
	case selfupdate.OutcomeLanded:
		u.Log.InfoEvent(logging.EventSelfUpdateApplied, "updater self-update completed",
			"from", rec.FromVersion, "to", running, "target", rec.Version, "attempts", rec.Attempts)
		u.emit(wsclient.EvtUpdateApplied, updateEvent{Target: "updater", FromVersion: rec.FromVersion, ToVersion: running,
			Attempt: rec.Attempts})
		landed := *rec
		u.selfLanded.Store(&landed)
		if err := u.Store.ClearSelfUpdate(); err != nil {
			u.Log.Warn("failed to clear the self-update record", "error", err.Error())
		}
		// Debug, not Warn: this first cycle runs while the setup that started
		// this service is usually still finishing, and Windows locks a running
		// executable - so the expected outcome here is a failure to delete,
		// and the next cycle picks it up.
		if err := u.SelfDownloads.CleanupExcept(""); err != nil {
			u.Log.Debug("updater installer not removed yet, the setup may still be running",
				"error", err.Error())
		}
		return nil
	case selfupdate.OutcomeMissed:
		u.Log.Warn("a previously launched updater setup did not take effect",
			"target", rec.Version, "stillRunning", running, "attempts", rec.Attempts)
		u.emitUpdateFailed(updateEvent{Target: "updater", FromVersion: running, ToVersion: rec.Version,
			Attempt: rec.Attempts, WillRetry: rec.Attempts < selfupdate.MaxAttempts,
			Error: &wsclient.ErrorBody{Code: "version_mismatch", Message: "the launched setup did not leave the new version running"}})
	}
	return rec
}

// resolveUpdaterManifest fetches the updater's own release manifest, asking
// each source for its own updater endpoint so a machine on a site's internal
// mirror updates from that mirror.
// It returns the URL that answered alongside the manifest, so the log can name
// the exact endpoint this machine reached rather than the source it was
// derived from. A successful resolution updates the preferred server for the
// rest of this session (see resolveUpdaterManifestWith).
func (u *Updater) resolveUpdaterManifest(ctx context.Context, cyc *cycleState) (source.Source, *manifest.UpdaterManifest, string, error) {
	return u.resolveUpdaterManifestWith(ctx, cyc, true)
}

// resolveUpdaterManifestWith is resolveUpdaterManifest with control over
// whether a successful resolution is allowed to change the preferred server
// for the rest of this session. notePreferred=false is for read-only
// diagnostics - the client-channel updater.manifest.check dry run
// (clientcmd.go) - which must observe the same chain evaluation as a real
// self-update check without the side effect of pinning the machine to a
// backup server or waking the presence supervisor (wakeClientWS) as a
// consequence of a status check.
func (u *Updater) resolveUpdaterManifestWith(ctx context.Context, cyc *cycleState, notePreferred bool) (source.Source, *manifest.UpdaterManifest, string, error) {
	resolver := u.newResolver(cyc)
	resolver.Document = "updater manifest"

	src, m, servedBy, err := source.ResolveUpdater(ctx, resolver, func(s source.Source) (string, error) {
		http, ok := s.(*source.HTTPSource)
		if !ok {
			return "", fmt.Errorf("source %s has no manifest URL to derive from", s.Name())
		}
		return u.Cfg.UpdaterManifestURL(http.ManifestURL)
	})
	if err == nil && notePreferred {
		u.notePreferredServer(cyc, resolver, src)
	}
	return src, m, servedBy, err
}

// applySelfUpdate downloads the release, verifies it, records the attempt and
// hands the setup off. It reports whether the setup was launched.
//
// Every failure short of the launch itself is recoverable by the next cycle,
// so none of them is fatal - but none of them may lead to running the setup
// either: the file is executed as LocalSystem and replaces this very binary.
func (u *Updater) applySelfUpdate(ctx context.Context, src source.Source, m *manifest.UpdaterManifest, attempt int) bool {
	setupPath, err := u.SelfDownloads.Ensure(ctx, src, m.Target())
	if err != nil {
		u.Log.Warn("failed to download the updater setup, retrying next cycle",
			"target", m.Version, "error", err.Error())
		// download.Manager.Ensure wraps both a fetch failure and a checksum
		// mismatch in plain fmt.Errorf, with no sentinel to tell them apart
		// (unlike installFailureCode's installer errors) - download_failed
		// covers both.
		u.emitUpdateFailed(updateEvent{Target: "updater", FromVersion: version.Version, ToVersion: m.Version,
			Attempt: attempt, WillRetry: true, Error: &wsclient.ErrorBody{Code: "download_failed", Message: err.Error()}})
		return false
	}

	verify := verifySelfSetup
	if u.verifySelfSetupFn != nil {
		verify = u.verifySelfSetupFn
	}
	if err := verify(setupPath); err != nil {
		// A checksum that matched a signature that does not means the manifest
		// and the file agree with each other but not with us: drop the file so
		// a re-download cannot be served from cache.
		u.Log.ErrorEvent(logging.EventSelfUpdateFailed, "refusing to run the updater setup",
			"target", m.Version, "path", setupPath, "error", err.Error())
		_ = os.Remove(setupPath)
		u.emitUpdateFailed(updateEvent{Target: "updater", FromVersion: version.Version, ToVersion: m.Version,
			Attempt: attempt, WillRetry: true, Error: &wsclient.ErrorBody{Code: "signature_invalid", Message: err.Error()}})
		return false
	}

	// Claimed before anything below is done, and nothing below is undone if
	// it refuses: beginInstall also refuses (false) when a destructive
	// client command has been committed in the meantime
	// (service.restart/machine.reboot) - see clientpower.go. Checking this
	// first, ahead of SetSelfUpdate and retireCache, means a refusal here
	// leaves neither the self-update attempt record nor the remote-config
	// cache touched - there would be nothing to undo them with, since
	// nothing has failed and rolling either back "because a reboot is
	// coming" would be its own bug. installing is not decremented on
	// success: the service is about to be stopped by the setup this
	// launches, so there is nothing left to un-mark - a destructive command
	// arriving between now and the actual restart is correctly refused as
	// busy by admitDestructive until this process exits. A failed launch,
	// below, is the only path that gets to undo it.
	if !u.beginInstall("updater self-update") {
		return false
	}

	// Persist before launching. The setup stops this service, so this record
	// is the only thing that will tell the next start what was attempted -
	// and without it the attempt could not be counted, which is what keeps a
	// bad release from restarting the service on every poll forever. If it
	// cannot be written, do not launch.
	rec := &state.SelfUpdate{
		Version:     m.Version,
		FromVersion: version.Version,
		SetupPath:   setupPath,
		SHA256:      m.SHA256,
		Attempts:    attempt,
		LaunchedAt:  time.Now().UTC(),
	}
	if err := u.Store.SetSelfUpdate(rec); err != nil {
		u.endInstall()
		u.Log.ErrorEvent(logging.EventSelfUpdateFailed,
			"refusing to launch the updater setup: the attempt could not be recorded",
			"target", m.Version, "error", err.Error())
		return false
	}

	// The new build must take its configuration from the API, not from the
	// document this one cached (see retireCache). Best-effort: a cache that
	// cannot be moved is no reason to hold back the update.
	if err := u.retireCache(); err != nil {
		u.Log.Warn("could not move the remote configuration cache aside before the self-update, the new build will start from it",
			"path", u.cachePath(), "error", err.Error())
	} else {
		u.Log.Info("remote configuration cache moved aside before the self-update",
			"path", u.cachePrevPath())
	}

	// Emitted right before the launch, not after: the service is about to
	// stop, so this may be the last message the old build ever manages to
	// send (spec §8.3 - "the last message the old build gets to send"), and
	// it is fine for it to still be sitting in the buffer when that happens.
	// Launch itself is never blocked on it.
	u.emit(wsclient.EvtUpdateStarted, updateEvent{Target: "updater", FromVersion: version.Version, ToVersion: m.Version,
		Attempt: attempt, Trigger: u.cycleTrigger})

	launch := selfupdate.Launch
	if u.launchFn != nil {
		launch = u.launchFn
	}
	logPath := filepath.Join(config.LogsDir(), fmt.Sprintf("updater-selfinstall-%s.log", m.Version))
	if err := launch(setupPath, logPath); err != nil {
		u.endInstall()
		u.Log.ErrorEvent(logging.EventSelfUpdateFailed, "failed to launch the updater setup",
			"target", m.Version, "path", setupPath, "error", err.Error())
		if err := u.restoreCache(); err != nil {
			u.Log.Warn("could not restore the remote configuration cache after the failed launch",
				"path", u.cachePath(), "error", err.Error())
		}
		// update.started was already emitted above: without this, a launch
		// failure would leave it dangling until the next cycle's
		// reconcileSelfUpdate (OutcomeMissed) eventually reports it, several
		// cooldown minutes later.
		u.emitUpdateFailed(updateEvent{Target: "updater", FromVersion: version.Version, ToVersion: m.Version,
			Attempt: attempt, WillRetry: true, Error: &wsclient.ErrorBody{Code: "launch_failed", Message: err.Error()}})
		// The record persisted above (SetSelfUpdate) is not cleared on a
		// failed launch, so the next cycle's reconcileSelfUpdate sees the
		// same attempt and reports OutcomeMissed - the same failure, under
		// "version_mismatch" instead of "launch_failed". Pre-mark it so
		// that repeat stays silent (spec §8.5, "each distinct failure once
		// per process") instead of restating this same attempt a second
		// time under a different code.
		u.markUpdateFailed("updater", m.Version, attempt, "version_mismatch")
		return false
	}

	u.Log.Info("updater setup launched, this service will now be stopped and replaced",
		"target", m.Version, "attempt", attempt, "log", logPath)
	return true
}

// verifySelfSetup refuses any file that is not signed by the certificate this
// build embeds.
//
// The SHA256 from the manifest is already enforced by the download manager,
// but it only proves the file matches what the manifest said - and the
// internal source is plain HTTP, so whoever can serve a tampered setup can
// serve a matching checksum with it. The signature is the part an attacker
// cannot produce.
func verifySelfSetup(path string) error {
	_, der, err := cert.Embedded()
	if err != nil {
		return fmt.Errorf("cannot verify the setup: %w", err)
	}
	return authenticode.Verify(path, authenticode.Thumbprint(der))
}
