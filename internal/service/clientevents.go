package service

import (
	"context"
	"slices"
	"time"

	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/process"
	"emlyupdater/internal/state"
	"emlyupdater/internal/version"
	"emlyupdater/internal/wsclient"
)

// eventBufferSize bounds the events kept while no v2 session is up. They
// are flushed after service.started on the next welcome; the oldest go
// first when it fills. In memory only: a service restart loses them, and the
// poll path still carries everything that matters (spec §2.3).
const eventBufferSize = 32

// bootWindow is how soon after boot a service start counts as "boot".
const bootWindow = 10 * time.Minute

type bufferedEvent struct {
	name    string
	payload any
}

// capabilities is what this build implements (spec §4): every command,
// event and topic of the wire list. Whether a command may run here is the
// policy's call, answered per command with disabled_by_policy.
func (u *Updater) capabilities() []string {
	return slices.Concat(wsclient.CommandNames, wsclient.EventNames, wsclient.TopicNames)
}

// emit sends an event on the current v2 session, or buffers it.
func (u *Updater) emit(name string, payload any) {
	if u.emitFn != nil {
		u.emitFn(name, payload)
		return
	}
	if s := u.wsSession.Load(); s != nil {
		if !s.Accepted(name) {
			u.Log.Debug("event not accepted by the server, dropped", "name", name)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.Event(ctx, name, payload); err == nil {
			return
		}
		// Connection going away: fall through and keep it for the next one.
	}
	u.eventsMu.Lock()
	defer u.eventsMu.Unlock()
	u.eventBuf = append(u.eventBuf, bufferedEvent{name, payload})
	if len(u.eventBuf) > eventBufferSize {
		u.eventBuf = u.eventBuf[len(u.eventBuf)-eventBufferSize:]
	}
}

// flushEvents hands the buffered events to send, oldest first, and empties
// the buffer. A failed send stops the flush and keeps the rest.
func (u *Updater) flushEvents(send func(name string, payload any) error) {
	u.eventsMu.Lock()
	buf := u.eventBuf
	u.eventBuf = nil
	u.eventsMu.Unlock()
	for i, ev := range buf {
		if err := send(ev.name, ev.payload); err != nil {
			u.eventsMu.Lock()
			u.eventBuf = append(buf[i:], u.eventBuf...)
			u.eventsMu.Unlock()
			return
		}
	}
}

// clientHandler is the service side of wsclient.Handler.
type clientHandler struct{ u *Updater }

func (h clientHandler) Welcome(s *wsclient.Session, _ wsclient.Welcome) {
	u := h.u
	u.wsSession.Store(s)
	send := func(name string, payload any) error {
		if !s.Accepted(name) {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.Event(ctx, name, payload)
	}
	if u.startedSent.CompareAndSwap(false, true) {
		if err := send(wsclient.EvtServiceStarted, u.serviceStarted()); err != nil {
			u.startedSent.Store(false)
		}
	}
	u.flushEvents(send)
	_ = send(wsclient.EvtMachineInfo, u.machineInfo(nil))
}

func (h clientHandler) Command(ctx context.Context, s *wsclient.Session, msg wsclient.Message, cmd wsclient.Command) {
	h.u.executeCommand(ctx, s, msg, cmd)
}

func (h clientHandler) Notify(_ *wsclient.Session, _ wsclient.Message, n wsclient.Notify) {
	h.u.handleNotify(n)
}

// serviceStarted builds service.started once per process (spec §8.6),
// consuming the pending restart/reboot command ids.
func (u *Updater) serviceStarted() wsclient.ServiceStarted {
	now := u.clock()
	facts := u.systemFacts(now)
	cmds, err := u.Store.TakePendingCommands()
	if err != nil {
		u.Log.Warn("could not read the pending command ids", "error", err.Error())
	}
	landed := u.selfLanded.Load()
	p := wsclient.ServiceStarted{
		Reason:         serviceStartedReason(cmds, landed, facts.BootTime, u.startedAt),
		StartedAt:      u.startedAt.UTC().Format(time.RFC3339),
		UpdaterVersion: version.Version,
	}
	if !facts.BootTime.IsZero() {
		p.BootTime = facts.BootTime.Format(time.RFC3339)
	}
	if landed != nil && landed.FromVersion != version.Version {
		p.PreviousVersion = landed.FromVersion
	}
	for _, c := range cmds {
		p.CompletedCommands = append(p.CompletedCommands, c.ID)
	}
	return p
}

func serviceStartedReason(cmds []state.PendingCommand, landed *state.SelfUpdate, boot, started time.Time) string {
	for _, c := range cmds {
		if c.Name == wsclient.CmdMachineReboot {
			return "boot"
		}
	}
	for _, c := range cmds {
		if c.Name == wsclient.CmdServiceRestart {
			return "command"
		}
	}
	switch {
	case landed != nil:
		return "self_update"
	case !boot.IsZero() && started.Sub(boot) < bootWindow:
		return "boot"
	default:
		return "unknown"
	}
}

func (u *Updater) systemFacts(now time.Time) machineinfo.SystemFacts {
	if u.systemFactsFn != nil {
		return u.systemFactsFn(now)
	}
	return machineinfo.CollectSystemFacts(now)
}

func sessionChangedPayload(kinds []machineinfo.SessionChangeKind, c machineinfo.SessionChange, changed bool) wsclient.SessionChanged {
	p := wsclient.SessionChanged{
		Events:      make([]string, 0, len(kinds)),
		SessionID:   c.SessionID,
		SessionUser: c.SessionUser,
		Changed:     changed,
	}
	for _, k := range kinds {
		p.Events = append(p.Events, string(k))
	}
	if !c.At.IsZero() {
		p.At = c.At.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	}
	if c.LoggedUser.User != "" {
		p.LoggedUser = userSession(c.LoggedUser)
	}
	return p
}

func userSession(s machineinfo.UserSession) *wsclient.UserSession {
	out := &wsclient.UserSession{User: s.User, State: string(s.State)}
	if !s.DisconnectedAt.IsZero() {
		out.DisconnectedAt = s.DisconnectedAt.UTC().Format(time.RFC3339)
	}
	return out
}

type machineInfoPayload struct {
	HWID           string                `json:"hwid,omitempty"`
	Hostname       string                `json:"hostname,omitempty"`
	ADDomain       string                `json:"ad_domain,omitempty"`
	Serial         string                `json:"serial,omitempty"`
	Product        string                `json:"product,omitempty"`
	OSVersion      string                `json:"os_version,omitempty"`
	EMLyVersion    string                `json:"emly_version,omitempty"`
	UpdaterVersion string                `json:"updater_version,omitempty"`
	LoggedUser     *wsclient.UserSession `json:"logged_user,omitempty"`
	BootTime       string                `json:"boot_time,omitempty"`
	UptimeSeconds  int64                 `json:"uptime_seconds,omitempty"`
	Network        *networkInfo          `json:"network,omitempty"`
	Site           *siteInfo             `json:"site,omitempty"`
	Config         *configInfo           `json:"config,omitempty"`
	Hardware       *hardwareInfo         `json:"hardware,omitempty"`
	EMLy           *emlyInfo             `json:"emly,omitempty"`
	PendingUpdate  *wsclient.PendingInfo `json:"pending_update,omitempty"`
	SelfUpdate     *selfUpdateInfo       `json:"self_update,omitempty"`
}

type networkInfo struct {
	Interfaces []machineinfo.NetInterface `json:"interfaces"`
}
type siteInfo struct {
	DC          string              `json:"dc,omitempty"`
	MatchedSite string              `json:"matched_site,omitempty"`
	Server      *wsclient.ServerRef `json:"server,omitempty"`
}
type configInfo struct {
	Revision  int64  `json:"revision"`
	FetchedAt string `json:"fetched_at,omitempty"`
	Source    string `json:"source"`
}
type hardwareInfo struct {
	CPU         string     `json:"cpu,omitempty"`
	Cores       int        `json:"cores,omitempty"`
	MemoryMB    uint64     `json:"memory_mb,omitempty"`
	SystemDrive *driveInfo `json:"system_drive,omitempty"`
}
type driveInfo struct {
	TotalMB uint64 `json:"total_mb"`
	FreeMB  uint64 `json:"free_mb"`
}
type emlyInfo struct {
	Installed  bool   `json:"installed"`
	Running    bool   `json:"running"`
	InstallDir string `json:"install_dir,omitempty"`
	Channel    string `json:"channel,omitempty"`
	Language   string `json:"language,omitempty"`
}
type selfUpdateInfo struct {
	Version  string `json:"version"`
	Attempts int    `json:"attempts"`
	GaveUp   bool   `json:"gave_up,omitempty"`
}

// machineInfo builds spec §6.5. sections nil = everything. It reads the
// current cycle through u.cur (atomic) and never u.site, which the poll
// goroutine mutates.
func (u *Updater) machineInfo(sections []string) machineInfoPayload {
	want := func(s string) bool { return len(sections) == 0 || slices.Contains(sections, s) }
	var p machineInfoPayload
	now := u.clock()

	if want("identity") {
		src := u.newHTTPSource("")
		p.HWID, p.Hostname, p.ADDomain = src.HWID, src.Hostname, src.ADDomain
		p.Serial, p.Product, p.OSVersion = src.Serial, src.Product, src.OSVersion
		p.EMLyVersion, p.UpdaterVersion = src.EMLyVersion, version.Version
	}
	if want("logged_user") {
		if s := u.loggedUser(); s.User != "" {
			p.LoggedUser = userSession(s)
		}
	}
	if want("hardware") || want("identity") {
		f := u.systemFacts(now)
		if !f.BootTime.IsZero() {
			p.BootTime = f.BootTime.Format(time.RFC3339)
			p.UptimeSeconds = int64(now.Sub(f.BootTime).Seconds())
		}
		if want("hardware") {
			p.Hardware = &hardwareInfo{CPU: f.CPU, Cores: f.Cores, MemoryMB: f.MemoryMB}
			if f.SystemDriveTotalMB > 0 {
				p.Hardware.SystemDrive = &driveInfo{TotalMB: f.SystemDriveTotalMB, FreeMB: f.SystemDriveFreeMB}
			}
		}
	}
	if want("network") {
		p.Network = &networkInfo{Interfaces: u.netInterfaces()}
	}
	if cyc := u.cur.Load(); cyc != nil {
		if want("site") {
			p.Site = &siteInfo{DC: cyc.host.DC, MatchedSite: cyc.site}
			if chain := u.preferredChain(cyc); len(chain) > 0 {
				p.Site.Server = u.serverRef(cyc, cyc.eff.ManifestURL(chain[0]))
			}
		}
		if want("config") {
			src := cyc.snap.Source.String()
			if src == "default" {
				src = "legacy"
			}
			p.Config = &configInfo{Revision: cyc.snap.Revision(), Source: src}
			if !cyc.snap.FetchedAt.IsZero() {
				p.Config.FetchedAt = cyc.snap.FetchedAt.UTC().Format(time.RFC3339)
			}
		}
	}
	if want("emly") {
		info := u.Cfg.ResolveEMLy()
		p.EMLy = &emlyInfo{Installed: !info.FreshInstall, Running: u.emlyRunning(),
			InstallDir: u.Cfg.EMLyInstallDir, Channel: info.Channel, Language: info.Language}
	}
	if want("update") {
		if st, err := u.Store.Load(); err == nil {
			if st.Pending != nil {
				p.PendingUpdate = &wsclient.PendingInfo{Version: st.Pending.Version, Forced: st.Pending.Forced,
					DownloadedAt: st.Pending.DownloadedAt.UTC().Format(time.RFC3339)}
			}
			if st.SelfUpdate != nil {
				p.SelfUpdate = &selfUpdateInfo{Version: st.SelfUpdate.Version, Attempts: st.SelfUpdate.Attempts, GaveUp: st.SelfUpdate.GaveUp}
			}
		}
	}
	return p
}

// serverRef names the role of the server that answered (spec §6.2).
func (u *Updater) serverRef(cyc *cycleState, url string) *wsclient.ServerRef {
	if url == "" {
		return nil
	}
	role := "backup"
	switch {
	case cyc.site == "":
		role = "default"
	case len(cyc.chain) > 0 && url == cyc.eff.ManifestURL(cyc.chain[0]):
		role = "primary"
	}
	return &wsclient.ServerRef{URL: url, Role: role}
}

func (u *Updater) netInterfaces() []machineinfo.NetInterface {
	if u.netInterfacesFn != nil {
		return u.netInterfacesFn()
	}
	return machineinfo.NetworkInterfaces()
}

func (u *Updater) emlyRunning() bool {
	if u.emlyRunningFn != nil {
		return u.emlyRunningFn()
	}
	return process.IsRunning(u.Cfg.EMLyExeName)
}
