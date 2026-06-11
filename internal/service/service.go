// Package service ties everything together: the Windows service handler and
// the poll loop implementing the update state machine (§6 of the spec).
package service

import (
	"context"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows/svc"

	"emlyupdater/internal/assoc"
	"emlyupdater/internal/config"
	"emlyupdater/internal/download"
	"emlyupdater/internal/installer"
	"emlyupdater/internal/logging"
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
}

// New builds an Updater on the standard ProgramData paths.
func New(cfg *config.Config, log *logging.Logger) *Updater {
	return &Updater{
		Cfg:       cfg,
		Log:       log,
		Store:     &state.Store{Path: config.StatePath()},
		Downloads: &download.Manager{Dir: config.DownloadsDir()},
	}
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
	emly := u.Cfg.ResolveEMLy()
	if emly.FreshInstall {
		u.Log.Info("EMLy config.ini not found — fresh-install mode",
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

	// 2) Normal poll: manifest via primary source, UNC as fallback.
	httpSrc := source.NewHTTPSource(u.Cfg.PrimaryManifestURL())
	httpSrc.UserAgent = u.Cfg.UserAgent
	httpSrc.APIKey = u.Cfg.APIKey
	resolver := &source.Resolver{
		Primary:  httpSrc,
		Fallback: source.NewUNCSource(u.Cfg.UNCRoot),
		Logf: func(format string, args ...any) {
			u.Log.Info(fmt.Sprintf(format, args...))
		},
	}
	src, m, err := resolver.Resolve(ctx)
	if err != nil {
		return err
	}
	if _, isUNC := src.(*source.UNCSource); isUNC {
		u.Log.WarnEvent(logging.EventSourceFallback, "primary update source unavailable, using UNC fallback",
			"unc", u.Cfg.UNCRoot)
	}

	target, err := src.ResolveTarget(m, emly.Channel)
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
			u.Log.Info("EMLy is running and update is not forced — waiting for exit", "target", p.Version)
			if err := process.WaitForExit(ctx, exe); err != nil {
				// Context cancelled (service stop) or wait failure: the
				// pending entry stays persisted and resumes next start.
				return err
			}
			u.Log.Info("EMLy exited, proceeding with queued update", "target", p.Version)
		}
	}

	return u.install(p)
}

// install runs the setup and the post-install steps. The pending entry is
// cleared only after the new version is confirmed in EMLy's config.ini.
func (u *Updater) install(p *state.Pending) error {
	// Final integrity gate immediately before execution.
	if err := download.VerifyFile(p.SetupPath, p.SHA256); err != nil {
		// Corrupt cache: drop it so the next cycle re-downloads cleanly.
		_ = os.Remove(p.SetupPath)
		_ = u.Store.ClearPending()
		return fmt.Errorf("refusing to install: %w", err)
	}

	u.Log.Info("running setup", "path", p.SetupPath, "version", p.Version)
	if err := installer.Run(p.SetupPath, p.Version, config.LogsDir()); err != nil {
		u.Log.ErrorEvent(logging.EventInstallFailed, "EMLy install failed",
			"version", p.Version, "error", err.Error())
		return err // pending kept → retried next cycle
	}

	if err := installer.VerifyInstalled(u.Cfg.EMLyConfigFile, p.Version); err != nil {
		u.Log.ErrorEvent(logging.EventInstallFailed, "EMLy install verification failed",
			"version", p.Version, "error", err.Error())
		return err // pending kept → retried next cycle
	}

	u.Log.InfoEvent(logging.EventInstallOK, "EMLy updated successfully", "version", p.Version)

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
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Updater.RunLoop(ctx)
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
