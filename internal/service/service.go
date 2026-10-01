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
	"emlyupdater/internal/product"
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

	// progress is the progress window of the EMLy update in flight, nil
	// outside one or when [progressWindow] is disabled. Like the field above,
	// it belongs to Cycle's single goroutine. Cycle sets it and closes it on
	// the way out; apply, install and forceRedownload drive it.
	progress *progressUI

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

	// runningFn/driverFn/notifyBoxFn/toastFn are the product engine's seams
	// (product.go): tests fake the running check, the setup, the user
	// notification and the update-complete toast of products other than
	// EMLy. nil means process.IsRunning, installer.For, notify.SendNotifyBox
	// and notify.LaunchToast.
	runningFn   func(exe string) bool
	driverFn    func(*product.Product) installer.Driver
	notifyBoxFn func(notify.Message, int) bool
	toastFn     func(icon, title, body string) bool
	// waitExitFn overrides process.WaitForExitUnder for watchProductExit.
	waitExitFn func(ctx context.Context, p *product.Product) error

	// exitWatchers holds the slugs whose watchProductExit goroutine is
	// running, so a cycle that defers the same product again does not start
	// a second one. The goroutine removes its own slug, hence the lock.
	exitWatchersMu sync.Mutex
	exitWatchers   map[string]bool

	// productDownloads caches one download.Manager per product other than
	// EMLy (downloadsFor). detectWarned and waitNotified keep the per-product
	// "unknown version" and "close the app" messages to one per session and
	// one per version. Poll goroutine only, like announced: no locking.
	productDownloads map[string]*download.Manager
	detectWarned     map[string]bool
	waitNotified     map[string]string
	// unavailableUntil backs off products whose manifest had nothing
	// (markUnavailable). Poll goroutine only.
	unavailableUntil map[string]time.Time
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
	// listUserUpgradableFn overrides listUserUpgradable (the logged-on
	// user's half of apps.list_upgradable) in tests.
	listUserUpgradableFn func(context.Context) (userUpgradable, error)
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

	// notifyWakeMu guards lastNotifyWake: handleNotify runs on the presence
	// connection's own read goroutine (see wsclient.Handler's doc comment),
	// which - unlike announced/cycleTrigger above - is not the poll
	// goroutine, so this one does need locking.
	notifyWakeMu sync.Mutex
	// lastNotifyWake is when a notify last scheduled an early wake (see
	// allowNotifyWake, clientnotify.go): at most one per
	// notifyWakeThrottle, so a burst of release.published/config.published
	// pushes cannot collapse into a stampede of early cycles. Zero means
	// none yet this process.
	lastNotifyWake time.Time
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
	// One Pacer for both download managers: the server's download slots are
	// a single pool shared by EMLy's setups and the updater's own, so a 429
	// on one must hold back the other too.
	pacer := &download.Pacer{}
	u := &Updater{
		Cfg:       cfg,
		Log:       log,
		Store:     &state.Store{Path: config.StatePath()},
		Downloads: &download.Manager{Dir: config.DownloadsDir(), Pacer: pacer, Log: log},
		SelfDownloads: &download.Manager{
			Dir:    config.SelfDownloadsDir(),
			Prefix: "EMLyUpdater-",
			Pacer:  pacer,
			Log:    log,
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
	u.Store.OnCorrupt = func(backup string, err error) {
		log.Warn("state file did not parse; moved aside and rebuilt empty", "backup", backup, "err", err)
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
	if trigger == "" || trigger == productExitWake {
		// A product's app closing is a local event, not one of the
		// update.started triggers the server knows: it reads as a cycle.
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

	// One product at a time, EMLy last: never two downloads or two setups
	// at once on this machine (MPLS, and beginInstall).
	for _, p := range u.cycleProducts(cyc) {
		if u.destructivePendingNow() {
			u.logDestructiveSkipOnce()
			return nil
		}
		// Each product starts from the cycle's own trigger: a product that
		// resumed a pending entry ("resume") must not hand it to the next
		// product's update.started - EMLy's included.
		u.cycleTrigger = trigger
		if err := u.productCycle(ctx, cyc, p); err != nil {
			if p.Legacy {
				return err // EMLy is last: its error stays the cycle's, as before
			}
			u.Log.Warn("product update failed this cycle", "product", p.Slug, "error", err.Error())
		}
	}
	return nil
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
// X-EMLy-InstalledProducts comes from the same read, so the two cannot
// disagree about EMLy.
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
	httpSrc.EMLyVersion, httpSrc.InstalledProducts = u.installedProducts()
	session := u.loggedUser()
	httpSrc.LoggedUser = session.User
	httpSrc.LoggedUserState = string(session.State)
	httpSrc.LoggedUserDisconnectedAt = session.DisconnectedAt
	return httpSrc
}

// installedProducts reads what is installed on this machine, for the
// X-EMLy-AppVersion and X-EMLy-InstalledProducts headers.
//
// emlyVersion is "" when EMLy is not installed or cannot be read: the 0.0.0
// fresh-install sentinel is the updater's own convention for comparing
// versions, not a release the API should record as installed, and a missing
// header leaves the stored value untouched.
//
// inventory is the complete product list - EMLy plus every product of the
// document, enabled or not (enabled turns updates off, not dashboard visibility).
// It is nil - no header - when detection failed, and an empty non-nil map only
// when all products are positively absent: the API treats the inventory as
// authoritative and an empty one sent by mistake drops the machine from the
// dashboard. One unreadable product voids the whole inventory, which is complete
// by definition. EMLy is listed with the same GUI_SEMVER value
// X-EMLy-AppVersion carries, never as 0.0.0.
func (u *Updater) installedProducts() (emlyVersion string, inventory map[string]string) {
	version, err := u.Cfg.DetectEMLy()
	if err != nil {
		return "", nil
	}
	inventory = map[string]string{}
	if version != "" {
		inventory[ProductEMLy] = version
	}
	var cyc *cycleState
	if u.Policy != nil {
		cyc = u.current()
	}
	for _, p := range u.documentProducts(cyc, false) {
		r := product.Detect(p)
		switch r.Outcome {
		case product.Installed:
			inventory[p.Slug] = r.Version
		case product.Unknown:
			// The inventory is complete by definition: leaving this product
			// out would report it uninstalled. Send nothing instead.
			return version, nil
		}
	}
	return version, inventory
}

// ProductEMLy is EMLy's product slug, the one the API uses in
// /v2/updates/{slug}/... and in the installed-products inventory.
const ProductEMLy = "emly"

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

// apply is applyProduct for EMLy.
func (u *Updater) apply(ctx context.Context, cyc *cycleState, p *state.Pending, emly config.EMLyInfo) error {
	return u.applyProduct(ctx, cyc, u.emlyProduct(cyc), p, emlyState(emly))
}

// install is installProduct for EMLy.
func (u *Updater) install(ctx context.Context, cyc *cycleState, p *state.Pending, emly config.EMLyInfo) error {
	return u.installProduct(ctx, cyc, u.emlyProduct(cyc), p, emlyState(emly))
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

// showUpdateToast announces a completed update in the session of the user at
// the machine, at the console or over RDP. Best-effort and non-fatal: EMLy is
// already updated by this point, so a toast failure (no active user session,
// WTS/token errors, ...) is only
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
		u.Log.Info("update-complete toast skipped (no active user session, console or RDP)", "version", version)
	}
}

// notifySourcesUnreachable warns the user at the machine (console or RDP) that this poll cycle could
// not reach any update source (primary retries exhausted, and the fallback -
// when wired in - failed too), pointing them at their IT department. It is
// logged (event 101) every time regardless, but the toast itself is shown at
// most once per outage: sourcesUnreachableNotified is only set once the toast
// actually launches, and Cycle clears it as soon as resolveTarget next
// succeeds. Never fatal - no active user session just means nobody was
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
		u.Log.Info("update-source-unreachable toast skipped (no active user session, console or RDP)")
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
