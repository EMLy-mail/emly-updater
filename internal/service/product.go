package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"emlyupdater/internal/assoc"
	"emlyupdater/internal/config"
	"emlyupdater/internal/download"
	"emlyupdater/internal/installer"
	"emlyupdater/internal/logging"
	"emlyupdater/internal/manifest"
	"emlyupdater/internal/notify"
	"emlyupdater/internal/process"
	"emlyupdater/internal/product"
	"emlyupdater/internal/source"
	"emlyupdater/internal/state"
	"emlyupdater/internal/wsclient"
)

// productState is what the engine needs to know about a product on this
// machine, resolved once per product per cycle.
type productState struct {
	Installed string // version on disk; config.FreshInstallVersion when absent
	Fresh     bool   // absent: Installed is the comparison sentinel, not a release
	Channel   string // "stable" | "beta"
	Language  string // notification language; EMLy's LANGUAGE, "it" otherwise
}

// from is the version to report as "installed before this update": empty on
// a fresh install (spec §8.3) - the 0.0.0 sentinel is an internal comparison
// value, not a release.
func (ps productState) from() string {
	if ps.Fresh {
		return ""
	}
	return ps.Installed
}

func emlyState(e config.EMLyInfo) productState {
	return productState{Installed: e.InstalledVersion, Fresh: e.FreshInstall, Channel: e.Channel, Language: e.Language}
}

// emlyProduct is the built-in EMLy definition. cyc may be nil (tests calling
// install directly); the channel override then stays empty.
func (u *Updater) emlyProduct(cyc *cycleState) *product.Product {
	channel := ""
	if cyc != nil && cyc.eff != nil {
		channel = cyc.eff.Doc.Updater.Channel()
	}
	return &product.Product{
		Slug:       product.EMLySlug,
		Name:       "EMLy",
		InstallDir: u.Cfg.EMLyInstallDir,
		ExeName:    u.Cfg.EMLyExeName,
		Channel:    channel,
		Detect: []product.VersionSource{{Type: product.SourceINI, Path: u.Cfg.EMLyConfigFile,
			Section: "EMLy", Key: "GUI_SEMVER"}},
		Installer:         product.InstallerSpec{Type: product.InstallerInno, CleanReinstall: true},
		InstallWhenAbsent: true,
		Legacy:            true,
	}
}

// resolveProductState reads what is installed. EMLy goes through
// ResolveEMLyWithChannel exactly as before (an unreadable config.ini is its
// fresh-install mode, and p.Channel carries the document's override). Any
// other product goes through its detection chain; ok=false means Unknown and
// the product must be left alone this cycle.
func (u *Updater) resolveProductState(p *product.Product) (productState, bool) {
	if p.Legacy {
		return emlyState(u.Cfg.ResolveEMLyWithChannel(p.Channel)), true
	}
	channel := p.Channel
	if channel != "beta" {
		channel = "stable"
	}
	ps := productState{Channel: channel, Language: "it"}
	r := product.Detect(p)
	switch r.Outcome {
	case product.Installed:
		ps.Installed = r.Version
	case product.Absent:
		ps.Installed, ps.Fresh = config.FreshInstallVersion, true
	default:
		if u.detectWarned == nil {
			u.detectWarned = map[string]bool{}
		}
		if !u.detectWarned[p.Slug] {
			u.detectWarned[p.Slug] = true
			u.Log.Warn("installed version unreadable, product left alone", "product", p.Slug, "detect", r.String())
		}
		return ps, false
	}
	if u.detectWarned[p.Slug] {
		delete(u.detectWarned, p.Slug)
	}
	return ps, true
}

// downloadsFor is the download cache of p: EMLy keeps u.Downloads (prefix
// "EMLy-"), every other product gets its own prefix in the same directory and
// the same Pacer - the server's download slots are one pool.
func (u *Updater) downloadsFor(p *product.Product) *download.Manager {
	if p.Legacy {
		return u.Downloads
	}
	if m, ok := u.productDownloads[p.Slug]; ok {
		return m
	}
	if u.productDownloads == nil {
		u.productDownloads = map[string]*download.Manager{}
	}
	m := &download.Manager{Dir: u.Downloads.Dir, Prefix: p.Slug + "-", Pacer: u.Downloads.Pacer, Log: u.Downloads.Log}
	u.productDownloads[p.Slug] = m
	return m
}

// driverFor returns the installer driver of p: its installer technology,
// with EMLy's /FORCEUPGRADE for EMLy only.
func (u *Updater) driverFor(p *product.Product) installer.Driver {
	if u.driverFn != nil {
		return u.driverFn(p)
	}
	return installer.For(installer.Spec{Slug: p.Slug, Type: p.Installer.Type, InstallDir: p.InstallDir,
		LogsDir: config.LogsDir(), ForceUpgrade: p.Legacy})
}

func (u *Updater) isRunning(exe string) bool {
	if u.runningFn != nil {
		return u.runningFn(exe)
	}
	return process.IsRunning(exe)
}

func (u *Updater) notifyBox(msg notify.Message, seconds int) bool {
	if u.notifyBoxFn != nil {
		return u.notifyBoxFn(msg, seconds)
	}
	return notify.SendNotifyBox(msg, seconds)
}

// resolveProductTarget is resolveTarget for any product. EMLy keeps the
// historical path (resolveTarget, preferred-server bookkeeping included).
// Other products are a read of the same server chain on their own route and
// never move the preferred server.
func (u *Updater) resolveProductTarget(ctx context.Context, cyc *cycleState, p *product.Product, channel string) (source.Source, *manifest.Manifest, manifest.Target, error) {
	if p.Legacy {
		return u.resolveTarget(ctx, cyc, channel)
	}
	resolver := u.newProductResolver(cyc, p)
	src, m, err := resolver.Resolve(ctx)
	if err != nil {
		return nil, nil, manifest.Target{}, err
	}
	target, err := src.ResolveTarget(m, channel)
	if err != nil {
		return nil, nil, manifest.Target{}, err
	}
	return src, m, target, nil
}

// newProductResolver is newResolver on p's manifest route.
func (u *Updater) newProductResolver(cyc *cycleState, p *product.Product) *source.Resolver {
	var urls []string
	for _, name := range u.preferredChain(cyc) {
		if base := cyc.eff.BaseURL(name); base != "" {
			urls = append(urls, base+p.ManifestPath())
		}
	}
	if len(urls) == 0 {
		urls = append(urls, "")
	}
	settings := cyc.eff.Doc.Updater.Resolver
	resolver := &source.Resolver{
		Primary:     u.newHTTPSource(urls[0]),
		Attempts:    settings.Attempts,
		BaseBackoff: settings.BaseBackoff(),
		Document:    p.Slug + " manifest",
		Logf: func(format string, args ...any) {
			u.Log.Info(fmt.Sprintf(format, args...))
		},
	}
	for _, url := range urls[1:] {
		resolver.Fallbacks = append(resolver.Fallbacks, u.newHTTPSource(url))
	}
	return resolver
}

// productCycle is one product's share of a cycle: resume a pending install,
// or poll, download and install. It is what Cycle did for EMLy alone.
func (u *Updater) productCycle(ctx context.Context, cyc *cycleState, p *product.Product) error {
	ps, ok := u.resolveProductState(p)
	if !ok {
		return nil
	}
	if p.Legacy && ps.Fresh {
		u.Log.Info("EMLy config.ini not found - fresh-install mode",
			"assumedVersion", ps.Installed, "channel", ps.Channel)
	}
	if ps.Fresh && !p.InstallWhenAbsent {
		return nil
	}
	dl := u.downloadsFor(p)

	// 1) A persisted pending update takes priority over polling: it may have
	// been queued right before a reboot and must not be lost or re-fetched.
	pend, err := u.Store.PendingFor(p.Slug)
	if err != nil {
		u.Log.Warn("state file unreadable, starting fresh", "error", err.Error())
		pend = nil
	}
	if pend != nil && !pend.GaveUp {
		stillNeeded, err := manifest.Less(ps.Installed, pend.Version)
		if err != nil {
			u.Log.Warn("pending update has invalid version, discarding", "product", p.Slug, "version", pend.Version, "error", err.Error())
			_ = u.Store.ClearPendingFor(p.Slug)
		} else if !stillNeeded {
			// Installed by other means (or the pending entry is stale).
			u.Log.Info("pending update already satisfied, clearing", "product", p.Slug, "version", pend.Version)
			_ = u.Store.ClearPendingFor(p.Slug)
			_ = dl.CleanupExcept("")
		} else if err := download.VerifyFile(pend.SetupPath, pend.SHA256); err != nil {
			u.Log.Warn("pending setup failed re-verification, discarding for re-download", "product", p.Slug, "error", err.Error())
			_ = os.Remove(pend.SetupPath)
			_ = u.Store.ClearPendingFor(p.Slug)
		} else {
			u.Log.Info("resuming pending update", "product", p.Slug, "version", pend.Version, "forced", pend.Forced)
			u.cycleTrigger = "resume"
			u.progress = u.newProductProgressUI(p, pend.Version)
			defer u.endProgress()
			return u.applyProduct(ctx, cyc, p, pend, ps)
		}
	}

	// 2) Normal poll: manifest via this machine's server chain.
	src, m, target, err := u.resolveProductTarget(ctx, cyc, p, ps.Channel)
	if err != nil {
		if p.Legacy {
			u.notifySourcesUnreachable()
		}
		return err
	}
	if p.Legacy {
		u.sourcesUnreachableNotified = false
	}

	needUpdate, err := manifest.Less(ps.Installed, target.Version)
	if err != nil {
		return err
	}
	if !needUpdate {
		u.Log.Debug("already on latest version", "product", p.Slug, "installed", ps.Installed,
			"target", target.Version, "channel", ps.Channel)
		// Nothing pending, nothing needed: superseded setups can go.
		_ = dl.CleanupExcept("")
		return nil
	}

	forced, err := m.Forced(ps.Installed)
	if err != nil {
		return err
	}

	u.Log.InfoEvent(logging.EventUpdateFound, "update available", "product", p.Slug,
		"installed", ps.Installed, "target", target.Version,
		"channel", ps.Channel, "forced", forced, "source", src.Name())

	if p.Legacy {
		enabled, _ := cyc.eff.UpdaterEnabled(cyc.host.Now)
		mc := wsclient.ManifestCheck{Target: "emly", Channel: ps.Channel, AvailableVersion: target.Version,
			UpdateAvailable: true, Critical: forced, MinRequiredVersion: m.MinRequiredVersion,
			Decision: emlyDecision(true, forced, u.emlyRunning(), !enabled),
			Source:   u.serverRef(cyc, sourceURL(src)), CheckedAt: u.clock().UTC().Format(time.RFC3339)}
		if !ps.Fresh {
			mc.InstalledVersion = ps.Installed
		}
		u.announceUpdate(mc)
	}

	u.progress = u.newProductProgressUI(p, target.Version)
	defer u.endProgress()
	setupPath, err := dl.Ensure(u.progress.watch(ctx), src, target)
	if err != nil {
		// A full download queue is the server pacing the fleet, not a
		// failure: Ensure has already waited out what it could and logged
		// each refusal, and the next cycle tries again.
		if download.IsQueueFull(err) {
			u.Log.Info(fmt.Sprintf("server download queue full, %s setup download retried next cycle", p.Name),
				"target", target.Version)
			return nil
		}
		return fmt.Errorf("download/verification failed: %w", err)
	}

	pend = &state.Pending{
		Version:      target.Version,
		SetupPath:    setupPath,
		SHA256:       target.SHA256,
		Forced:       forced,
		DownloadedAt: time.Now().UTC(),
	}
	// Persist before applying so a crash/reboot at any later point resumes
	// from the verified local file instead of re-downloading.
	if err := u.Store.SetPendingFor(p.Slug, pend); err != nil {
		u.Log.Warn("failed to persist pending update, continuing", "product", p.Slug, "error", err.Error())
	}

	return u.applyProduct(ctx, cyc, p, pend, ps)
}

// applyProduct installs a verified pending update according to the app's
// running state: not running → install now; running and forced → optional
// WTS warning, then kill; running and non-forced → EMLy waits for exit, any
// other product is deferred to a later cycle.
func (u *Updater) applyProduct(ctx context.Context, cyc *cycleState, p *product.Product, pend *state.Pending, ps productState) error {
	// Coarse check, same reasoning as Cycle's own: apply is reached after
	// resolveTarget/download, which can take a while, so a destructive
	// command could have been committed since Cycle's own top-level check.
	// install() (below, via beginInstall) is still the authoritative,
	// race-safe checkpoint for the non-forced path - but the forced path
	// kills the app before ever reaching install(), so it gets its own check
	// too, right before the kill (see below): a user closing the app because
	// of an unrelated reboot warning must not have the forced kill run
	// anyway for an install that is about to be refused.
	if u.destructivePendingNow() {
		u.logDestructiveSkipOnce()
		return nil
	}

	exe := p.ExeName

	if u.isRunning(exe) {
		// The download is done and what follows - a countdown, or waiting
		// for the user to close the app, possibly for hours - is no time to
		// keep a window up that cannot be closed. install opens it again.
		u.progress.close()
		if pend.Forced {
			warning := cyc.eff.Doc.Updater.CriticalWarning
			if warning.Enabled {
				seconds := warning.Seconds
				var shown bool
				if p.Legacy {
					shown = notify.WarnCriticalUpdate(ps.Language, seconds)
				} else {
					shown = notify.WarnCriticalUpdateProduct(p.Name, seconds)
				}
				if shown {
					u.Log.Info("critical update warning shown, counting down",
						"product", p.Slug, "seconds", seconds, "language", ps.Language)
					// Honor the full promised countdown even if the user
					// dismisses the box early (notify returns immediately).
					select {
					case <-time.After(time.Duration(seconds) * time.Second):
					case <-ctx.Done():
						return ctx.Err()
					}
				} else {
					u.Log.Info("no active user session (console or RDP), skipping warning")
				}
			}
			// Re-checked here, not just at the top of apply: the warning
			// countdown above can run for cyc.eff.Doc.Updater.CriticalWarning
			// .Seconds (default 30s) of real time, long enough for a
			// destructive command to be admitted and committed while the app
			// is still running and untouched. Killing it now would be for
			// nothing - install() is about to refuse anyway.
			if u.destructivePendingNow() {
				u.logDestructiveSkipOnce()
				return nil
			}
			killed, err := process.TerminateAll(exe)
			if err != nil {
				u.Log.Warn(fmt.Sprintf("terminating %s reported errors", p.Name), "killed", killed, "error", err.Error())
			}
			u.Log.WarnEvent(logging.EventForcedKill, fmt.Sprintf("terminated %s for forced update", p.Name),
				"instances", killed, "target", pend.Version)
		} else if p.Legacy {
			// Notify the user via MSGBox that EMLy will be updated after they exit, then wait for the process to exit.
			msg := notify.Message{}
			if ps.Language == "it" {
				msg.Title = "EMLy - Aggiornamento sospeso"
				msg.Body = "Un aggiornamento per EMLy è pronto per essere installato. Chiudere l'applicazione per completare l'aggiornamento."
			} else {
				msg.Title = "EMLy - Update Pending"
				msg.Body = "An update for EMLy is ready to be installed. Please close the application to complete the update."
			}
			u.notifyBox(msg, 60)
			u.Log.Info("EMLy is running and update is not forced - waiting for exit", "target", pend.Version)
			if err := process.WaitForExit(ctx, exe); err != nil {
				// Context cancelled (service stop) or wait failure: the
				// pending entry stays persisted and resumes next start.
				return err
			}
			u.Log.Info("EMLy exited, proceeding with queued update", "target", pend.Version)
		} else {
			// A product other than EMLy never blocks the cycle: it may be a
			// chat kept open all day, and waiting on it would hold every
			// product after it. The pending entry stays; the next cycle
			// installs as soon as the app is closed.
			u.notifyWaitingOnce(p, pend.Version)
			u.Log.Info("app is running and the update is not forced - install deferred to a later cycle",
				"product", p.Slug, "target", pend.Version)
			return nil
		}
	}

	return u.installProduct(ctx, cyc, p, pend, ps)
}

// notifyWaitingOnce tells the user at the machine that p's update waits for
// the app to close - once per target version per service session.
func (u *Updater) notifyWaitingOnce(p *product.Product, version string) {
	if u.waitNotified == nil {
		u.waitNotified = map[string]string{}
	}
	if u.waitNotified[p.Slug] == version {
		return
	}
	u.waitNotified[p.Slug] = version
	u.notifyBox(notify.ProductWaitingMessage(p.Name), 60)
}

// installProduct runs the setup and the post-install steps. The pending
// entry is cleared only after the new version is confirmed on disk (EMLy's
// config.ini, or the product's detection chain).
//
// The Updater's own decision about the correct version always wins over
// whatever is already on disk: if a normal run doesn't leave the product
// reporting pend.Version - whether the setup itself failed, or it exited
// clean but the version still doesn't match (e.g. EMLy's installer treating a
// stale/inconsistent prior install as already up to date) - the setup is
// retried once. With CleanReinstall (always for EMLy) the existing install is
// first wiped with the product's own uninstaller, so the retry runs against a
// clean slate, ignoring whatever state was there before.
//
// A same-bits retry cannot fix anything a matching checksum already
// verified: if the cached setup itself is the problem (a stale or corrupt
// local copy, or the manifest having briefly pointed at a bad build), running
// it again just reproduces the same failure. So before the retry, the cache
// entry is dropped and re-fetched fresh from the source; only if that
// re-fetch cannot happen at all (e.g. offline) does the retry fall back to the
// original local copy.
func (u *Updater) installProduct(ctx context.Context, cyc *cycleState, p *product.Product, pend *state.Pending, ps productState) error {
	// Claims installing for the whole of this function - both setup runs,
	// the forced redownload and the uninstall/reinstall clean-retry, not
	// just the setup execution itself - so a destructive client command
	// cannot be admitted mid-uninstall. This is also the checkpoint that
	// stops a WaitForExit-released install (apply, above) from starting:
	// the client channel may have accepted a reboot/restart while EMLy was
	// still running and this cycle was waiting on it.
	if !u.beginInstall(p.Name + " install") {
		return fmt.Errorf("%s install skipped: a destructive client command is pending", p.Name)
	}
	defer u.endInstall()

	// from is the version installed before this attempt, omitted (empty) on
	// a fresh install (spec §8.3).
	from := ps.from()

	// Final integrity gate immediately before execution.
	if err := download.VerifyFile(pend.SetupPath, pend.SHA256); err != nil {
		// Corrupt cache: drop it so the next cycle re-downloads cleanly.
		_ = os.Remove(pend.SetupPath)
		_ = u.Store.ClearPendingFor(p.Slug)
		if p.Legacy {
			u.emitUpdateFailed(updateEvent{Target: "emly", FromVersion: from, ToVersion: pend.Version,
				WillRetry: true, Error: &wsclient.ErrorBody{Code: "checksum_mismatch", Message: err.Error()}})
		}
		return fmt.Errorf("refusing to install: %w", err)
	}

	started := u.clock()
	if p.Legacy {
		u.emit(wsclient.EvtUpdateStarted, updateEvent{Target: "emly", FromVersion: from, ToVersion: pend.Version,
			Forced: pend.Forced, Attempt: 1, Trigger: u.cycleTrigger})
	}
	u.progress.installing()

	reinstalled := false
	if err := u.runSetupAndVerifyProduct(p, pend, "running setup"); err != nil {
		how := "retrying with a fresh download"
		if p.Installer.CleanReinstall {
			how = "forcing a clean reinstall over the existing state"
		}
		u.Log.WarnEvent(logging.EventInstallFailed,
			fmt.Sprintf("%s did not reach the target version, %s", p.Name, how),
			"version", pend.Version, "error", err.Error())
		reinstalled = true

		if fresh, ferr := u.forceRedownloadProduct(ctx, cyc, p, pend, ps.Channel); ferr != nil {
			msg := "could not force a fresh download for the retry, retrying with the cached copy"
			if p.Legacy {
				msg = "could not force a fresh download for the clean-install retry, retrying with the cached copy"
			}
			u.Log.Warn(msg, "product", p.Slug, "version", pend.Version, "error", ferr.Error())
		} else {
			pend = fresh
		}

		u.progress.installing() // back from the re-download's progress
		label := "running setup (retry)"
		if p.Installer.CleanReinstall {
			label = "running setup (clean install)"
			if uerr := u.driverFor(p).Uninstall(); uerr != nil {
				// Best-effort: a failed cleanup is not itself a reason to give up
				// on the reinstall (e.g. no uninstaller present at all).
				u.Log.Warn("clean-install uninstall step reported an error, reinstalling anyway",
					"product", p.Slug, "version", pend.Version, "error", uerr.Error())
			}
		}

		if err := u.runSetupAndVerifyProduct(p, pend, label); err != nil {
			u.Log.ErrorEvent(logging.EventInstallFailed, fmt.Sprintf("%s clean install failed", p.Name),
				"version", pend.Version, "error", err.Error())
			if p.Legacy {
				u.emitUpdateFailed(updateEvent{Target: "emly", FromVersion: from, ToVersion: pend.Version,
					Attempt: 2, WillRetry: true, Error: &wsclient.ErrorBody{Code: installFailureCode(err), Message: err.Error()}})
			}
			return err // pending kept → retried next cycle
		}
	}

	u.Log.InfoEvent(logging.EventInstallOK, fmt.Sprintf("%s updated successfully", p.Name), "version", pend.Version)

	if p.Legacy {
		attempt := 1
		if reinstalled {
			attempt = 2
		}
		u.emit(wsclient.EvtUpdateApplied, updateEvent{Target: "emly", FromVersion: from, ToVersion: pend.Version,
			Forced: pend.Forced, Attempt: attempt, DurationMS: u.clock().Sub(started).Milliseconds(), Reinstalled: reinstalled})
	}

	u.progress.close() // before the toast, which says the same thing is over
	u.showProductToast(p, pend.Version)

	if err := u.Store.ClearPendingFor(p.Slug); err != nil {
		u.Log.Warn("failed to clear pending state", "product", p.Slug, "error", err.Error())
	}
	if err := u.downloadsFor(p).CleanupExcept(pend.Version); err != nil {
		u.Log.Warn("failed to clean up old downloads", "product", p.Slug, "error", err.Error())
	}

	if p.Legacy {
		// Association self-heal is a backstop; its failure must not fail the
		// (already successful) update.
		exePath := assoc.ExePath(u.Cfg.EMLyInstallDir, u.Cfg.EMLyExeName)
		mappings := assoc.DefaultMappings(u.Cfg.ProgIDEml, u.Cfg.ProgIDMsg)
		changed, err := assoc.Repair(exePath, mappings, func(format string, args ...any) {
			u.Log.Info(fmt.Sprintf(format, args...))
		})
		if err != nil {
			u.Log.Warn("file association repair failed", "error", err.Error())
		} else if changed {
			u.Log.InfoEvent(logging.EventAssocRepaired, "file associations repaired", "exe", exePath)
		}
	}
	return nil
}

// forceRedownloadProduct wipes p's downloads cache and pending entry, then
// re-resolves the manifest for channel and fetches whatever it currently
// offers from scratch. A same-checksum cache hit can't be trusted after a
// verified install still didn't land (stale local copy, or the manifest
// briefly having pointed at a bad build), so nothing short of a full wipe +
// fresh pull from the API guarantees clean bits. Returns the new, persisted
// pending entry, which keeps pend's Forced and Attempts.
func (u *Updater) forceRedownloadProduct(ctx context.Context, cyc *cycleState, p *product.Product, pend *state.Pending, channel string) (*state.Pending, error) {
	dl := u.downloadsFor(p)
	if err := dl.CleanupExcept(""); err != nil {
		u.Log.Warn("failed to fully clear the downloads cache before forcing a re-download",
			"product", p.Slug, "error", err.Error())
	}
	if err := u.Store.ClearPendingFor(p.Slug); err != nil {
		u.Log.Warn("failed to clear state.json before forcing a re-download", "product", p.Slug, "error", err.Error())
	}

	src, _, target, err := u.resolveProductTarget(ctx, cyc, p, channel)
	if err != nil {
		return nil, err
	}

	setupPath, err := dl.Ensure(u.progress.watch(ctx), src, target)
	if err != nil {
		return nil, fmt.Errorf("re-download failed: %w", err)
	}

	fresh := &state.Pending{
		Version:      target.Version,
		SetupPath:    setupPath,
		SHA256:       target.SHA256,
		Forced:       pend.Forced,
		DownloadedAt: time.Now().UTC(),
		Attempts:     pend.Attempts,
	}
	if err := u.Store.SetPendingFor(p.Slug, fresh); err != nil {
		u.Log.Warn("failed to persist re-downloaded pending update, continuing", "product", p.Slug, "error", err.Error())
	}
	msg := "re-downloaded setup for the retry"
	if p.Legacy {
		msg = "re-downloaded setup for clean-install retry"
	}
	u.Log.Info(msg, "product", p.Slug, "version", fresh.Version, "path", fresh.SetupPath)
	return fresh, nil
}

// runSetupAndVerifyProduct runs p's setup and confirms the target version is
// now on disk: EMLy through its config.ini (VerifyInstalled, unchanged),
// every other product by re-running its detection chain. label distinguishes
// the first attempt from the retry in the logs.
// installing is claimed by the caller (installProduct, above) for its whole
// duration - both attempts, plus the redownload/uninstall between them -
// not by this function per call.
func (u *Updater) runSetupAndVerifyProduct(p *product.Product, pend *state.Pending, label string) error {
	u.Log.Info(label, "product", p.Slug, "path", pend.SetupPath, "version", pend.Version)
	if err := u.driverFor(p).Install(pend.SetupPath, pend.Version); err != nil {
		return err
	}
	if p.Legacy {
		return installer.VerifyInstalled(u.Cfg.EMLyConfigFile, pend.Version)
	}
	r := product.Detect(p)
	if r.Outcome != product.Installed {
		return fmt.Errorf("post-install verification failed: %s", r)
	}
	older, err := manifest.Less(r.Version, pend.Version)
	if err != nil {
		return fmt.Errorf("post-install verification failed: %w", err)
	}
	if older {
		return fmt.Errorf("post-install verification failed: %s reports %s, expected %s", r.Source, r.Version, pend.Version)
	}
	return nil
}

// showProductToast: EMLy keeps its localized toast; other products get the
// Italian one with their own icon.
func (u *Updater) showProductToast(p *product.Product, version string) {
	if p.Legacy {
		u.showUpdateToast(version)
		return
	}
	msg := notify.ProductUpdatedMessage(p.Name, version)
	icon := filepath.Join(p.InstallDir, p.ExeName)
	launch := u.toastFn
	if launch == nil {
		self, err := os.Executable()
		if err != nil {
			u.Log.Warn("failed to resolve own executable path, skipping update toast", "error", err.Error())
			return
		}
		launch = func(icon, title, body string) bool { return notify.LaunchToast(self, icon, title, body) }
	}
	if launch(icon, msg.Title, msg.Body) {
		u.Log.Info("update-complete toast shown", "product", p.Slug, "version", version)
	} else {
		u.Log.Info("update-complete toast skipped (no active user session, console or RDP)", "product", p.Slug, "version", version)
	}
}
