// Package service ties everything together: the Windows service handler and
// the poll loop implementing the update state machine (§6 of the spec).
package service

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/windows/svc"

	"emlyupdater/internal/assoc"
	"emlyupdater/internal/cert"
	"emlyupdater/internal/config"
	"emlyupdater/internal/download"
	"emlyupdater/internal/installer"
	"emlyupdater/internal/ipc"
	"emlyupdater/internal/logging"
	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/manifest"
	"emlyupdater/internal/notify"
	"emlyupdater/internal/process"
	"emlyupdater/internal/source"
	"emlyupdater/internal/state"
)

// Name is the Windows service name (also the Event Log source).
const Name = "EMLyUpdater"

// Updater holds the wiring for one running instance of the update loop.
type Updater struct {
	Cfg       *config.Config
	Log       *logging.Logger
	Store     *state.Store
	Downloads *download.Manager
	Machine   machineinfo.Info
	IPC       *ipc.Server
}

// New builds an Updater on the standard ProgramData paths.
func New(cfg *config.Config, log *logging.Logger) *Updater {
	machine := machineinfo.Collect()
	u := &Updater{
		Cfg:       cfg,
		Log:       log,
		Store:     &state.Store{Path: config.StatePath()},
		Downloads: &download.Manager{Dir: config.DownloadsDir()},
		Machine:   machine,
	}
	// Decide which manifest source this machine can actually reach before
	// the first cycle runs: cfg.Primary may be rewritten here.
	applySourcePolicy(cfg, log, config.ConfigPath(), machineinfo.NearestDomainController, machineinfo.LocalIPv4Addresses)

	u.IPC = ipc.New(cfg, log, func() machineinfo.Info { return u.Machine },
		assoc.ExePath(cfg.EMLyInstallDir, cfg.EMLyExeName))
	return u
}

// RunLoop runs update cycles until the context is cancelled. The first cycle
// starts immediately (it also resumes any pending update persisted before a
// restart/reboot); afterwards the loop polls on the configured interval.
func (u *Updater) RunLoop(ctx context.Context) {
	u.Log.Info("update loop started",
		"pollInterval", u.Cfg.PollInterval.String(),
		"primary", u.Cfg.Primary,
		"channelOverride", u.Cfg.ChannelOverride,
	)

	for {
		if err := u.Cycle(ctx); err != nil && ctx.Err() == nil {
			u.Log.Error("update cycle failed", "error", err.Error())
		}
		select {
		case <-time.After(u.Cfg.PollInterval):
		case <-ctx.Done():
			u.Log.Info("update loop stopped")
			return
		}
	}
}

// Cycle performs one full pass: resume pending → fetch manifest → decide →
// download → apply.
func (u *Updater) Cycle(ctx context.Context) error {
	// Trust-store self-heal, before anything that might run EMLy's setup.
	u.ensureCertificate()

	emly := u.Cfg.ResolveEMLy()
	if emly.FreshInstall {
		u.Log.Info("EMLy config.ini not found - fresh-install mode",
			"assumedVersion", emly.InstalledVersion, "channel", emly.Channel)
	}

	// 1) A persisted pending update takes priority over polling: it may have
	// been queued right before a reboot and must not be lost or re-fetched.
	st, err := u.Store.Load()
	if err != nil {
		u.Log.Warn("state file unreadable, starting fresh", "error", err.Error())
		st = &state.State{}
	}
	if p := st.Pending; p != nil {
		stillNeeded, err := manifest.Less(emly.InstalledVersion, p.Version)
		if err != nil {
			u.Log.Warn("pending update has invalid version, discarding", "version", p.Version, "error", err.Error())
			_ = u.Store.ClearPending()
		} else if !stillNeeded {
			// Installed by other means (or the pending entry is stale).
			u.Log.Info("pending update already satisfied, clearing", "version", p.Version)
			_ = u.Store.ClearPending()
			_ = u.Downloads.CleanupExcept("")
		} else if err := download.VerifyFile(p.SetupPath, p.SHA256); err != nil {
			u.Log.Warn("pending setup failed re-verification, discarding for re-download", "error", err.Error())
			_ = os.Remove(p.SetupPath)
			_ = u.Store.ClearPending()
		} else {
			u.Log.Info("resuming pending update", "version", p.Version, "forced", p.Forced)
			return u.apply(ctx, p, emly)
		}
	}

	// 2) Normal poll: manifest via the primary source.
	src, m, target, err := u.resolveTarget(ctx, emly.Channel)
	if err != nil {
		return err
	}

	needUpdate, err := manifest.Less(emly.InstalledVersion, target.Version)
	if err != nil {
		return err
	}
	if !needUpdate {
		u.Log.Debug("already on latest version", "installed", emly.InstalledVersion,
			"target", target.Version, "channel", emly.Channel)
		// Nothing pending, nothing needed: superseded setups can go.
		_ = u.Downloads.CleanupExcept("")
		return nil
	}

	forced, err := m.Forced(emly.InstalledVersion)
	if err != nil {
		return err
	}

	u.Log.InfoEvent(logging.EventUpdateFound, "update available",
		"installed", emly.InstalledVersion, "target", target.Version,
		"channel", emly.Channel, "forced", forced, "source", src.Name())

	setupPath, err := u.Downloads.Ensure(ctx, src, target)
	if err != nil {
		return fmt.Errorf("download/verification failed: %w", err)
	}

	p := &state.Pending{
		Version:      target.Version,
		SetupPath:    setupPath,
		SHA256:       target.SHA256,
		Forced:       forced,
		DownloadedAt: time.Now().UTC(),
	}
	// Persist before applying so a crash/reboot at any later point resumes
	// from the verified local file instead of re-downloading.
	if err := u.Store.SetPending(p); err != nil {
		u.Log.Warn("failed to persist pending update, continuing", "error", err.Error())
	}

	return u.apply(ctx, p, emly)
}

// resolveTarget fetches the update manifest from the primary source (with
// retries) and resolves it to a channel target. Shared by the normal poll in
// Cycle and by the forced re-download path in install.
func (u *Updater) resolveTarget(ctx context.Context, channel string) (source.Source, *manifest.Manifest, manifest.Target, error) {
	httpSrc := source.NewHTTPSource(u.Cfg.PrimaryManifestURL())
	httpSrc.UserAgent = u.Cfg.UserAgent
	httpSrc.APIKey = u.Cfg.APIKey
	httpSrc.Hostname = u.Machine.Hostname
	httpSrc.HWID = u.Machine.HWID
	httpSrc.ADDomain = u.Machine.ADDomain
	httpSrc.InternalIP = u.Machine.InternalIP
	resolver := &source.Resolver{
		Primary: httpSrc,
		Logf: func(format string, args ...any) {
			u.Log.Info(fmt.Sprintf(format, args...))
		},
	}
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

// apply installs a verified pending update according to EMLy's running state:
// not running → install now; running and non-forced → wait for exit; running
// and forced → optional WTS warning, then kill.
func (u *Updater) apply(ctx context.Context, p *state.Pending, emly config.EMLyInfo) error {
	exe := u.Cfg.EMLyExeName

	if process.IsRunning(exe) {
		if p.Forced {
			if u.Cfg.CriticalWarningEnabled {
				seconds := u.Cfg.CriticalWarningSeconds
				if notify.WarnCriticalUpdate(emly.Language, seconds) {
					u.Log.Info("critical update warning shown, counting down",
						"seconds", seconds, "language", emly.Language)
					// Honor the full promised countdown even if the user
					// dismisses the box early (notify returns immediately).
					select {
					case <-time.After(time.Duration(seconds) * time.Second):
					case <-ctx.Done():
						return ctx.Err()
					}
				} else {
					u.Log.Info("no active console session, skipping warning")
				}
			}
			killed, err := process.TerminateAll(exe)
			if err != nil {
				u.Log.Warn("terminating EMLy reported errors", "killed", killed, "error", err.Error())
			}
			u.Log.WarnEvent(logging.EventForcedKill, "terminated EMLy for forced update",
				"instances", killed, "target", p.Version)
		} else {
			// Notify the user via MSGBox that EMLy will be updated after they exit, then wait for the process to exit.
			msg := notify.Message{}
			if emly.Language == "it" {
				msg.Title = "EMLy - Aggiornamento sospeso"
				msg.Body = "Un aggiornamento per EMLy è pronto per essere installato. Chiudere l'applicazione per completare l'aggiornamento."
			} else {
				msg.Title = "EMLy - Update Pending"
				msg.Body = "An update for EMLy is ready to be installed. Please close the application to complete the update."
			}
			notify.SendNotifyBox(msg, 60)
			u.Log.Info("EMLy is running and update is not forced - waiting for exit", "target", p.Version)
			if err := process.WaitForExit(ctx, exe); err != nil {
				// Context cancelled (service stop) or wait failure: the
				// pending entry stays persisted and resumes next start.
				return err
			}
			u.Log.Info("EMLy exited, proceeding with queued update", "target", p.Version)
		}
	}

	return u.install(ctx, p, emly)
}

// install runs the setup and the post-install steps. The pending entry is
// cleared only after the new version is confirmed in EMLy's config.ini.
//
// The Updater's own decision about the correct version always wins over
// whatever is already on disk: if a normal run doesn't leave config.ini
// reporting p.Version - whether the setup itself failed, or it exited clean
// but the version still doesn't match (e.g. EMLy's installer treating a
// stale/inconsistent prior install as already up to date) - the existing
// install is wiped with EMLy's own uninstaller and Run is retried once
// against a clean slate, ignoring whatever state was there before.
//
// A same-bits retry cannot fix anything a matching checksum already
// verified: if the cached setup itself is the problem (a stale or corrupt
// local copy, or the manifest having briefly pointed at a bad build), running
// it again just reproduces the same failure. So before the clean-install
// retry, the cache entry is dropped and re-fetched fresh from the source;
// only if that re-fetch cannot happen at all (e.g. offline) does the retry
// fall back to the original local copy.
func (u *Updater) install(ctx context.Context, p *state.Pending, emly config.EMLyInfo) error {
	// Final integrity gate immediately before execution.
	if err := download.VerifyFile(p.SetupPath, p.SHA256); err != nil {
		// Corrupt cache: drop it so the next cycle re-downloads cleanly.
		_ = os.Remove(p.SetupPath)
		_ = u.Store.ClearPending()
		return fmt.Errorf("refusing to install: %w", err)
	}

	if err := u.runSetupAndVerify(p, "running setup"); err != nil {
		u.Log.WarnEvent(logging.EventInstallFailed,
			"EMLy did not reach the target version, forcing a clean reinstall over the existing state",
			"version", p.Version, "error", err.Error())

		if fresh, ferr := u.forceRedownload(ctx, p, emly.Channel); ferr != nil {
			u.Log.Warn("could not force a fresh download for the clean-install retry, retrying with the cached copy",
				"version", p.Version, "error", ferr.Error())
		} else {
			p = fresh
		}

		if uerr := installer.Uninstall(u.Cfg.EMLyInstallDir, config.LogsDir()); uerr != nil {
			// Best-effort: a failed cleanup is not itself a reason to give up
			// on the reinstall (e.g. no uninstaller present at all).
			u.Log.Warn("clean-install uninstall step reported an error, reinstalling anyway",
				"version", p.Version, "error", uerr.Error())
		}

		if err := u.runSetupAndVerify(p, "running setup (clean install)"); err != nil {
			u.Log.ErrorEvent(logging.EventInstallFailed, "EMLy clean install failed",
				"version", p.Version, "error", err.Error())
			return err // pending kept → retried next cycle
		}
	}

	u.Log.InfoEvent(logging.EventInstallOK, "EMLy updated successfully", "version", p.Version)

	u.showUpdateToast(p.Version)

	if err := u.Store.ClearPending(); err != nil {
		u.Log.Warn("failed to clear pending state", "error", err.Error())
	}
	if err := u.Downloads.CleanupExcept(p.Version); err != nil {
		u.Log.Warn("failed to clean up old downloads", "error", err.Error())
	}

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

	return nil
}

// forceRedownload wipes the downloads cache and the persisted state file
// entirely, then re-resolves the manifest for channel and fetches whatever
// it currently offers from scratch. A same-checksum cache hit can't be
// trusted after a verified install still didn't land (stale local copy, or
// the manifest briefly having pointed at a bad build), so nothing short of a
// full wipe + fresh pull from the API guarantees clean bits. Returns the new,
// persisted pending entry.
func (u *Updater) forceRedownload(ctx context.Context, p *state.Pending, channel string) (*state.Pending, error) {
	if err := u.Downloads.CleanupExcept(""); err != nil {
		u.Log.Warn("failed to fully clear the downloads cache before forcing a re-download",
			"error", err.Error())
	}
	if err := u.Store.ClearPending(); err != nil {
		u.Log.Warn("failed to clear state.json before forcing a re-download", "error", err.Error())
	}

	src, _, target, err := u.resolveTarget(ctx, channel)
	if err != nil {
		return nil, err
	}

	setupPath, err := u.Downloads.Ensure(ctx, src, target)
	if err != nil {
		return nil, fmt.Errorf("re-download failed: %w", err)
	}

	fresh := &state.Pending{
		Version:      target.Version,
		SetupPath:    setupPath,
		SHA256:       target.SHA256,
		Forced:       p.Forced,
		DownloadedAt: time.Now().UTC(),
	}
	if err := u.Store.SetPending(fresh); err != nil {
		u.Log.Warn("failed to persist re-downloaded pending update, continuing", "error", err.Error())
	}
	u.Log.Info("re-downloaded setup for clean-install retry", "version", fresh.Version, "path", fresh.SetupPath)
	return fresh, nil
}

// runSetupAndVerify runs EMLy's setup for p and confirms config.ini now
// reports p.Version. label distinguishes the first attempt from the
// clean-install retry in the logs.
func (u *Updater) runSetupAndVerify(p *state.Pending, label string) error {
	u.Log.Info(label, "path", p.SetupPath, "version", p.Version)
	if err := installer.Run(p.SetupPath, p.Version, config.LogsDir()); err != nil {
		return err
	}
	return installer.VerifyInstalled(u.Cfg.EMLyConfigFile, p.Version)
}

// ensureCertificate installs the 3gIT code-signing certificate into the
// machine trust stores and, when somebody is logged on at the console, into
// that user's stores as well.
//
// Best-effort by design, like the file-association self-heal: a certificate
// that cannot be installed costs a friendlier UAC prompt, never an update. No
// failure here is ever returned to the caller.
//
// This runs on every cycle rather than once at service start. A user who logs
// on after boot - the normal case, not the exception - would otherwise never
// be covered, and manual removal of the certificate would never heal. When it
// is already installed everywhere the cost is two syscalls per store and
// nothing above Debug reaches the log.
func (u *Updater) ensureCertificate() {
	if !u.Cfg.CertificateEnabled {
		return
	}

	c, der, err := cert.Embedded()
	if err != nil {
		u.Log.Warn("embedded code-signing certificate unusable, skipping install",
			"error", err.Error())
		return
	}

	targets := cert.MachineTargets()
	if sid, ok := notify.ConsoleUserSID(); ok {
		targets = append(targets, cert.UserTargets(sid)...)
	} else {
		// ConsoleUserSID collapses "nobody is logged on" and "the token could
		// not be queried" into the same false - the first is routine and the
		// second is rare, and neither changes what we do. Say both.
		u.Log.Debug("no console user available (nobody logged on, or the user token " +
			"could not be queried), installing certificate for the machine only")
	}

	installed, err := cert.Ensure(der, targets, func(format string, args ...any) {
		u.Log.Debug(fmt.Sprintf(format, args...))
	})
	for _, store := range installed {
		u.Log.InfoEvent(logging.EventCertInstalled, "code-signing certificate installed",
			"store", store,
			"subject", c.Subject.CommonName,
			"notAfter", c.NotAfter.Format(time.RFC3339))
	}
	if err != nil {
		u.Log.WarnEvent(logging.EventCertFailed, "code-signing certificate install incomplete",
			"error", err.Error(), "storesWritten", len(installed), "storesTried", len(targets))
		return
	}
	if len(installed) == 0 {
		u.Log.Debug("code-signing certificate already present in all trust stores",
			"stores", len(targets))
	}
}

// showUpdateToast announces a completed update in the active console user's
// session. Best-effort and non-fatal: EMLy is already updated by this point,
// so a toast failure (no console session, WTS/token errors, ...) is only
// ever logged, never returned as an install error. Channel/language are
// re-read post-install so the notification reflects EMLy's actual current
// config rather than the pre-update snapshot.
func (u *Updater) showUpdateToast(version string) {
	post := u.Cfg.ResolveEMLy()
	msg := notify.UpdateCompleteMessage(post.Language, version, post.Channel)

	self, err := os.Executable()
	if err != nil {
		u.Log.Warn("failed to resolve own executable path, skipping update toast", "error", err.Error())
		return
	}

	emlyExe := assoc.ExePath(u.Cfg.EMLyInstallDir, u.Cfg.EMLyExeName)
	if notify.LaunchToast(self, emlyExe, msg.Title, msg.Body) {
		u.Log.Info("update-complete toast shown", "version", version)
	} else {
		u.Log.Info("update-complete toast skipped (no active console session)", "version", version)
	}
}

// Handler adapts Updater to the SCM. svc.Run blocks until Execute returns.
type Handler struct {
	Updater *Updater
}

// Execute implements svc.Handler: it reports Running, drives the update loop
// in a goroutine, and translates Stop/Shutdown into context cancellation.
func (h *Handler) Execute(_ []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	changes <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		h.Updater.RunLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		h.Updater.IPC.Serve(ctx)
	}()
	go func() {
		wg.Wait()
		close(done)
	}()

	changes <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		c := <-r
		switch c.Cmd {
		case svc.Interrogate:
			changes <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			changes <- svc.Status{State: svc.StopPending}
			cancel()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				h.Updater.Log.Warn("update loop did not stop within 30s, exiting anyway")
			}
			return false, 0
		}
	}
}
