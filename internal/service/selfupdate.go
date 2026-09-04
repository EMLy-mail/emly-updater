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
			tried, _ := u.Cfg.UpdaterManifestURL(cyc.eff.ManifestURL(cyc.chain[0]))
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
		if rec != nil {
			rec.GaveUp = true
			if err := u.Store.SetSelfUpdate(rec); err != nil {
				u.Log.Warn("failed to record the abandoned updater release", "error", err.Error())
			}
		}
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
	}
	return rec
}

// resolveUpdaterManifest fetches the updater's own release manifest, asking
// each source for its own updater endpoint so a machine on a site's internal
// mirror updates from that mirror.
// It returns the URL that answered alongside the manifest, so the log can name
// the exact endpoint this machine reached rather than the source it was
// derived from.
func (u *Updater) resolveUpdaterManifest(ctx context.Context, cyc *cycleState) (source.Source, *manifest.UpdaterManifest, string, error) {
	resolver := u.newResolver(cyc)
	resolver.Document = "updater manifest"

	return source.ResolveUpdater(ctx, resolver, func(s source.Source) (string, error) {
		http, ok := s.(*source.HTTPSource)
		if !ok {
			return "", fmt.Errorf("source %s has no manifest URL to derive from", s.Name())
		}
		return u.Cfg.UpdaterManifestURL(http.ManifestURL)
	})
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
		return false
	}

	if err := verifySelfSetup(setupPath); err != nil {
		// A checksum that matched a signature that does not means the manifest
		// and the file agree with each other but not with us: drop the file so
		// a re-download cannot be served from cache.
		u.Log.ErrorEvent(logging.EventSelfUpdateFailed, "refusing to run the updater setup",
			"target", m.Version, "path", setupPath, "error", err.Error())
		_ = os.Remove(setupPath)
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
		u.Log.ErrorEvent(logging.EventSelfUpdateFailed,
			"refusing to launch the updater setup: the attempt could not be recorded",
			"target", m.Version, "error", err.Error())
		return false
	}

	logPath := filepath.Join(config.LogsDir(), fmt.Sprintf("updater-selfinstall-%s.log", m.Version))
	if err := selfupdate.Launch(setupPath, logPath); err != nil {
		u.Log.ErrorEvent(logging.EventSelfUpdateFailed, "failed to launch the updater setup",
			"target", m.Version, "path", setupPath, "error", err.Error())
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
