// Package service ties everything together: the Windows service handler and
// the poll loop implementing the update state machine (§6 of the spec).
package service

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
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
	"emlyupdater/internal/policy"
	"emlyupdater/internal/process"
	"emlyupdater/internal/source"
	"emlyupdater/internal/state"
	"emlyupdater/internal/winget"
	"emlyupdater/internal/wsclient"
)

// Name is the Windows service name (also the Event Log source).
const Name = "EMLyUpdater"

// Updater holds the wiring for one running instance of the update loop.
type Updater struct {
	Cfg   *config.Config
	Log   *logging.Logger
	Store *state.Store
	// Downloads caches EMLy's setups, SelfDownloads the updater's own
	// installers. Two managers over two directories, so neither cleanup can
	// reach the other's files.
	Downloads     *download.Manager
	SelfDownloads *download.Manager
	Machine       machineinfo.Info
	IPC           *ipc.Server

	// Policy holds the current remote configuration snapshot. Everything
	// operational is read from it, per cycle, through beginCycle: config.ini
	// only supplies the bootstrap and the values the default policy is
	// derived from. Built by initPolicy on the first RunLoop.
	Policy   *policy.Store
	Defaults policy.Defaults
	// CachePath overrides config.RemoteConfigPath() in tests.
	CachePath string

	// sourcesUnreachableNotified marks that the console user has already
	// been toasted for the outage currently in progress - Cycle runs
	// sequentially in one goroutine, so this needs no locking. It is only
	// set once LaunchToast actually shows the toast, and cleared the next
	// time resolveTarget succeeds, so a server down for hours nags the user
	// once per outage rather than once per poll - but a still-ongoing
	// outage keeps trying every cycle until someone is there to see it.
	sourcesUnreachableNotified bool

	// cur is the state the last beginCycle produced: the snapshot, this
	// machine's facts and the server chain they select. Read by the IPC
	// server on its own goroutines, hence the atomic.
	cur atomic.Pointer[cycleState]

	// site carries what the source policy remembers between cycles (last DC
	// answer, last decision logged) - see sourcepolicy.go.
	site siteState

	// preferred is the server of the chain this service session found
	// reachable when the chain's head was not, nil for none - see
	// preferred.go. clientWSWake nudges the presence supervisor when it
	// changes; nil (tests) simply means no nudge.
	preferred    atomic.Pointer[string]
	clientWSWake chan struct{}

	// paused mirrors control.updater.enabled so event 904 fires on the
	// transition rather than on every cycle.
	paused bool
	// appliedLogging is the last logging section pushed into the logger.
	appliedLogging *policy.LoggingSettings
	// lastConfigAttempt, lastStaleWarn and configUnreachable pace the config
	// fetch, the staleness warning and the once-per-outage event 901.
	lastConfigAttempt time.Time
	lastStaleWarn     time.Time
	configUnreachable bool
	// consoleDebug records logging.New's console flag so the policy derived
	// from config.ini does not lower the foreground `run` mode back to info.
	consoleDebug bool

	// Seams for the tests: the machine-facts lookups, the clock and the
	// config fetch. In production these are the real ones, set by New.
	dcFn          dcLookup
	ipsFn         localIPsLookup
	loggedUserFn  func() machineinfo.UserSession
	nowFn         func() time.Time
	fetchConfigFn func(ctx context.Context, url, etag string) (*source.ConfigResponse, error)

	// clientWSWatch overrides clientWSWatchInterval (how often the presence
	// supervisor re-reads the current cycle) when > 0. Zero means the real
	// interval. Tests set it short so the supervisor suite does not spend
	// real seconds asleep per test; production never sets it.
	clientWSWatch time.Duration
	// clientWSInitialDelayFn overrides the presence supervisor's randomised
	// startup delay (see runClientWS). Tests set it to return 0 so the
	// supervisor dials immediately instead of sleeping up to 60s; nil means
	// the real jittered delay.
	clientWSInitialDelayFn func() time.Duration

	// sessionChanges carries the SCM's session notifications from the
	// service control loop to watchSessions (sessionwatch.go). nil disables
	// the watcher.
	sessionChanges chan machineinfo.SessionChange
	// Seams for the session watcher's tests: the WTS resolution, the settle
	// window (when > 0) and a callback observing each resolved burst.
	resolveSessionFn func(machineinfo.SessionChange) machineinfo.SessionChange
	sessionSettle    time.Duration
	onSessionChange  func(c machineinfo.SessionChange, changed bool)

	// startedAt is when this process's Updater was built (New), used for
	// service.started's started_at and the boot-window heuristic in
	// serviceStartedReason.
	startedAt time.Time

	// wsSession is the live v2 client-channel session, or nil when none is
	// up - emit and the welcome burst goroutine read/write it from different
	// goroutines, hence the atomic. All writes go through
	// storeWSSession/clearWSSession, which serialise on wsSessionMu together
	// with wsGen so a stale connection's burst can never resurrect it after
	// runClientWS has already cleared it for a newer one.
	wsSession   atomic.Pointer[wsclient.Session]
	wsSessionMu sync.Mutex
	// wsGen is the current presence-channel connection attempt's generation,
	// guarded by wsSessionMu. See nextWSGeneration/storeWSSession/clearWSSession.
	wsGen uint64
	// eventsMu guards eventBuf, the in-memory buffer emit falls back to
	// while no v2 session is up (see clientevents.go).
	eventsMu sync.Mutex
	eventBuf []bufferedEvent
	// startedSent marks that service.started has already been sent once for
	// this process (it must not repeat on a reconnect).
	startedSent atomic.Bool
	// serviceStartedOnce/serviceStartedPayload cache service.started's
	// payload (see cachedServiceStarted): it is built at most once per
	// process, because building it consumes the pending command ids, and a
	// retry after a failed send must report the same ids the first attempt
	// would have.
	serviceStartedOnce    sync.Once
	serviceStartedPayload wsclient.ServiceStarted
	// selfLanded is set by reconcileSelfUpdate on selfupdate.OutcomeLanded
	// and read once by buildServiceStarted, for service.started's
	// previous_version/reason=self_update. Written on the poll goroutine,
	// read on the client-channel goroutine, hence the atomic.
	selfLanded atomic.Pointer[state.SelfUpdate]

	// emitFn overrides emit in tests, so the session watcher and other
	// callers can be exercised without a real client-channel session.
	emitFn func(name string, payload any)
	// systemFactsFn overrides machineinfo.CollectSystemFacts in tests.
	systemFactsFn func(time.Time) machineinfo.SystemFacts
	// netInterfacesFn overrides machineinfo.NetworkInterfaces in tests.
	netInterfacesFn func() []machineinfo.NetInterface
	// emlyRunningFn overrides process.IsRunning in tests.
	emlyRunningFn func() bool
	// wsSendFn overrides the welcome burst's per-event sender in tests, so a
	// failed service.started send (and the retry it causes) can be exercised
	// without a real *wsclient.Session.
	wsSendFn func(name string, payload any) error
	// welcomeBurstFn overrides the whole welcome burst in tests, so Welcome's
	// own contract (it must return immediately) can be exercised without a
	// real *wsclient.Session.
	welcomeBurstFn func(gen uint64, s *wsclient.Session)

	// commands is the dedupe ring for the client-channel command executor
	// (clientcmd.go): a redelivered command id is confirmed, not re-run.
	commands commandRing
	// runningMu/running track, per command name, whether an instance of it
	// is currently executing - so a second delivery of a different command
	// with the same name is refused busy instead of running concurrently.
	runningMu sync.Mutex
	running   map[string]bool

	// listUpgradableFn overrides winget.ListUpgradable in tests.
	listUpgradableFn func(context.Context) ([]winget.Package, error)
	// emlyCheckFn/updaterCheckFn override the manifest dry runs in tests, so
	// their network paths need not be exercised.
	emlyCheckFn    func(context.Context, *cycleState) wsclient.ManifestCheck
	updaterCheckFn func(context.Context, *cycleState) wsclient.ManifestCheck
	// verifySelfSetupFn overrides applySelfUpdate's Authenticode signature
	// check in tests - there is no way to produce a file signed by the
	// embedded 3gIT certificate's private key outside a real release build.
	// nil means the real verifySelfSetup.
	verifySelfSetupFn func(string) error
	// launchFn overrides applySelfUpdate's call to selfupdate.Launch in
	// tests, so a launch failure (and the update.failed it now emits) can be
	// exercised without starting a real process. nil means the real
	// selfupdate.Launch.
	launchFn func(setupPath, logPath string) error

	// installing counts installs currently in flight: EMLy's own (the whole
	// of install() - both setup runs, the forced redownload and the
	// uninstall/reinstall clean-retry, not just runSetupAndVerify, since a
	// destructive command must not be admitted mid-uninstall either) and
	// this updater's own (applySelfUpdate, right before Launch). It gates
	// admitDestructive (clientpower.go), which refuses service.restart and
	// machine.reboot as busy while it is nonzero. After a successful
	// self-update launch it intentionally stays >0 for the rest of this
	// process's life - the service is about to stop, there is nothing left
	// to un-mark, and refusing destructive commands as busy until the
	// restart actually happens is the correct behaviour, not a bug.
	installing atomic.Int32
	// restartFn/rebootFn are the seams clientpower.go's runDestructive uses
	// in place of launching restart-service / calling power.Reboot; tests
	// set them so no test ever restarts the service or reboots the machine.
	restartFn func() error
	rebootFn  func(time.Duration) error

	// destructiveMu guards destructivePending, and is also held for the
	// whole check-then-act step wherever an install is about to start
	// (beginInstall, used by install() and applySelfUpdate) or a
	// destructive command is about to be committed (commitDestructive,
	// used by runDestructive right before restartFn/rebootFn) - see
	// clientpower.go. Sharing one mutex between both sides is what makes
	// the check and the act atomic with respect to each other: without it,
	// an install could start in the gap between admitDestructive's busy
	// check and runDestructive actually committing to the reboot/restart,
	// or a destructive command could be committed in the gap between an
	// install's own busy check and it incrementing installing.
	destructiveMu sync.Mutex
	// destructivePending marks that a service.restart or machine.reboot has
	// been committed for this process: set by commitDestructive right
	// before restartFn/rebootFn runs, cleared if that call fails. While
	// set, Cycle skips self-update and the EMLy pending/install path
	// entirely (logged once per pending episode at Info), and
	// admitDestructive refuses a second destructive command as busy.
	destructivePending bool
	// destructiveDeadline is when destructivePending stops being trusted:
	// commitDestructive sets it to roughly "when the reboot/restart should
	// have already happened, plus a safety margin" (see rebootGrace and
	// serviceRestartGrace in clientpower.go). An aborted shutdown
	// (`shutdown /a`) or a restart-service child that never brings the
	// service back (a hung cmdStop, an SCM that refuses the start) would
	// otherwise leave this flag - and therefore every Cycle and every new
	// destructive command - stuck for the rest of the process's life, with
	// no way to recover the host except physically touching it.
	// destructivePendingLocked is what actually enforces the deadline,
	// lazily, on whichever check happens to run next.
	destructiveDeadline time.Time
	// destructiveCommandID is the msg.ID commitDestructive was called with,
	// remembered alongside destructiveDeadline so an auto-expiry
	// (destructivePendingLocked) can also remove that command's
	// state.json pendingCommands record. Without this, an aborted
	// reboot/restart whose deadline lapses leaves the record behind, and
	// the next start's service.started (buildServiceStarted,
	// clientevents.go) reads it back and reports the aborted command as
	// completed - reboot always with reason "boot".
	destructiveCommandID string
	// destructiveSkipLogged marks that Cycle's "skipping this cycle"
	// message has already been logged for the destructivePending episode
	// currently in progress, so a countdown of several minutes does not
	// repeat the line on every poll. Reset whenever destructivePending
	// clears, explicitly or by expiry.
	destructiveSkipLogged bool

	// announced remembers, per target ("emly"/"updater"), the last version
	// announceUpdate has already sent update.available for - poll goroutine
	// only, so it needs no locking.
	announced map[string]string
	// updateFailed remembers which (target, to_version, attempt, code)
	// update.failed occurrences have already been reported this process -
	// poll goroutine only (selfUpdate/applySelfUpdate/install all run on
	// it), so it needs no locking, same as announced. Without this,
	// reconcileSelfUpdate would repeat "version_mismatch" every cycle a
	// launch stays pending (the cooldown, every retry, and forever once
	// GaveUp is recorded), and a broken mirror would repeat
	// download_failed/signature_invalid on every cycle since a failed
	// download never advances the attempt counter. See emitUpdateFailed
	// (selfupdate.go).
	updateFailed map[updateFailedKey]bool
	// wakeReason is set by RunLoop right before a cycle that was triggered by
	// a notify wake (rather than the poll timer) and read once, at the top of
	// Cycle, to compute this cycle's trigger for the update.started events it
	// emits; empty means the ordinary poll ("cycle").
	wakeReason string
	// cycleTrigger is the current cycle's trigger ("cycle", "resume", or
	// whatever wakeReason named it), stashed here rather than threaded
	// through apply/install's signatures - both run on the poll goroutine, so
	// this needs no locking either.
	cycleTrigger string

	// wake carries the reason for an early wake-up of RunLoop, sent by
	// handleNotify (clientnotify.go) after its jittered delay. Buffered 1:
	// several notifies before RunLoop's select picks it up collapse into the
	// one pending wake, so a burst of server pushes never queues more than
	// one extra cycle. New sets it; a literal Updater built by a test (that
	// never calls RunLoop) may leave it nil, which is fine for a send
	// (select/default below) but would block forever on a receive.
	wake chan string
	// forceConfig is set by a config.published notify and consumed by
	// RunLoop's next refreshConfig call (Swap(false)): the notify only knows
	// a newer document exists, not its content, so the next fetch is made
	// unconditional rather than waiting for the poll interval to elapse.
	forceConfig atomic.Bool
	// jitterFn/afterFunc are scheduleWake's seams: tests pin the jitter to a
	// deterministic value and run afterFunc's callback synchronously instead
	// of waiting on a real timer. nil means the real rand.Int64N / time.AfterFunc.
	jitterFn  func(max time.Duration) time.Duration
	afterFunc func(time.Duration, func())
}

// clock is the time source; tests pin it.
func (u *Updater) clock() time.Time {
	if u.nowFn != nil {
		return u.nowFn()
	}
	return time.Now()
}

// New builds an Updater on the standard ProgramData paths. consoleDebug
// mirrors the flag passed to logging.New, so the policy that is derived from
// config.ini keeps the foreground `run` mode at debug level.
func New(cfg *config.Config, log *logging.Logger, consoleDebug bool) *Updater {
	machine := machineinfo.Collect()
	u := &Updater{
		Cfg:       cfg,
		Log:       log,
		Store:     &state.Store{Path: config.StatePath()},
		Downloads: &download.Manager{Dir: config.DownloadsDir()},
		SelfDownloads: &download.Manager{
			Dir:    config.SelfDownloadsDir(),
			Prefix: "EMLyUpdater-",
		},
		Machine:      machine,
		consoleDebug: consoleDebug,
		dcFn:         machineinfo.NearestDomainController,
		ipsFn:        machineinfo.LocalIPv4Addresses,
		loggedUserFn: machineinfo.LoggedUser,
		clientWSWake: make(chan struct{}, 1),
		wake:         make(chan string, 1),
		startedAt:    time.Now(),

		sessionChanges: make(chan machineinfo.SessionChange, sessionChangeBuffer),
	}
	u.IPC = ipc.New(cfg, log, func() machineinfo.Info { return u.Machine },
		assoc.ExePath(cfg.EMLyInstallDir, cfg.EMLyExeName))
	u.IPC.SetPolicyProvider(u.policyView)
	return u
}

// RunLoop runs update cycles until the context is cancelled. The first cycle
// starts immediately (it also resumes any pending update persisted before a
// restart/reboot); afterwards the loop polls on the configured interval.
func (u *Updater) RunLoop(ctx context.Context) {
	// The last-known-good document, or the policy derived from config.ini,
	// before anything else: every decision below reads from it.
	u.initPolicy()
	// One fetch attempt before the first cycle, so a machine that can reach
	// the API starts on the current policy. It is not retried - the first
	// cycle must not wait on the network.
	u.refreshConfig(ctx, true)

	// Which site (and therefore which servers) this machine is at. This
	// runs here and not in New because the domain controller lookup retries
	// a boot-time failure for up to the configured window, and New is called
	// before svc.Run has reported the service started - blocking there is
	// what the SCM's start timeout counts against.
	cyc := u.beginCycle(ctx, true)

	u.Log.Info("update loop started",
		"pollInterval", cyc.eff.Doc.Updater.PollInterval().String(),
		"site", cyc.site,
		"channelOverride", cyc.eff.Doc.Updater.Channel(),
		"policyRevision", cyc.snap.Revision(),
		"policySource", cyc.snap.Source.String(),
	)

	// Seed the command dedupe ring from whatever pending command ids
	// survived from a previous process (state.json) before the presence
	// channel below makes its first connection attempt - a server that
	// re-sends the same service.restart/machine.reboot id after the
	// reconnect must be told "already seen", not have it executed again.
	u.seedCommandRing()

	// The presence channel runs for the life of the service, beside the poll
	// loop rather than inside it: it follows the same server chain beginCycle
	// picks but has its own reconnection schedule, and holding a connection
	// open for days has nothing to do with a cycle that runs every few
	// minutes. It starts here, after the first beginCycle, because there is
	// no server to point it at before one has run.
	//
	// RunLoop waits for it on the way out so the service handler does not
	// report the service stopped with a connection still open.
	//
	// The supervisor gets its own child context, cancelled explicitly before
	// the wait below rather than sharing ctx directly. runClientWS only ever
	// returns when its context ends, and ctx itself is only cancelled by an
	// SCM stop - so an unrecovered panic inside Cycle (which shares this
	// goroutine's stack with nothing, but whose panic still has to unwind
	// through this defer) would otherwise leave presence.Wait() blocked
	// forever: the process never exits, the SCM still reports Running, and
	// the dashboard keeps reporting the wedged machine as online. Cancelling
	// presenceCtx first guarantees runClientWS unwinds promptly on that path
	// too, not just on the normal stop path where ctx itself gets cancelled.
	presenceCtx, stopPresence := context.WithCancel(ctx)
	var presence sync.WaitGroup
	presence.Add(1)
	go func() {
		defer presence.Done()
		u.runClientWS(presenceCtx)
	}()
	defer func() {
		stopPresence()
		presence.Wait()
	}()

	first := true
	for {
		if !first {
			// Every later cycle re-fetches the document when it is due and
			// re-evaluates the site: a laptop that changed subnet, or a
			// policy that changed under it, takes effect here. Swap(false)
			// both reads and clears forceConfig, so a config.published
			// notify forces exactly the next fetch, not every one after it.
			u.refreshConfig(ctx, u.forceConfig.Swap(false))
			cyc = u.beginCycle(ctx, false)
		}
		first = false

		if err := u.Cycle(ctx, cyc); err != nil && ctx.Err() == nil {
			u.Log.Error("update cycle failed", "error", err.Error())
		}
		u.wakeReason = ""
		select {
		case <-time.After(cyc.eff.Doc.Updater.PollInterval()):
		case reason := <-u.wake:
			u.wakeReason = reason
			u.Log.Info("update loop woken early", "reason", reason)
		case <-ctx.Done():
			u.Log.Info("update loop stopped")
			return
		}
	}
}

// Cycle performs one full pass: resume pending → fetch manifest → decide →
// download → apply.
func (u *Updater) Cycle(ctx context.Context, cyc *cycleState) error {
	if cyc == nil {
		cyc = u.current()
	}
	// This cycle's trigger for the update.started events install() emits
	// below (spec §8.3): empty wakeReason is the ordinary poll timer, set by
	// RunLoop otherwise when a notify wakes the loop early. Stashed in a
	// field rather than threaded through apply/install's signatures - both
	// run on this same poll goroutine.
	trigger := u.wakeReason
	if trigger == "" {
		trigger = "cycle"
	}
	u.cycleTrigger = trigger
	u.Log.Debug("update cycle starting",
		"policyRevision", cyc.snap.Revision(), "policySource", cyc.snap.Source.String(),
		"site", cyc.site, "overrides", len(cyc.eff.Applied))

	// Trust-store self-heal, before anything that might run a setup - the
	// self-update below verifies an Authenticode signature that chains to this
	// very certificate.
	u.ensureCertificate(cyc)

	// A paused updater still fetched its configuration (that is what can
	// un-pause it), still healed the trust store and still serves IPC - but
	// it downloads and installs nothing, its own release included.
	if !u.applyControlGate(cyc) {
		return nil
	}

	// A service.restart or machine.reboot accepted over the client channel
	// has already been committed (runDestructive, clientpower.go) - the
	// service is going to stop on its own within the announced delay.
	// Starting a self-update or an EMLy install now would either race that
	// shutdown or, for a forced kill, tear down a setup mid-run because the
	// user closed EMLy in response to the reboot warning. This is the
	// coarse, cycle-level skip; install() (below, via beginInstall) is the
	// authoritative, race-safe checkpoint for a destructive command
	// admitted after this check passes but before an install actually
	// starts - e.g. while apply() is waiting on WaitForExit.
	if u.destructivePendingNow() {
		u.logDestructiveSkipOnce()
		return nil
	}

	// The updater updates itself first, ahead of even a pending EMLy install.
	// If this build has a bug in the way it handles EMLy, it has to be able to
	// replace itself before exercising that bug again; the pending entry is
	// persisted and resumes under the new binary. Returning here is not
	// optional - the setup is already stopping this service.
	if u.selfUpdate(ctx, cyc) {
		return nil
	}

	emly := u.Cfg.ResolveEMLyWithChannel(cyc.eff.Doc.Updater.Channel())
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
			u.cycleTrigger = "resume"
			return u.apply(ctx, cyc, p, emly)
		}
	}

	// 2) Normal poll: manifest via this machine's server chain.
	src, m, target, err := u.resolveTarget(ctx, cyc, emly.Channel)
	if err != nil {
		u.notifySourcesUnreachable()
		return err
	}
	u.sourcesUnreachableNotified = false

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

	enabled, _ := cyc.eff.UpdaterEnabled(cyc.host.Now)
	mc := wsclient.ManifestCheck{Target: "emly", Channel: emly.Channel, AvailableVersion: target.Version,
		UpdateAvailable: true, Critical: forced, MinRequiredVersion: m.MinRequiredVersion,
		Decision: emlyDecision(true, forced, u.emlyRunning(), !enabled),
		Source: u.serverRef(cyc, sourceURL(src)), CheckedAt: u.clock().UTC().Format(time.RFC3339)}
	if !emly.FreshInstall {
		mc.InstalledVersion = emly.InstalledVersion
	}
	u.announceUpdate(mc)

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

	return u.apply(ctx, cyc, p, emly)
}

// newHTTPSource builds an HTTPSource for manifestURL with this machine's
// identity headers attached.
//
// Everything but the logged-on user comes from the snapshot New took at
// service startup - those are machine facts, and collecting them shells out
// to PowerShell. X-EMLy-LoggedUser is resolved here instead, on every source
// this builds: who is at the machine changes through the day, and a value
// frozen at boot would report whoever happened to be logged on when the
// service started (usually nobody) for the machine's whole uptime. The
// lookup is a WTS enumeration, so it costs no process spawn. X-EMLy-AppVersion
// is re-read here for the same reason: a setup this updater just ran changes
// the installed release, and the header has to report what is on disk now.
func (u *Updater) newHTTPSource(manifestURL string) *source.HTTPSource {
	httpSrc := source.NewHTTPSource(manifestURL)
	httpSrc.UserAgent = u.Cfg.UserAgent
	httpSrc.APIKey = u.Cfg.APIKey
	httpSrc.Hostname = u.Machine.Hostname
	httpSrc.HWID = u.Machine.HWID
	httpSrc.ADDomain = u.Machine.ADDomain
	httpSrc.InternalIP = u.Machine.InternalIP
	httpSrc.OSVersion = u.Machine.OSVersion
	httpSrc.Serial = u.Machine.Serial
	httpSrc.Product = u.Machine.Product
	httpSrc.EMLyVersion = u.emlyVersion()
	session := u.loggedUser()
	httpSrc.LoggedUser = session.User
	httpSrc.LoggedUserState = string(session.State)
	httpSrc.LoggedUserDisconnectedAt = session.DisconnectedAt
	return httpSrc
}

// emlyVersion reads the installed EMLy release from EMLy's config.ini for the
// X-EMLy-AppVersion header, and returns "" when EMLy is not installed: the
// 0.0.0 fresh-install sentinel is the updater's own convention for comparing
// versions, not a release the API should record as installed. A missing
// header leaves the inventory's stored value untouched.
func (u *Updater) emlyVersion() string {
	info := u.Cfg.ResolveEMLy()
	if info.FreshInstall {
		return ""
	}
	return info.InstalledVersion
}

// loggedUser resolves the interactive user and their session state for the
// X-EMLy-LoggedUser* headers, through the seam the tests pin.
func (u *Updater) loggedUser() machineinfo.UserSession {
	if u.loggedUserFn != nil {
		return u.loggedUserFn()
	}
	return machineinfo.LoggedUser()
}

// newResolver builds the source resolver for this cycle from the server
// chain the policy assigns to this machine: the site's base server as the
// primary (with retries and backoff) and its backups as one-shot fallbacks,
// in the order the document lists them. A machine at no site gets the single
// defaultServer with no fallback.
//
// Unlike the legacy behaviour the public API is not appended implicitly: a
// site that wants the cloud as a last resort lists it in backupServer. The
// policy derived from config.ini does list it, so nothing changes for a
// machine that has never received a document.
//
// EMLy's manifest and the updater's own both go through this, so a machine
// that can only reach its site's mirror behaves the same way for both.
//
// A backup this session already found reachable when the head was not leads
// the chain instead (see preferredChain), so an unreachable base server costs
// its retries once per service session rather than once per cycle.
func (u *Updater) newResolver(cyc *cycleState) *source.Resolver {
	chain := u.preferredChain(cyc)
	if len(chain) > 0 && len(cyc.chain) > 0 && chain[0] != cyc.chain[0] {
		u.Log.Debug("trying this session's preferred server ahead of the policy order",
			"preferred", chain[0], "policyHead", cyc.chain[0])
	}
	urls := make([]string, 0, len(chain))
	for _, name := range chain {
		if url := cyc.eff.ManifestURL(name); url != "" {
			urls = append(urls, url)
		}
	}
	if len(urls) == 0 {
		// Validation guarantees every referenced server exists, so this can
		// only happen on a document whose servers map is empty for this
		// machine - keep a resolver that fails cleanly rather than panicking.
		urls = append(urls, "")
	}

	settings := cyc.eff.Doc.Updater.Resolver
	resolver := &source.Resolver{
		Primary:     u.newHTTPSource(urls[0]),
		Attempts:    settings.Attempts,
		BaseBackoff: settings.BaseBackoff(),
		Logf: func(format string, args ...any) {
			u.Log.Info(fmt.Sprintf(format, args...))
		},
	}
	for _, url := range urls[1:] {
		resolver.Fallbacks = append(resolver.Fallbacks, u.newHTTPSource(url))
	}
	return resolver
}

// resolveTarget fetches the update manifest from this machine's server chain
// and resolves it to a channel target. Shared by the normal poll in Cycle and
// by the forced re-download path in install. A successful resolution updates
// the preferred server for the rest of this session (see resolveTargetWith).
func (u *Updater) resolveTarget(ctx context.Context, cyc *cycleState, channel string) (source.Source, *manifest.Manifest, manifest.Target, error) {
	return u.resolveTargetWith(ctx, cyc, channel, true)
}

// resolveTargetWith is resolveTarget with control over whether a successful
// resolution is allowed to change the preferred server for the rest of this
// session. notePreferred=false is for read-only diagnostics - the
// client-channel emly.manifest.check dry run (clientcmd.go) - which must
// observe the same chain evaluation as a real cycle without the side effect
// of pinning the machine to a backup server or waking the presence
// supervisor (wakeClientWS) as a consequence of a status check.
func (u *Updater) resolveTargetWith(ctx context.Context, cyc *cycleState, channel string, notePreferred bool) (source.Source, *manifest.Manifest, manifest.Target, error) {
	resolver := u.newResolver(cyc)
	src, m, err := resolver.Resolve(ctx)
	if err != nil {
		return nil, nil, manifest.Target{}, err
	}
	if notePreferred {
		u.notePreferredServer(cyc, resolver, src)
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
func (u *Updater) apply(ctx context.Context, cyc *cycleState, p *state.Pending, emly config.EMLyInfo) error {
	// Coarse check, same reasoning as Cycle's own: apply is reached after
	// resolveTarget/download, which can take a while, so a destructive
	// command could have been committed since Cycle's own top-level check.
	// install() (below, via beginInstall) is still the authoritative,
	// race-safe checkpoint for the non-forced path - but the forced path
	// kills EMLy before ever reaching install(), so it gets its own check
	// too, right before the kill (see below): a user closing EMLy because
	// of an unrelated reboot warning must not have the forced kill run
	// anyway for an install that is about to be refused.
	if u.destructivePendingNow() {
		u.logDestructiveSkipOnce()
		return nil
	}

	exe := u.Cfg.EMLyExeName

	if process.IsRunning(exe) {
		if p.Forced {
			warning := cyc.eff.Doc.Updater.CriticalWarning
			if warning.Enabled {
				seconds := warning.Seconds
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
			// Re-checked here, not just at the top of apply: the warning
			// countdown above can run for cyc.eff.Doc.Updater.CriticalWarning
			// .Seconds (default 30s) of real time, long enough for a
			// destructive command to be admitted and committed while EMLy is
			// still running and untouched. Killing it now would be for
			// nothing - install() is about to refuse anyway.
			if u.destructivePendingNow() {
				u.logDestructiveSkipOnce()
				return nil
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

	return u.install(ctx, cyc, p, emly)
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
func (u *Updater) install(ctx context.Context, cyc *cycleState, p *state.Pending, emly config.EMLyInfo) error {
	// Claims installing for the whole of this function - both setup runs,
	// the forced redownload and the uninstall/reinstall clean-retry, not
	// just the setup execution itself - so a destructive client command
	// cannot be admitted mid-uninstall. This is also the checkpoint that
	// stops a WaitForExit-released install (apply, above) from starting:
	// the client channel may have accepted a reboot/restart while EMLy was
	// still running and this cycle was waiting on it.
	if !u.beginInstall("EMLy install") {
		return fmt.Errorf("EMLy install skipped: a destructive client command is pending")
	}
	defer u.endInstall()

	// from is the version installed before this attempt, omitted (empty) on
	// a fresh install (spec §8.3): the 0.0.0 sentinel is an internal
	// comparison value, not a version to report.
	var from string
	if !emly.FreshInstall {
		from = emly.InstalledVersion
	}

	// Final integrity gate immediately before execution.
	if err := download.VerifyFile(p.SetupPath, p.SHA256); err != nil {
		// Corrupt cache: drop it so the next cycle re-downloads cleanly.
		_ = os.Remove(p.SetupPath)
		_ = u.Store.ClearPending()
		u.emitUpdateFailed(updateEvent{Target: "emly", FromVersion: from, ToVersion: p.Version,
			WillRetry: true, Error: &wsclient.ErrorBody{Code: "checksum_mismatch", Message: err.Error()}})
		return fmt.Errorf("refusing to install: %w", err)
	}

	started := u.clock()
	u.emit(wsclient.EvtUpdateStarted, updateEvent{Target: "emly", FromVersion: from, ToVersion: p.Version,
		Forced: p.Forced, Attempt: 1, Trigger: u.cycleTrigger})

	reinstalled := false
	if err := u.runSetupAndVerify(p, "running setup"); err != nil {
		u.Log.WarnEvent(logging.EventInstallFailed,
			"EMLy did not reach the target version, forcing a clean reinstall over the existing state",
			"version", p.Version, "error", err.Error())
		reinstalled = true

		if fresh, ferr := u.forceRedownload(ctx, cyc, p, emly.Channel); ferr != nil {
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
			u.emitUpdateFailed(updateEvent{Target: "emly", FromVersion: from, ToVersion: p.Version,
				Attempt: 2, WillRetry: true, Error: &wsclient.ErrorBody{Code: installFailureCode(err), Message: err.Error()}})
			return err // pending kept → retried next cycle
		}
	}

	u.Log.InfoEvent(logging.EventInstallOK, "EMLy updated successfully", "version", p.Version)

	attempt := 1
	if reinstalled {
		attempt = 2
	}
	u.emit(wsclient.EvtUpdateApplied, updateEvent{Target: "emly", FromVersion: from, ToVersion: p.Version,
		Forced: p.Forced, Attempt: attempt, DurationMS: u.clock().Sub(started).Milliseconds(), Reinstalled: reinstalled})

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
func (u *Updater) forceRedownload(ctx context.Context, cyc *cycleState, p *state.Pending, channel string) (*state.Pending, error) {
	if err := u.Downloads.CleanupExcept(""); err != nil {
		u.Log.Warn("failed to fully clear the downloads cache before forcing a re-download",
			"error", err.Error())
	}
	if err := u.Store.ClearPending(); err != nil {
		u.Log.Warn("failed to clear state.json before forcing a re-download", "error", err.Error())
	}

	src, _, target, err := u.resolveTarget(ctx, cyc, channel)
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
// installing is claimed by the caller (install, above) for its whole
// duration - both attempts, plus the redownload/uninstall between them -
// not by this function per call.
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
func (u *Updater) ensureCertificate(cyc *cycleState) {
	if !cyc.eff.Doc.Updater.InstallCertificate.Enabled {
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

// notifySourcesUnreachable warns the console user that this poll cycle could
// not reach any update source (primary retries exhausted, and the fallback -
// when wired in - failed too), pointing them at their IT department. It is
// logged (event 101) every time regardless, but the toast itself is shown at
// most once per outage: sourcesUnreachableNotified is only set once the toast
// actually launches, and Cycle clears it as soon as resolveTarget next
// succeeds. Never fatal - a missing console session just means nobody was
// there to see it, and the next cycle tries again.
func (u *Updater) notifySourcesUnreachable() {
	u.Log.WarnEvent(logging.EventSourcesUnreachable,
		"no update source reachable this cycle", "alreadyNotified", u.sourcesUnreachableNotified)

	if u.sourcesUnreachableNotified {
		return
	}

	lang := u.Cfg.ResolveEMLy().Language
	msg := notify.SourcesUnreachableMessage(lang)

	self, err := os.Executable()
	if err != nil {
		u.Log.Warn("failed to resolve own executable path, skipping unreachable-source toast", "error", err.Error())
		return
	}

	emlyExe := assoc.ExePath(u.Cfg.EMLyInstallDir, u.Cfg.EMLyExeName)
	if notify.LaunchToast(self, emlyExe, msg.Title, msg.Body) {
		u.Log.Info("update-source-unreachable toast shown")
		u.sourcesUnreachableNotified = true
	} else {
		u.Log.Info("update-source-unreachable toast skipped (no active console session)")
	}
}

// Handler adapts Updater to the SCM. svc.Run blocks until Execute returns.
type Handler struct {
	Updater *Updater
}

// Execute implements svc.Handler: it reports Running, drives the update loop
// in a goroutine, and translates Stop/Shutdown into context cancellation.
// Session notifications (console/RDP connect, disconnect, logon, ...) are
// handed to watchSessions.
func (h *Handler) Execute(_ []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown | svc.AcceptSessionChange

	changes <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(3)
	go func() {
		defer wg.Done()
		h.Updater.RunLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		h.Updater.watchSessions(ctx)
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
		case svc.SessionChange:
			// Parsed right here: EventData points into memory the SCM only
			// lends for the duration of its control handler call.
			if ev, ok := machineinfo.ParseSessionChange(c.EventType, c.EventData, time.Now()); ok {
				h.Updater.queueSessionChange(ev)
			}
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
