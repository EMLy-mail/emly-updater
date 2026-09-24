package wsclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
)

// Protocol v2 of GET /v2/client/ws. The normative reference is
// emly-go-api/CLIENT_WS_PROTOCOL.md; the API's internal/clientproto holds
// the same names and shapes. A change here without the same change there
// (or the other way round) is a protocol split.
const ProtocolV2 = 2

const (
	TypeWelcome = "welcome"
	TypeCommand = "command"
	TypeAck     = "ack"
	TypeResult  = "result"
	TypeEvent   = "event"
	TypeNotify  = "notify"
)

const (
	CmdMachineInfo          = "machine.info"
	CmdEMLyManifestCheck    = "emly.manifest.check"
	CmdUpdaterManifestCheck = "updater.manifest.check"
	CmdAppsListUpgradable   = "apps.list_upgradable"
	CmdServiceRestart       = "service.restart"
	CmdMachineReboot        = "machine.reboot"
)

const (
	EvtMachineInfo     = "machine.info"
	EvtSessionChanged  = "session.changed"
	EvtUpdateAvailable = "update.available"
	EvtUpdateStarted   = "update.started"
	EvtUpdateApplied   = "update.applied"
	EvtUpdateFailed    = "update.failed"
	EvtServiceStarted  = "service.started"
)

const (
	TopicReleasePublished = "release.published"
	TopicConfigPublished  = "config.published"
)

const (
	ErrUnsupportedCommand = "unsupported_command"
	ErrInvalidArgs        = "invalid_args"
	ErrExpired            = "expired"
	ErrBusy               = "busy"
	ErrDisabledByPolicy   = "disabled_by_policy"
	ErrInsecureTransport  = "insecure_transport"
	ErrUserActive         = "user_active"
	ErrWingetModuleMissing = "winget_module_missing"
	ErrPowerShellNotFound = "powershell_not_found"
	ErrSourcesUnreachable = "sources_unreachable"
	ErrNotFound           = "not_found"
	ErrManifestInvalid    = "manifest_invalid"
	ErrTimeout            = "timeout"
	ErrInternal           = "internal"
)

var (
	CommandNames = []string{CmdMachineInfo, CmdEMLyManifestCheck, CmdUpdaterManifestCheck,
		CmdAppsListUpgradable, CmdServiceRestart, CmdMachineReboot}
	EventNames = []string{EvtMachineInfo, EvtSessionChanged, EvtUpdateAvailable,
		EvtUpdateStarted, EvtUpdateApplied, EvtUpdateFailed, EvtServiceStarted}
	TopicNames = []string{TopicReleasePublished, TopicConfigPublished}
)

// Destructive verbs need a wss:// connection (spec §12.2).
func Destructive(name string) bool {
	return name == CmdServiceRestart || name == CmdMachineReboot
}

type Limits struct {
	MaxMessageBytes    int `json:"max_message_bytes"`
	MaxEventsPerMinute int `json:"max_events_per_minute"`
	AckTimeoutSeconds  int `json:"ack_timeout_seconds"`
}

var DefaultLimits = Limits{MaxMessageBytes: 65536, MaxEventsPerMinute: 60, AckTimeoutSeconds: 5}

type Welcome struct {
	Protocol             int      `json:"protocol"`
	AcceptedCapabilities []string `json:"accepted_capabilities"`
	Limits               Limits   `json:"limits"`
}

type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

type Command struct {
	Name      string          `json:"name"`
	Args      json.RawMessage `json:"args,omitempty"`
	ExpiresAt string          `json:"expires_at"`
	IssuedBy  string          `json:"issued_by,omitempty"`
}

type Ack struct {
	Accepted  bool       `json:"accepted"`
	Error     *ErrorBody `json:"error,omitempty"`
	Duplicate bool       `json:"duplicate,omitempty"`
}

const (
	ResultOK    = "ok"
	ResultError = "error"
)

type Result struct {
	Status     string          `json:"status"`
	DurationMS int64           `json:"duration_ms,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	Error      *ErrorBody      `json:"error,omitempty"`
	Truncated  bool            `json:"truncated,omitempty"`
}

type Event struct {
	Name    string `json:"name"`
	Payload any    `json:"payload,omitempty"`
}

type Notify struct {
	Topic   string          `json:"topic"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type UserSession struct {
	User           string `json:"user,omitempty"`
	State          string `json:"state,omitempty"`
	DisconnectedAt string `json:"disconnected_at,omitempty"`
}

type SessionChanged struct {
	Events      []string     `json:"events"`
	SessionID   uint32       `json:"session_id"`
	SessionUser string       `json:"session_user,omitempty"`
	LoggedUser  *UserSession `json:"logged_user,omitempty"`
	Changed     bool         `json:"changed"`
	At          string       `json:"at,omitempty"`
}

type ServiceStarted struct {
	Reason            string   `json:"reason"`
	BootTime          string   `json:"boot_time,omitempty"`
	StartedAt         string   `json:"started_at,omitempty"`
	UpdaterVersion    string   `json:"updater_version,omitempty"`
	PreviousVersion   string   `json:"previous_version,omitempty"`
	CompletedCommands []string `json:"completed_commands,omitempty"`
}

type ReleasePublished struct {
	Target        string `json:"target"`
	Channel       string `json:"channel,omitempty"`
	Version       string `json:"version"`
	JitterSeconds int    `json:"jitter_seconds"`
}

type ConfigPublished struct {
	Revision      int64 `json:"revision"`
	JitterSeconds int   `json:"jitter_seconds"`
}

// ServerRef is spec §6.2.
type ServerRef struct {
	URL  string `json:"url"`
	Role string `json:"role"` // primary | backup | default
}

type PendingInfo struct {
	Version      string `json:"version"`
	Forced       bool   `json:"forced"`
	DownloadedAt string `json:"downloaded_at,omitempty"`
}

// ManifestCheck is spec §6.3.
type ManifestCheck struct {
	Target             string       `json:"target"`
	InstalledVersion   string       `json:"installed_version,omitempty"`
	Channel            string       `json:"channel,omitempty"`
	AvailableVersion   string       `json:"available_version,omitempty"`
	UpdateAvailable    bool         `json:"update_available"`
	Critical           bool         `json:"critical,omitempty"`
	MinRequiredVersion string       `json:"min_required_version,omitempty"`
	Decision           string       `json:"decision"`
	Pending            *PendingInfo `json:"pending,omitempty"`
	Source             *ServerRef   `json:"source,omitempty"`
	CheckedAt          string       `json:"checked_at"`
	Error              *ErrorBody   `json:"error,omitempty"`
}

// UpgradablePackage is spec §6.4.
type UpgradablePackage struct {
	Name             string `json:"name"`
	ID               string `json:"id"`
	InstalledVersion string `json:"installed_version"`
	AvailableVersion string `json:"available_version"`
	Source           string `json:"source"`
}

const (
	RebootDefaultDelaySeconds = 300
	RebootMaxDelaySeconds     = 3600
	WhenUserActiveWarn        = "warn"
	WhenUserActiveSkip        = "skip"
)

var MachineInfoSections = []string{"identity", "logged_user", "network", "site", "config", "hardware", "emly", "update"}

type MachineInfoArgs struct {
	Sections []string `json:"sections,omitempty"`
}

type RebootArgs struct {
	DelaySeconds   *int   `json:"delay_seconds,omitempty"`
	WhenUserActive string `json:"when_user_active,omitempty"`
}

func (a RebootArgs) Delay() int {
	if a.DelaySeconds == nil {
		return RebootDefaultDelaySeconds
	}
	return *a.DelaySeconds
}

func (a RebootArgs) Mode() string {
	if a.WhenUserActive == "" {
		return WhenUserActiveWarn
	}
	return a.WhenUserActive
}

// DecodeArgs decodes a command's args rejecting unknown fields; empty raw is
// valid and leaves v zero.
func DecodeArgs(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func invalidArgs(format string, args ...any) *ErrorBody {
	return &ErrorBody{Code: ErrInvalidArgs, Message: fmt.Sprintf(format, args...)}
}

// ValidateArgs is the same check the API runs before sending (clientproto).
// The client repeats it because it never trusts the server (spec §2.1).
func ValidateArgs(name string, raw json.RawMessage) *ErrorBody {
	switch name {
	case CmdMachineInfo:
		var a MachineInfoArgs
		if err := DecodeArgs(raw, &a); err != nil {
			return invalidArgs("machine.info: %v", err)
		}
		for _, s := range a.Sections {
			if !slices.Contains(MachineInfoSections, s) {
				return invalidArgs("machine.info: unknown section %q", s)
			}
		}
		return nil
	case CmdMachineReboot:
		var a RebootArgs
		if err := DecodeArgs(raw, &a); err != nil {
			return invalidArgs("machine.reboot: %v", err)
		}
		if d := a.Delay(); d < 0 || d > RebootMaxDelaySeconds {
			return invalidArgs("machine.reboot: delay_seconds must be between 0 and %d", RebootMaxDelaySeconds)
		}
		if m := a.Mode(); m != WhenUserActiveWarn && m != WhenUserActiveSkip {
			return invalidArgs("machine.reboot: when_user_active must be %q or %q", WhenUserActiveWarn, WhenUserActiveSkip)
		}
		return nil
	case CmdEMLyManifestCheck, CmdUpdaterManifestCheck, CmdAppsListUpgradable, CmdServiceRestart:
		var none struct{}
		if err := DecodeArgs(raw, &none); err != nil {
			return invalidArgs("%s takes no arguments: %v", name, err)
		}
		return nil
	default:
		return &ErrorBody{Code: ErrUnsupportedCommand, Message: fmt.Sprintf("unknown command %q", name)}
	}
}
