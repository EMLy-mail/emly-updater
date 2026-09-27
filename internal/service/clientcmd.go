package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"emlyupdater/internal/logging"
	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/manifest"
	"emlyupdater/internal/notify"
	"emlyupdater/internal/selfupdate"
	"emlyupdater/internal/source"
	"emlyupdater/internal/state"
	"emlyupdater/internal/version"
	"emlyupdater/internal/winget"
	"emlyupdater/internal/wsclient"
)

const (
	commandRingSize = 64
	// maxClockSkew is how far the server's ts and this clock may disagree
	// for expires_at to be judged at all (spec §5.1).
	maxClockSkew = 5 * time.Minute
	// resultPayloadBudget leaves room for the envelope under 64 KiB.
	resultPayloadBudget = 60000
	wingetTimeout       = 150 * time.Second
	checkTimeout        = 50 * time.Second
	// maxUserScopeError bounds user_scope_error, which can carry a whole
	// PowerShell stderr, so it cannot eat the packages' share of the frame.
	maxUserScopeError = 1000
)

type commandSession interface {
	Secure() bool
	Ack(ctx context.Context, replyTo string, a wsclient.Ack) error
	Result(ctx context.Context, replyTo string, r wsclient.Result) error
	Close(code websocket.StatusCode, reason string)
}

// commandRing remembers the last commandRingSize command IDs and their
// result, so a redelivered command is confirmed, not re-run (spec §5.5).
//
// refusals holds the Ack a redelivered id must be replayed with when the
// original delivery was refused (policy, transport, validation, busy):
// without it, a redelivery of a refused command falls back to the
// accepted+duplicate Ack results holds for a command that actually ran -
// telling a redelivered refusal it was accepted.
type commandRing struct {
	mu       sync.Mutex
	order    []string
	results  map[string]*wsclient.Result
	refusals map[string]*wsclient.ErrorBody
}

func (r *commandRing) add(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.results == nil {
		r.results = map[string]*wsclient.Result{}
	}
	r.order = append(r.order, id)
	r.results[id] = nil
	if len(r.order) > commandRingSize {
		delete(r.results, r.order[0])
		delete(r.refusals, r.order[0])
		r.order = r.order[1:]
	}
}

func (r *commandRing) lookup(id string) (*wsclient.Result, *wsclient.ErrorBody, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	res, ok := r.results[id]
	return res, r.refusals[id], ok
}

func (r *commandRing) store(id string, res wsclient.Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.results[id]; ok {
		r.results[id] = &res
	}
}

// refuse remembers the Ack a refused id's redelivery must be replayed with.
func (r *commandRing) refuse(id string, e *wsclient.ErrorBody) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refusals == nil {
		r.refusals = map[string]*wsclient.ErrorBody{}
	}
	r.refusals[id] = e
}

func refuse(code, msg string) *wsclient.ErrorBody {
	return &wsclient.ErrorBody{Code: code, Message: msg}
}

// commandRefusalEventID returns the Event Log id a command refusal should
// mirror to, or 0 for none. Only a destructive command's refusal reaches
// Event Viewer (925): a read-only command refused by policy, an unsupported
// verb, or a busy winget call is common enough (a stale policy document, a
// second machine.info while the first is still in flight) that mirroring
// every one would fill the log with noise that carries no operational
// weight - a refused service.restart/machine.reboot does.
func commandRefusalEventID(name string) uint32 {
	if wsclient.Destructive(name) {
		return logging.EventClientCommandRefused
	}
	return 0
}

// logCommandRefused logs a command refusal, every time, to the file -
// mirroring to the Event Log only for a destructive command (see
// commandRefusalEventID). Used for both admitCommand's refusal and the
// per-name busy refusal (claimCommand), which previously logged nothing at
// all.
func (u *Updater) logCommandRefused(cmd wsclient.Command, msg wsclient.Message, e *wsclient.ErrorBody) {
	kv := []any{"name", cmd.Name, "id", msg.ID, "issuedBy", cmd.IssuedBy, "code", e.Code, "reason", e.Message}
	if id := commandRefusalEventID(cmd.Name); id != 0 {
		u.Log.WarnEvent(id, "client command refused", kv...)
		return
	}
	u.Log.Warn("client command refused", kv...)
}

// seedCommandRing loads the pending command ids left in state.json - a
// service.restart or machine.reboot this process's predecessor accepted
// and persisted right before it stopped or rebooted - and marks them
// already-seen in the dedupe ring, before the first client-channel
// connection is even attempted. Without this, a server re-sending the same
// command id after the reconnect (exactly what it is expected to do until
// service.started confirms it) would be re-executed instead of recognised
// as a duplicate.
//
// Uses Load, not Take: cachedServiceStarted/buildServiceStarted is what
// consumes (and clears) these same records, on a different goroutine once
// the connection is up and the welcome burst runs. Seeding must not race it
// for ownership - it only reads.
func (u *Updater) seedCommandRing() {
	st, err := u.Store.Load()
	if err != nil {
		u.Log.Warn("could not read state.json to seed the command dedupe ring", "error", err.Error())
		return
	}
	for _, c := range st.PendingCommands {
		u.commands.add(c.ID)
	}
}

// executeCommand runs one command end to end: admission, ack, execution,
// result. It is called on its own goroutine per command by wsclient.
func (u *Updater) executeCommand(ctx context.Context, s commandSession, msg wsclient.Message, cmd wsclient.Command) {
	defer u.recoverGoroutine("executeCommand", "name", cmd.Name, "id", msg.ID)

	if msg.ID == "" {
		// Nothing to dedupe or correlate an Ack/Result against - the ring is
		// keyed on it, and a bare "" would make every other id-less command
		// collide with it. Refuse instead of ever touching the ring.
		e := refuse(wsclient.ErrInvalidArgs, "command id is required")
		u.logCommandRefused(cmd, msg, e)
		_ = s.Ack(ctx, msg.ID, wsclient.Ack{Accepted: false, Error: e})
		return
	}

	if prev, refusal, seen := u.commands.lookup(msg.ID); seen {
		if refusal != nil {
			// The original delivery was refused: replay that refusal, not
			// the generic accepted+duplicate Ack below - otherwise a
			// redelivered refusal reads back as an acceptance.
			_ = s.Ack(ctx, msg.ID, wsclient.Ack{Accepted: false, Duplicate: true, Error: refusal})
			return
		}
		_ = s.Ack(ctx, msg.ID, wsclient.Ack{Accepted: true, Duplicate: true})
		if prev != nil {
			_ = s.Result(ctx, msg.ID, *prev)
		}
		return
	}
	u.commands.add(msg.ID)

	if e := u.admitCommand(s, msg, cmd); e != nil {
		u.commands.refuse(msg.ID, e)
		u.logCommandRefused(cmd, msg, e)
		_ = s.Ack(ctx, msg.ID, wsclient.Ack{Accepted: false, Error: e})
		return
	}
	if !u.claimCommand(cmd.Name) {
		e := refuse(wsclient.ErrBusy, cmd.Name+" is already running")
		u.commands.refuse(msg.ID, e)
		u.logCommandRefused(cmd, msg, e)
		_ = s.Ack(ctx, msg.ID, wsclient.Ack{Accepted: false, Error: e})
		return
	}
	defer u.releaseCommand(cmd.Name)

	if err := s.Ack(ctx, msg.ID, wsclient.Ack{Accepted: true}); err != nil {
		return // connection gone: the server will time the command out
	}
	if wsclient.Destructive(cmd.Name) {
		u.Log.WarnEvent(logging.EventClientCommand, "client command accepted",
			"name", cmd.Name, "id", msg.ID, "issuedBy", cmd.IssuedBy)
		u.runDestructive(ctx, s, msg, cmd) // Task 8
		return
	}
	u.Log.Info("client command accepted", "name", cmd.Name, "id", msg.ID, "issuedBy", cmd.IssuedBy)

	started := u.clock()
	payload, e, truncated := u.runReadOnly(ctx, cmd)
	res := wsclient.Result{Status: wsclient.ResultOK, DurationMS: u.clock().Sub(started).Milliseconds(), Truncated: truncated}
	if e != nil {
		res.Status, res.Error = wsclient.ResultError, e
		u.Log.Warn("client command failed", "name", cmd.Name, "id", msg.ID, "code", e.Code, "reason", e.Message)
	} else if raw, err := json.Marshal(payload); err != nil {
		res.Status, res.Error = wsclient.ResultError, refuse(wsclient.ErrInternal, err.Error())
	} else {
		res.Payload = raw
	}
	u.commands.store(msg.ID, res)
	_ = s.Result(ctx, msg.ID, res)
}

func (u *Updater) admitCommand(s commandSession, msg wsclient.Message, cmd wsclient.Command) *wsclient.ErrorBody {
	if e := wsclient.ValidateArgs(cmd.Name, cmd.Args); e != nil {
		return e
	}
	now := u.clock()
	if exp, err := time.Parse(time.RFC3339, cmd.ExpiresAt); err == nil && now.After(exp) {
		if ts, err := time.Parse(time.RFC3339, msg.TS); err == nil {
			if skew := now.Sub(ts); skew < maxClockSkew && skew > -maxClockSkew {
				return refuse(wsclient.ErrExpired, "command expired at "+cmd.ExpiresAt)
			}
		}
	}
	cyc := u.cur.Load()
	if cyc == nil || !cyc.eff.Doc.ClientWS.Allows(cmd.Name) {
		return refuse(wsclient.ErrDisabledByPolicy, cmd.Name+" is not enabled for this host (clientWs.commands)")
	}
	if wsclient.Destructive(cmd.Name) && !s.Secure() {
		return refuse(wsclient.ErrInsecureTransport, cmd.Name+" requires a wss:// connection")
	}
	return u.admitDestructive(cmd) // Task 8; nil for everything else
}

func (u *Updater) claimCommand(name string) bool {
	u.runningMu.Lock()
	defer u.runningMu.Unlock()
	if u.running == nil {
		u.running = map[string]bool{}
	}
	if u.running[name] {
		return false
	}
	u.running[name] = true
	return true
}

func (u *Updater) releaseCommand(name string) {
	u.runningMu.Lock()
	delete(u.running, name)
	u.runningMu.Unlock()
}

func (u *Updater) runReadOnly(ctx context.Context, cmd wsclient.Command) (any, *wsclient.ErrorBody, bool) {
	switch cmd.Name {
	case wsclient.CmdMachineInfo:
		var a wsclient.MachineInfoArgs
		_ = wsclient.DecodeArgs(cmd.Args, &a)
		return u.machineInfo(a.Sections), nil, false
	case wsclient.CmdEMLyManifestCheck:
		cctx, cancel := context.WithTimeout(ctx, checkTimeout)
		defer cancel()
		return u.emlyManifestCheck(cctx, u.cur.Load()), nil, false
	case wsclient.CmdUpdaterManifestCheck:
		cctx, cancel := context.WithTimeout(ctx, checkTimeout)
		defer cancel()
		return u.updaterManifestCheck(cctx, u.cur.Load()), nil, false
	case wsclient.CmdAppsListUpgradable:
		return u.listUpgradable(ctx)
	}
	return nil, refuse(wsclient.ErrUnsupportedCommand, cmd.Name), false
}

// listUpgradable runs winget (Microsoft.WinGet.Client) twice, in parallel -
// as LocalSystem and as the logged-on user in their own session - merges the
// two lists and fits the result under the frame budget, dropping packages
// from the end.
//
// Both halves are needed because neither sees everything. LocalSystem sees
// what is installed for every user (HKLM, provisioned MSIX) but nothing a
// user installed for themselves alone (HKCU, per-user MSIX); the user's
// session sees their own packages. The session asked is the one
// machineinfo.InteractiveSession picks - the console user when someone is at
// the machine, an RDP user otherwise - i.e. the user X-EMLy-LoggedUser names.
//
// The machine half decides the outcome: its failure fails the command, as it
// always did. The user half is best-effort - nobody logged on is the normal
// state of a machine at the lock screen - and its outcome travels in
// user_scope/user_scope_error, so an absent user list is never mistaken for
// "that user has nothing to update".
func (u *Updater) listUpgradable(ctx context.Context) (any, *wsclient.ErrorBody, bool) {
	ctx, cancel := context.WithTimeout(ctx, wingetTimeout)
	defer cancel()
	list := winget.ListUpgradable
	if u.listUpgradableFn != nil {
		list = u.listUpgradableFn
	}
	listUser := listUserUpgradable
	if u.listUserUpgradableFn != nil {
		listUser = u.listUserUpgradableFn
	}

	var (
		userRes userUpgradable
		userErr error
		user    sync.WaitGroup
	)
	user.Go(func() { userRes, userErr = listUser(ctx) })
	pkgs, err := list(ctx)
	user.Wait()

	switch {
	case errors.Is(err, winget.ErrModuleNotInstalled):
		return nil, refuse(wsclient.ErrWingetModuleMissing, err.Error()), false
	case errors.Is(err, winget.ErrPowerShellNotFound), errors.Is(err, winget.ErrPowerShell7Required):
		return nil, refuse(wsclient.ErrPowerShellNotFound, err.Error()), false
	case err != nil && ctx.Err() != nil:
		return nil, refuse(wsclient.ErrTimeout, "winget did not answer in time"), false
	case err != nil:
		return nil, refuse(wsclient.ErrInternal, err.Error()), false
	}
	type payload struct {
		Packages []wsclient.UpgradablePackage `json:"packages"`
		// UserScope: the logged-on user's own packages are included.
		UserScope bool `json:"user_scope"`
		// User is the account whose session was asked, when there was one.
		User string `json:"user,omitempty"`
		// UserScopeError says why UserScope is false.
		UserScopeError string `json:"user_scope_error,omitempty"`
		CollectedAt    string `json:"collected_at"`
	}
	p := payload{
		Packages:    mergeUpgradable(pkgs, userRes.Pkgs),
		User:        userRes.User,
		CollectedAt: u.clock().UTC().Format(time.RFC3339),
	}
	switch {
	case userErr == nil:
		p.UserScope = true
	case errors.Is(userErr, errNoInteractiveUser):
		p.UserScopeError = userErr.Error()
	default:
		if ctx.Err() != nil {
			userErr = errors.New("winget did not answer in time in the user's session")
		}
		p.UserScopeError = userErr.Error()
		if len(p.UserScopeError) > maxUserScopeError {
			p.UserScopeError = p.UserScopeError[:maxUserScopeError] + "..."
		}
		u.Log.Warn("apps.list_upgradable: could not list the logged-on user's packages, returning the machine's only",
			"user", userRes.User, "error", userErr.Error())
	}
	truncated := false
	for {
		b, _ := json.Marshal(p)
		if len(b) <= resultPayloadBudget || len(p.Packages) == 0 {
			break
		}
		cut := len(p.Packages) / 10
		if cut == 0 {
			cut = 1
		}
		p.Packages, truncated = p.Packages[:len(p.Packages)-cut], true
	}
	return p, nil, truncated
}

// userUpgradable is the logged-on user's half of apps.list_upgradable.
type userUpgradable struct {
	// User is DOMAIN\user of the session asked, set even when Pkgs could
	// not be listed.
	User string
	Pkgs []winget.Package
}

// errNoInteractiveUser: nobody is logged on, so there is no user half.
var errNoInteractiveUser = errors.New("no user is logged on")

// listUserUpgradable runs the winget query inside the interactive user's
// session, as that user (see listUpgradable).
func listUserUpgradable(ctx context.Context) (userUpgradable, error) {
	id, us, ok := machineinfo.InteractiveSession()
	if !ok {
		return userUpgradable{}, errNoInteractiveUser
	}
	pkgs, err := winget.ListUpgradableWith(ctx, sessionRunner(id))
	return userUpgradable{User: us.User, Pkgs: pkgs}, err
}

// sessionRunner is a winget.Runner that starts PowerShell as the user of
// session sessionID, with the same command line winget.PowerShell uses.
func sessionRunner(sessionID uint32) winget.Runner {
	return func(ctx context.Context, script string) ([]byte, error) {
		out, err := notify.RunInSession(ctx, sessionID, winget.Command(script))
		if err != nil {
			return nil, err
		}
		if out.ExitCode != 0 {
			return out.Stdout, &winget.ExitError{Code: out.ExitCode, Stderr: string(out.Stderr)}
		}
		return out.Stdout, nil
	}
}

// mergeUpgradable combines the two halves of apps.list_upgradable: every
// package LocalSystem saw, as ScopeMachine, then those only the user's
// session saw, as ScopeUser. The user's session also sees machine-wide
// packages, so a package is the same one when its id (case-insensitively)
// and installed version match - the version is part of the key because two
// side-by-side installs of one id (two JDKs, two Python launchers) are two
// packages to update.
func mergeUpgradable(machine, user []winget.Package) []wsclient.UpgradablePackage {
	out := make([]wsclient.UpgradablePackage, 0, len(machine)+len(user))
	seen := make(map[string]bool, len(machine)+len(user))
	add := func(k winget.Package, scope string) {
		key := strings.ToLower(k.ID) + "\x00" + k.InstalledVersion
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, wsclient.UpgradablePackage{Name: k.Name, ID: k.ID,
			InstalledVersion: k.InstalledVersion, AvailableVersion: k.Available, Source: k.Source, Scope: scope})
	}
	for _, k := range machine {
		add(k, wsclient.ScopeMachine)
	}
	for _, k := range user {
		add(k, wsclient.ScopeUser)
	}
	return out
}

// emlyManifestCheck is the dry run of spec §7.2: the same manifest
// resolution as a Cycle, stopping before any download. It is read-only
// (no state.json write, no download, no RunLoop wake-up, and - via
// resolveTargetWith's notePreferred=false - no change to the preferred
// server or the presence supervisor either), which is what makes it safe to
// run while a Cycle is in progress.
func (u *Updater) emlyManifestCheck(ctx context.Context, cyc *cycleState) wsclient.ManifestCheck {
	if u.emlyCheckFn != nil {
		return u.emlyCheckFn(ctx, cyc)
	}
	emly := u.Cfg.ResolveEMLyWithChannel(cyc.eff.Doc.Updater.Channel())
	mc := wsclient.ManifestCheck{Target: "emly", Channel: emly.Channel, CheckedAt: u.clock().UTC().Format(time.RFC3339)}
	if !emly.FreshInstall {
		mc.InstalledVersion = emly.InstalledVersion
	}
	if st, err := u.Store.Load(); err == nil && st.Pending != nil {
		mc.Pending = &wsclient.PendingInfo{Version: st.Pending.Version, Forced: st.Pending.Forced,
			DownloadedAt: st.Pending.DownloadedAt.UTC().Format(time.RFC3339)}
	}
	src, m, target, err := u.resolveTargetWith(ctx, cyc, emly.Channel, false)
	if err != nil {
		mc.Decision = "up_to_date"
		mc.Error = manifestError(err)
		return mc
	}
	mc.AvailableVersion, mc.MinRequiredVersion = target.Version, m.MinRequiredVersion
	mc.Source = u.serverRef(cyc, sourceURL(src))
	newer, _ := manifest.Less(emly.InstalledVersion, target.Version)
	forced, _ := m.Forced(emly.InstalledVersion)
	mc.UpdateAvailable, mc.Critical = newer, forced
	enabled, _ := cyc.eff.UpdaterEnabled(u.clock())
	mc.Decision = emlyDecision(newer, forced, u.emlyRunning(), !enabled)
	return mc
}

func emlyDecision(newer, forced, running, paused bool) string {
	switch {
	case !newer:
		return "up_to_date"
	case paused:
		return "paused"
	case forced:
		return "forced"
	case running:
		return "waiting_for_emly_exit"
	default:
		return "install_next_cycle"
	}
}

// updaterManifestCheck is the same dry run for the updater itself (§7.3):
// selfupdate.Decide is evaluated on the raw record - not reconciled, which
// would log and clear it - and nothing is launched or counted. Like
// emlyManifestCheck, it resolves via resolveUpdaterManifestWith's
// notePreferred=false, so a status check cannot pin the machine to a backup
// server or wake the presence supervisor either.
func (u *Updater) updaterManifestCheck(ctx context.Context, cyc *cycleState) wsclient.ManifestCheck {
	if u.updaterCheckFn != nil {
		return u.updaterCheckFn(ctx, cyc)
	}
	running := version.Version
	mc := wsclient.ManifestCheck{Target: "updater", InstalledVersion: running, CheckedAt: u.clock().UTC().Format(time.RFC3339)}
	if !cyc.eff.Doc.Updater.SelfUpdate.Enabled {
		mc.Decision = "disabled"
		return mc
	}
	_, m, servedBy, err := u.resolveUpdaterManifestWith(ctx, cyc, false)
	if err != nil {
		mc.Decision = "up_to_date"
		mc.Error = manifestError(err)
		return mc
	}
	mc.AvailableVersion = m.Version
	mc.Source = u.serverRef(cyc, servedBy)
	newer, _ := manifest.Less(running, m.Version)
	mc.UpdateAvailable = newer
	var rec *state.SelfUpdate
	if st, err := u.Store.Load(); err == nil {
		rec = st.SelfUpdate
	}
	d, _ := selfupdate.Decide(running, m, rec, u.clock())
	switch {
	case !newer:
		mc.Decision = "up_to_date"
	case d.GiveUp || (rec != nil && rec.Version == m.Version && rec.GaveUp):
		mc.Decision = "gave_up"
	case d.Install:
		mc.Decision = "install_next_cycle"
	case rec != nil && rec.Version == m.Version:
		mc.Decision = "cooldown"
	default:
		mc.Decision = "up_to_date"
	}
	return mc
}

func manifestError(err error) *wsclient.ErrorBody {
	switch {
	case errors.Is(err, source.ErrNotFound):
		return refuse(wsclient.ErrNotFound, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return refuse(wsclient.ErrTimeout, err.Error())
	default:
		return refuse(wsclient.ErrSourcesUnreachable, err.Error())
	}
}

// sourceURL is the manifest URL a Source was built from.
func sourceURL(s source.Source) string {
	if h, ok := s.(*source.HTTPSource); ok {
		return h.ManifestURL
	}
	return s.Name()
}
