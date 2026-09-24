# Client WS protocol v2 — Updater side Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the updater's presence channel speak protocol v2 of `emly-go-api/CLIENT_WS_PROTOCOL.md`: declare capabilities, execute the six commands (machine.info, emly/updater manifest dry-run checks, winget upgradable list, service restart, PC reboot), push events (session changes, update available/started/applied/failed, service started, machine info) and react to `release.published` / `config.published` notifies by waking the poll loop.

**Architecture:** `internal/wsclient` stays pure Go and learns the v2 wire format and a `Handler` callback interface; it knows nothing about what a command does. `internal/service` implements that interface: `clientcmd.go` admits and runs commands (allowlist from the remote document, `wss://`-only for destructive verbs, dedupe ring), `clientevents.go` turns what the service already does into events, `clientnotify.go` turns notifies into a jittered wake-up of `RunLoop`. The channel never calls `Cycle` (R-LOOP); the manifest checks are read-only dry runs.

**Tech Stack:** Go 1.26.1, Windows only, `github.com/coder/websocket` v1.8.15 (`Conn` methods are safe for concurrent use except `Read`/`Reader`, so the heartbeat loop and command goroutines can write to one connection), `golang.org/x/sys/windows`, the `internal/winget` package from branch `feat/winget-upgradable`.

**Spec:** `emly-go-api/CLIENT_WS_PROTOCOL.md` (branch `docs/client-ws-protocol`). Twin plan: `emly-go-api/docs/superpowers/plans/2026-09-23-client-ws-v2-api.md` — its Tasks 1–3 define the same wire types and fixtures this plan mirrors.

## Global Constraints

- Everything v1 keeps working: against a server that never sends `welcome`, the client behaves exactly as today (identity, pongs) and executes nothing.
- Wire names are verbatim from the spec: `snake_case` fields; command/event/topic names and error codes exactly as in `emly-go-api/internal/clientproto/protocol.go`.
- `ping`/`pong` bytes unchanged: `{"type":"ping"}` / `{"type":"pong"}`.
- Identity v2 adds `"protocol": 2` and `"capabilities": [...]`; every other identity field keeps its v1 semantics (absent ≠ empty).
- Default `clientWs.commands` (absent from the document, or legacy policy): `machine.info`, `emly.manifest.check`, `updater.manifest.check`, `apps.list_upgradable`. `service.restart` and `machine.reboot` only when a document lists them.
- `service.restart` and `machine.reboot` are refused with `insecure_transport` on a `ws://` connection.
- `ack` within 5s of receiving a command; one command of a given name at a time (`busy`); dedupe ring of 64 IDs.
- `machine.reboot`: `delay_seconds` 0–3600 (default 300), `when_user_active` `warn`|`skip` (default `warn`); with a user active the effective delay is at least 60s.
- `release.published`: wait a uniformly random time in `[0, max(jitter_seconds, 60)]` seconds before waking the loop.
- Outgoing frames ≤ 65536 bytes; list results are truncated with `truncated: true` instead.
- Events emitted while no v2 session is up are buffered in memory (max 32, oldest dropped) and flushed after `service.started` on the next `welcome`. Nothing is persisted except the IDs of pending `service.restart`/`machine.reboot` commands.
- `go test ./...` stays pure Go (no Windows API calls in tests); new Windows calls sit behind seams on `Updater`.
- `testdata/remoteconfig/` files touched here are byte-identical to the API's.
- Commits without any `Co-Authored-By` trailer.

## Review Focus

1. **Server that speaks v1 only (an old mirror)** — no `welcome` ever arrives; a `command` frame arriving anyway (should never happen) must not be executed. Test: `TestCommandIgnoredBeforeWelcome` (Task 2).
2. **The same command delivered twice after a flaky reconnect** — second copy gets `ack duplicate:true` and the stored result, the handler runs once. Test: `TestExecuteCommandDedupes` (Task 7).
3. **`machine.reboot` on a site mirror reached over plain `ws://`** — refused with `insecure_transport`, nothing persisted, no reboot. Test: `TestDestructiveRefusedOnInsecureTransport` (Task 8).
4. **Update applied by the self-update, reported by the new binary before its channel is up** — the `update.applied` event must survive until the first `welcome` and be sent after `service.started`. Test: `TestEventsBufferedUntilWelcome` (Task 6).
5. **A burst of `release.published` notifies (an admin clicking twice, or a fleet broadcast)** — at most one pending wake, never two concurrent cycles. Test: `TestNotifyCoalescesWakes` (Task 10).

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/wsclient/id.go` (new) | `NewID()` ULID — verbatim copy of `emly-go-api/internal/clientproto/id.go`. |
| `internal/wsclient/protocol.go` (new) | v2 message types, names, payloads, `ValidateArgs` (mirror of the API's `clientproto`). |
| `internal/wsclient/client.go` | `Message` gets `id`/`reply_to`/`ts`; `Identity` gets `protocol`/`capabilities`; `Handler`, `Session`, v2 dispatch. |
| `internal/wsclient/session.go` (new) | `Session`: ack/result/event writers, size limit, `Secure()`. |
| `internal/policy/document.go`, `parse.go`, `legacy.go` | `clientWs.commands` + `Allows`. |
| `testdata/remoteconfig/...` | Shared fixtures (copied from the API). |
| `internal/state/state.go` | `PendingCommands`. |
| `internal/machineinfo/system.go`, `system_windows.go`?, `netif.go` (new) | Boot time, CPU, memory, system drive, network interfaces. |
| `internal/power/power.go` (new) | `Reboot` via `InitiateSystemShutdownEx`. |
| `internal/service/clientevents.go` (new) | `emit`, event buffer, `clientHandler.Welcome`, `service.started`, `machine.info` builder. |
| `internal/service/clientcmd.go` (new) | Command admission, dedupe, dispatch, the four read-only verbs. |
| `internal/service/clientpower.go` (new) | `service.restart`, `machine.reboot`. |
| `internal/service/clientnotify.go` (new) | Notify → jittered wake; `RunLoop` wake channel. |
| `internal/service/clientws.go`, `service.go`, `selfupdate.go`, `sessionwatch.go` | Hooks. |
| `internal/logging/logging.go` | Events 924/925. |
| `main.go` | Hidden `restart-service` subcommand. |
| `AGENTS.md` (+ `CLAUDE.md` if the winget merge brings it) | Conventions. |

---

### Task 0: Branch

- [ ] **Step 1: Create the branch on top of the session watcher and merge winget**

```bash
cd C:/Users/LyzCoote/Desktop/3gIT/EMLy/emly-updater
git status --short                       # must print nothing
git switch feat/session-change-watcher
git switch -c feat/client-ws-v2
git merge --no-ff feat/winget-upgradable -m "merge feat/winget-upgradable into feat/client-ws-v2"
```

If the merge conflicts (most likely in `AGENTS.md`), keep both sides' text. Then:

Run: `go build ./... && go test ./...`
Expected: PASS.

- [ ] **Step 2: Commit this plan**

```bash
git add docs/superpowers/plans/2026-09-23-client-ws-v2-updater.md
git commit -m "docs: add implementation plan for client ws protocol v2 (updater)"
```

---

### Task 1: `wsclient` wire types (ID + protocol mirror)

**Files:**
- Create: `internal/wsclient/id.go`, `internal/wsclient/id_test.go`
- Create: `internal/wsclient/protocol.go`, `internal/wsclient/protocol_test.go`

**Interfaces:**
- Produces: `NewID()`; consts `ProtocolV2`, `TypeWelcome`, `TypeCommand`, `TypeAck`, `TypeResult`, `TypeEvent`, `TypeNotify`; `Cmd*`, `Evt*`, `Topic*`, `Err*` names; types `Welcome`, `Limits`, `ErrorBody`, `Command`, `Ack`, `Result`, `Event`, `Notify`, `UserSession`, `SessionChanged`, `ServiceStarted`, `ReleasePublished`, `ConfigPublished`, `ServerRef`, `ManifestCheck`, `PendingInfo`, `UpgradablePackage`, `RebootArgs`, `MachineInfoArgs`; `DefaultLimits`; `CommandNames`, `EventNames`, `TopicNames` ([]string); `Destructive(name) bool`; `ValidateArgs(name, raw) *ErrorBody`.

- [ ] **Step 1: `id.go` + test** — copy `emly-go-api/internal/clientproto/id.go` and `id_test.go` verbatim, changing only `package clientproto` → `package wsclient` and the package doc comment (keep the sentence saying the two files are copies of each other). The code, for reference:

```go
package wsclient

import (
	"crypto/rand"
	"io"
	"time"
)

// crockford is the ULID alphabet: base32 without I, L, O, U.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewID returns a ULID (48-bit ms time + 80 random bits, 26 Crockford
// base32 chars). Verbatim copy of emly-go-api/internal/clientproto/id.go:
// both ends mint message IDs and they must look the same.
func NewID() string { return newIDAt(time.Now(), rand.Reader) }

func newIDAt(t time.Time, entropy io.Reader) string {
	var b [16]byte
	ms := uint64(t.UnixMilli())
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (40 - 8*i))
	}
	if _, err := io.ReadFull(entropy, b[6:]); err != nil {
		clear(b[6:])
	}
	var out [26]byte
	for i := range out {
		var v byte
		for j := 0; j < 5; j++ {
			p := i*5 + j - 2
			v <<= 1
			if p >= 0 && b[p/8]&(0x80>>(p%8)) != 0 {
				v |= 1
			}
		}
		out[i] = crockford[v]
	}
	return string(out[:])
}
```

The test file is the API's `id_test.go` (`TestNewIDBounds`, `TestNewIDTimePrefix`, `TestNewIDSortsByTime`, `TestNewIDShapeAndUniqueness`) with the package line changed.

- [ ] **Step 2: Write the failing protocol test** (`protocol_test.go`)

```go
package wsclient

import (
	"encoding/json"
	"testing"
)

func TestValidateArgsMirrorsTheAPI(t *testing.T) {
	cases := []struct {
		name, args, want string
	}{
		{CmdMachineInfo, ``, ""},
		{CmdMachineInfo, `{"sections":["network"]}`, ""},
		{CmdMachineInfo, `{"sections":["gpu"]}`, ErrInvalidArgs},
		{CmdAppsListUpgradable, `{"x":1}`, ErrInvalidArgs},
		{CmdMachineReboot, `{"delay_seconds":3600,"when_user_active":"skip"}`, ""},
		{CmdMachineReboot, `{"delay_seconds":3601}`, ErrInvalidArgs},
		{CmdMachineReboot, `{"when_user_active":"force"}`, ErrInvalidArgs},
		{"machine.format_disk", `{}`, ErrUnsupportedCommand},
	}
	for _, c := range cases {
		got := ValidateArgs(c.name, json.RawMessage(c.args))
		if (c.want == "") != (got == nil) || (got != nil && got.Code != c.want) {
			t.Errorf("ValidateArgs(%s, %s) = %+v, want %q", c.name, c.args, got, c.want)
		}
	}
}

func TestRebootArgsDefaults(t *testing.T) {
	var a RebootArgs
	if a.Delay() != 300 || a.Mode() != WhenUserActiveWarn {
		t.Fatalf("defaults = %d %s", a.Delay(), a.Mode())
	}
}

func TestDestructive(t *testing.T) {
	if !Destructive(CmdServiceRestart) || !Destructive(CmdMachineReboot) || Destructive(CmdMachineInfo) {
		t.Fatal("destructive set wrong")
	}
}

func TestManifestCheckOmitsUnknowns(t *testing.T) {
	b, _ := json.Marshal(ManifestCheck{Target: "emly", Decision: "up_to_date", CheckedAt: "t"})
	for _, key := range []string{"installed_version", "available_version", "pending", "source", "error", "channel"} {
		if strings.Contains(string(b), `"`+key+`"`) {
			t.Errorf("%s present in %s", key, b)
		}
	}
}
```

(imports: `encoding/json`, `strings`, `testing`.)

Run: `go test ./internal/wsclient/ -run "ValidateArgs|RebootArgs|Destructive|ManifestCheck" -v`
Expected: FAIL — undefined names.

- [ ] **Step 3: Implement `protocol.go`**

```go
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
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/wsclient/ -v`
Expected: PASS (new and existing).

- [ ] **Step 5: Commit**

```bash
git add internal/wsclient/id.go internal/wsclient/id_test.go internal/wsclient/protocol.go internal/wsclient/protocol_test.go
git commit -m "feat(wsclient): client ws protocol v2 wire types, names and ULIDs"
```

---

### Task 2: `wsclient.Client` v2 — capabilities, welcome, `Handler`, `Session`

**Files:**
- Modify: `internal/wsclient/client.go`
- Create: `internal/wsclient/session.go`
- Test: `internal/wsclient/client_test.go`

**Interfaces:**
- Consumes: Task 1.
- Produces:

```go
type Message struct { Type, ID, ReplyTo, TS string; Data json.RawMessage } // json: type,id,reply_to,ts,data (all but type omitempty)
type Identity struct { /* v1 fields */ ; Protocol int `json:"protocol,omitempty"`; Capabilities []string `json:"capabilities,omitempty"` }
type Handler interface {
	Welcome(s *Session, w Welcome)
	Command(ctx context.Context, s *Session, msg Message, cmd Command)
	Notify(s *Session, msg Message, n Notify)
}
// Client gains: Handler Handler
type Session struct { ... }
func (s *Session) Secure() bool
func (s *Session) Accepted(name string) bool
func (s *Session) Ack(ctx context.Context, replyTo string, a Ack) error
func (s *Session) Result(ctx context.Context, replyTo string, r Result) error
func (s *Session) Event(ctx context.Context, name string, payload any) error
func (s *Session) Close(code websocket.StatusCode, reason string)
var ErrTooLarge error
```

Rules: a `welcome` switches the connection to v2 and calls `Handler.Welcome` once; `command`/`notify` before a `welcome` are ignored (logged); each `command` runs `Handler.Command` on its own goroutine with a context that ends when `Run` returns; `Handler` nil = v1 behaviour even if the server sends `welcome`.

- [ ] **Step 1: Write the failing tests** (append to `client_test.go`; read the existing server helpers in that file first and reuse them where they fit)

```go
type recHandler struct {
	welcomes chan *Session
	commands chan Command
	notifies chan Notify
}

func newRecHandler() *recHandler {
	return &recHandler{welcomes: make(chan *Session, 1), commands: make(chan Command, 4), notifies: make(chan Notify, 4)}
}

func (h *recHandler) Welcome(s *Session, w Welcome) { h.welcomes <- s }
func (h *recHandler) Command(ctx context.Context, s *Session, msg Message, cmd Command) {
	_ = s.Ack(ctx, msg.ID, Ack{Accepted: true})
	h.commands <- cmd
}
func (h *recHandler) Notify(s *Session, msg Message, n Notify) { h.notifies <- n }

// v2Server runs a fake API: hello, read identity (handed to gotIdentity),
// then script(c).
func v2Server(t *testing.T, gotIdentity chan<- Identity, script func(ctx context.Context, c *websocket.Conn)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		_ = wsjson.Write(ctx, c, Message{Type: TypeHello})
		var msg Message
		if err := wsjson.Read(ctx, c, &msg); err != nil {
			return
		}
		var id Identity
		_ = json.Unmarshal(msg.Data, &id)
		gotIdentity <- id
		script(ctx, c)
	}))
}

func welcomeFrame(caps ...string) Message {
	data, _ := json.Marshal(Welcome{Protocol: 2, AcceptedCapabilities: caps, Limits: DefaultLimits})
	return Message{Type: TypeWelcome, ID: NewID(), Data: data}
}

func runClient(t *testing.T, srv *httptest.Server, h Handler) (context.CancelFunc, <-chan error) {
	t.Helper()
	url, _ := URLFor(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	c := &Client{URL: url, Identity: Identity{HWID: "HW-1", Protocol: 2, Capabilities: []string{CmdMachineInfo}}, Handler: h}
	go func() { done <- c.Run(ctx) }()
	return cancel, done
}

func TestIdentityCarriesProtocolAndCapabilities(t *testing.T) {
	ids := make(chan Identity, 1)
	srv := v2Server(t, ids, func(ctx context.Context, c *websocket.Conn) { <-ctx.Done() })
	defer srv.Close()
	cancel, _ := runClient(t, srv, newRecHandler())
	defer cancel()
	select {
	case id := <-ids:
		if id.Protocol != 2 || len(id.Capabilities) != 1 || id.Capabilities[0] != CmdMachineInfo {
			t.Fatalf("identity = %+v", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no identity")
	}
}

func TestWelcomeThenCommandIsDispatchedAndAcked(t *testing.T) {
	ids := make(chan Identity, 1)
	acks := make(chan Message, 1)
	srv := v2Server(t, ids, func(ctx context.Context, c *websocket.Conn) {
		_ = wsjson.Write(ctx, c, welcomeFrame(CmdMachineInfo))
		data, _ := json.Marshal(Command{Name: CmdMachineInfo, ExpiresAt: "2099-01-01T00:00:00Z"})
		cmdID := NewID()
		_ = wsjson.Write(ctx, c, Message{Type: TypeCommand, ID: cmdID, Data: data})
		var m Message
		if wsjson.Read(ctx, c, &m) == nil {
			acks <- m
		}
		<-ctx.Done()
	})
	defer srv.Close()
	h := newRecHandler()
	cancel, _ := runClient(t, srv, h)
	defer cancel()

	select {
	case s := <-h.welcomes:
		if s.Secure() || !s.Accepted(CmdMachineInfo) || s.Accepted(CmdMachineReboot) {
			t.Fatalf("session secure=%v", s.Secure())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no welcome")
	}
	select {
	case cmd := <-h.commands:
		if cmd.Name != CmdMachineInfo {
			t.Fatalf("cmd = %+v", cmd)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("command not dispatched")
	}
	select {
	case m := <-acks:
		if m.Type != TypeAck || m.ReplyTo == "" || len(m.ID) != 26 {
			t.Fatalf("ack = %+v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no ack")
	}
}

func TestCommandIgnoredBeforeWelcome(t *testing.T) {
	ids := make(chan Identity, 1)
	srv := v2Server(t, ids, func(ctx context.Context, c *websocket.Conn) {
		data, _ := json.Marshal(Command{Name: CmdMachineInfo})
		_ = wsjson.Write(ctx, c, Message{Type: TypeCommand, ID: NewID(), Data: data})
		_ = wsjson.Write(ctx, c, Message{Type: TypePing})
		var pong Message
		_ = wsjson.Read(ctx, c, &pong)
		<-ctx.Done()
	})
	defer srv.Close()
	h := newRecHandler()
	cancel, _ := runClient(t, srv, h)
	defer cancel()
	select {
	case <-h.commands:
		t.Fatal("command executed on a connection that never received welcome")
	case <-time.After(500 * time.Millisecond):
	}
}

func TestNotifyDispatched(t *testing.T) {
	ids := make(chan Identity, 1)
	srv := v2Server(t, ids, func(ctx context.Context, c *websocket.Conn) {
		_ = wsjson.Write(ctx, c, welcomeFrame(TopicConfigPublished))
		payload, _ := json.Marshal(ConfigPublished{Revision: 44, JitterSeconds: 60})
		data, _ := json.Marshal(Notify{Topic: TopicConfigPublished, Payload: payload})
		_ = wsjson.Write(ctx, c, Message{Type: TypeNotify, ID: NewID(), Data: data})
		<-ctx.Done()
	})
	defer srv.Close()
	h := newRecHandler()
	cancel, _ := runClient(t, srv, h)
	defer cancel()
	select {
	case n := <-h.notifies:
		if n.Topic != TopicConfigPublished {
			t.Fatalf("n = %+v", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("notify not dispatched")
	}
}

func TestSessionRefusesOversizedFrames(t *testing.T) {
	ids := make(chan Identity, 1)
	srv := v2Server(t, ids, func(ctx context.Context, c *websocket.Conn) {
		_ = wsjson.Write(ctx, c, welcomeFrame(EvtMachineInfo))
		<-ctx.Done()
	})
	defer srv.Close()
	h := newRecHandler()
	cancel, _ := runClient(t, srv, h)
	defer cancel()
	s := <-h.welcomes
	big := strings.Repeat("x", DefaultLimits.MaxMessageBytes)
	if err := s.Event(context.Background(), EvtMachineInfo, big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/wsclient/ -v`
Expected: FAIL — `Handler`/`Session` undefined.

- [ ] **Step 3: Implement `session.go`**

```go
package wsclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// ErrTooLarge is a frame over the negotiated max_message_bytes. The caller
// truncates what it sends instead (spec §11).
var ErrTooLarge = errors.New("message exceeds max_message_bytes")

// Session is one v2 connection as seen by the Handler. Its methods are safe
// to call from any goroutine (coder/websocket allows concurrent writes) and
// fail harmlessly once the connection is gone.
type Session struct {
	conn     *websocket.Conn
	secure   bool
	accepted map[string]bool
	limits   Limits
}

func newSession(conn *websocket.Conn, url string, w Welcome) *Session {
	acc := make(map[string]bool, len(w.AcceptedCapabilities))
	for _, c := range w.AcceptedCapabilities {
		acc[c] = true
	}
	limits := w.Limits
	if limits.MaxMessageBytes <= 0 {
		limits = DefaultLimits
	}
	return &Session{conn: conn, secure: strings.HasPrefix(url, "wss://"), accepted: acc, limits: limits}
}

// Secure reports whether the connection is TLS (wss://), the condition for
// destructive commands (spec §12.2).
func (s *Session) Secure() bool { return s.secure }

// Accepted reports whether the server accepted this capability in welcome.
func (s *Session) Accepted(name string) bool { return s.accepted[name] }

// Limits is what the server announced.
func (s *Session) Limits() Limits { return s.limits }

func (s *Session) write(ctx context.Context, typ, replyTo string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	b, err := json.Marshal(Message{Type: typ, ID: NewID(), ReplyTo: replyTo,
		TS: time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"), Data: raw})
	if err != nil {
		return err
	}
	if len(b) > s.limits.MaxMessageBytes {
		return ErrTooLarge
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return s.conn.Write(wctx, websocket.MessageText, b)
}

func (s *Session) Ack(ctx context.Context, replyTo string, a Ack) error {
	return s.write(ctx, TypeAck, replyTo, a)
}

func (s *Session) Result(ctx context.Context, replyTo string, r Result) error {
	return s.write(ctx, TypeResult, replyTo, r)
}

func (s *Session) Event(ctx context.Context, name string, payload any) error {
	return s.write(ctx, TypeEvent, "", Event{Name: name, Payload: payload})
}

// Close ends the connection with code (1001 before a restart or reboot).
func (s *Session) Close(code websocket.StatusCode, reason string) {
	_ = s.conn.Close(code, reason)
}
```

- [ ] **Step 4: Modify `client.go`**
  - `Message` becomes:
    ```go
    type Message struct {
    	Type    string          `json:"type"`
    	ID      string          `json:"id,omitempty"`
    	ReplyTo string          `json:"reply_to,omitempty"`
    	TS      string          `json:"ts,omitempty"`
    	Data    json.RawMessage `json:"data,omitempty"`
    }
    ```
    (pong stays `Message{Type: TypePong}` → `{"type":"pong"}`.)
  - `Identity` gets, after `EMLyVersion`:
    ```go
    	// Protocol and Capabilities are the v2 negotiation (spec §4). Zero
    	// Protocol keeps the payload byte-identical to v1.
    	Protocol     int      `json:"protocol,omitempty"`
    	Capabilities []string `json:"capabilities,omitempty"`
    ```
  - Add the `Handler` interface (doc: "called from the connection's goroutine for Welcome/Notify, on a fresh goroutine per Command; must not block Welcome/Notify for long") and a `Handler Handler` field on `Client`.
  - In `Run`, right after `dial`: `conn.SetReadLimit(int64(DefaultLimits.MaxMessageBytes) + 1024)` (slack for the envelope; the server enforces the exact value on what it receives).
  - `heartbeat` gets a local `var sess *Session` and `cmdCtx, cancelCmds := context.WithCancel(ctx); defer cancelCmds()`; add cases:
    ```go
    		case TypeWelcome:
    			if c.Handler == nil || sess != nil {
    				continue
    			}
    			var w Welcome
    			if err := json.Unmarshal(msg.Data, &w); err != nil || w.Protocol < ProtocolV2 {
    				c.logf("presence channel ignoring an unusable welcome: %s", msg.Data)
    				continue
    			}
    			sess = newSession(conn, c.URL, w)
    			c.logf("presence channel switched to protocol v2 (%d capabilities)", len(w.AcceptedCapabilities))
    			c.Handler.Welcome(sess, w)
    		case TypeCommand:
    			if sess == nil {
    				c.logf("presence channel ignoring a command received before welcome")
    				continue
    			}
    			var cmd Command
    			if err := json.Unmarshal(msg.Data, &cmd); err != nil {
    				c.logf("presence channel ignoring a malformed command: %v", err)
    				continue
    			}
    			go c.Handler.Command(cmdCtx, sess, msg, cmd)
    		case TypeNotify:
    			if sess == nil {
    				continue
    			}
    			var n Notify
    			if err := json.Unmarshal(msg.Data, &n); err == nil {
    				c.Handler.Notify(sess, msg, n)
    			}
    ```
    Put the constants `TypeHello`… where they are and keep the unknown-type default.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/wsclient/ -v`
Expected: PASS, including every pre-existing test (v1 behaviour).

- [ ] **Step 6: Commit**

```bash
git add internal/wsclient
git commit -m "feat(wsclient): protocol v2 negotiation, command/notify dispatch and session writers"
```

---

### Task 3: `clientWs.commands` in the policy document

**Files:**
- Modify: `internal/policy/document.go`, `internal/policy/parse.go`, `internal/policy/legacy.go`
- Modify: `testdata/remoteconfig/defaults.json`, `testdata/remoteconfig/valid/full.json`
- Create: `testdata/remoteconfig/invalid/clientws-unknown-command.json`, `.problems.json`
- Test: `internal/policy/clientws_test.go` (new)

**Interfaces:**
- Produces: `type ClientWSSettings struct{Enabled bool; Commands []string}` (json `enabled`, `commands`); `func (c ClientWSSettings) Allows(name string) bool`; `var DefaultClientWSCommands []string`; `Document.ClientWS` and `Defaults.ClientWS` become `ClientWSSettings`.

- [ ] **Step 1: Write the failing test**

```go
package policy

import (
	"testing"
)

func TestClientWSCommandsDefaultWhenAbsent(t *testing.T) {
	p, problems := Parse([]byte(`{"schemaVersion":1,"revision":1,"servers":{"api":"https://api.example.test"},"defaultServer":"api","clientWs":{"enabled":true}}`), fixtureDefaults(t))
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	c := p.Global.ClientWS
	if !c.Enabled || !c.Allows("machine.info") || c.Allows("machine.reboot") {
		t.Fatalf("ClientWS = %+v", c)
	}
}

func TestClientWSCommandsReplaceTheDefault(t *testing.T) {
	p, problems := Parse([]byte(`{"schemaVersion":1,"revision":1,"servers":{"api":"https://api.example.test"},"defaultServer":"api","clientWs":{"enabled":true,"commands":["machine.reboot"]}}`), fixtureDefaults(t))
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	c := p.Global.ClientWS
	if !c.Allows("machine.reboot") || c.Allows("machine.info") {
		t.Fatalf("ClientWS = %+v (arrays replace, they do not merge)", c)
	}
}

func TestLegacyPolicyAllowsOnlyReadOnlyCommands(t *testing.T) {
	d := fixtureDefaults(t)
	if d.ClientWS.Enabled || d.ClientWS.Allows("service.restart") || !d.ClientWS.Allows("apps.list_upgradable") {
		t.Fatalf("defaults ClientWS = %+v", d.ClientWS)
	}
}
```

Check the minimal document against the existing `valid/minimal.json`; if `Parse` needs more keys, build these documents from that fixture instead. The invalid fixture is picked up by the existing fixture-walking test.

Run: `go test ./internal/policy/ -run ClientWS -v` — Expected: FAIL (`Allows` undefined).

- [ ] **Step 2: Implement**

`document.go` — replace `ClientWS Toggle` with `ClientWS ClientWSSettings` (keep the comment, add a paragraph on `commands`), and add:

```go
// ClientWSSettings is the clientWs section. Commands is the allowlist of
// protocol v2 commands this machine executes (CLIENT_WS_PROTOCOL.md §12.3).
// It is an array, so a document that sets it replaces the default whole -
// merge patch never merges arrays - and a document that omits it keeps
// DefaultClientWSCommands: read-only verbs only, so the two that restart
// the service or the PC need a document that names them.
type ClientWSSettings struct {
	Enabled  bool     `json:"enabled"`
	Commands []string `json:"commands"`
}

func (c ClientWSSettings) Allows(name string) bool { return slices.Contains(c.Commands, name) }

// DefaultClientWSCommands are the read-only commands.
var DefaultClientWSCommands = []string{"machine.info", "emly.manifest.check", "updater.manifest.check", "apps.list_upgradable"}

// knownClientWSCommands mirrors wsclient.CommandNames; policy must not
// import wsclient, so internal/service pins the two equal in a test.
var knownClientWSCommands = []string{"machine.info", "emly.manifest.check", "updater.manifest.check",
	"apps.list_upgradable", "service.restart", "machine.reboot"}

// KnownClientWSCommands is exported for that test.
func KnownClientWSCommands() []string { return slices.Clone(knownClientWSCommands) }
```

`parse.go` — `Defaults.ClientWS` becomes `ClientWSSettings`; in `validateDocument` add:

```go
	for i, name := range d.ClientWS.Commands {
		if !slices.Contains(knownClientWSCommands, name) {
			add(fmt.Sprintf("/clientWs/commands/%d", i), fmt.Sprintf("unknown command %q", name))
		}
	}
```

`legacy.go` — both places that build `ClientWS` (`DefaultsFromConfig` ~line 55 and `FromLegacy` ~line 132) use `ClientWSSettings{Enabled: false, Commands: slices.Clone(DefaultClientWSCommands)}` / `d.ClientWS`.

Fix every compile error (`grep -rn "ClientWS" internal`) — `internal/service/clientws.go` only reads `.Enabled`, unchanged.

- [ ] **Step 3: Fixtures** — copy from the API branch `feat/client-ws-v2` (API plan Task 3), byte for byte:

```bash
API=../emly-go-api/testdata/remoteconfig
cp $API/valid/full.json testdata/remoteconfig/valid/full.json
cp $API/invalid/clientws-unknown-command.json $API/invalid/clientws-unknown-command.problems.json testdata/remoteconfig/invalid/
git diff --stat testdata/remoteconfig/valid/full.json   # only the clientWs block must change
```

If `full.json` differs in more than the `clientWs` block, the two repos had already drifted: stop and reconcile by hand (take this repo's file, apply only the `clientWs` change, and copy the result back to the API). Then add to `testdata/remoteconfig/defaults.json`, after `logging`:

```json
  "clientWs": {
    "enabled": false,
    "commands": ["machine.info", "emly.manifest.check", "updater.manifest.check", "apps.list_upgradable"]
  },
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/policy/ ./internal/service/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/policy testdata/remoteconfig internal/service
git commit -m "feat(policy): clientWs.commands allowlist, read-only commands by default"
```

---

### Task 4: `state.json` pending commands

**Files:**
- Modify: `internal/state/state.go`
- Test: `internal/state/state_test.go`

**Interfaces:**
- Produces: `type PendingCommand struct{ID, Name string; AcceptedAt time.Time}` (json `id`, `name`, `acceptedAt`); `State.PendingCommands []PendingCommand \`json:"pendingCommands,omitempty"\``; `(*Store).AddPendingCommand(PendingCommand) error`, `(*Store).RemovePendingCommand(id string) error`, `(*Store).TakePendingCommands() ([]PendingCommand, error)`.

- [ ] **Step 1: Write the failing test**

```go
func TestPendingCommandsSurviveAndAreTakenOnce(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "state.json")}
	if err := s.SetPending(&Pending{Version: "3.5.0"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)
	_ = s.AddPendingCommand(PendingCommand{ID: "A", Name: "machine.reboot", AcceptedAt: now})
	_ = s.AddPendingCommand(PendingCommand{ID: "B", Name: "service.restart", AcceptedAt: now})
	_ = s.RemovePendingCommand("B")

	got, err := s.TakePendingCommands()
	if err != nil || len(got) != 1 || got[0].ID != "A" {
		t.Fatalf("take = %+v, %v", got, err)
	}
	if again, _ := s.TakePendingCommands(); len(again) != 0 {
		t.Fatalf("second take = %+v", again)
	}
	st, _ := s.Load()
	if st.Pending == nil || st.Pending.Version != "3.5.0" {
		t.Fatalf("pending update lost: %+v", st.Pending)
	}
}
```

Run: `go test ./internal/state/ -run PendingCommands -v` — Expected: FAIL.

- [ ] **Step 2: Implement** — all three through `Store.update` (the read-modify-write the file already uses, see the "state.json holds two independent lifecycles" rule in AGENTS.md; this is a third one):

```go
// PendingCommand is a service.restart or machine.reboot this service
// accepted over the client channel. Those verbs kill the process that
// accepted them, so the ID is written here first and reported in
// service.started by the next start (CLIENT_WS_PROTOCOL.md §8.6).
type PendingCommand struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	AcceptedAt time.Time `json:"acceptedAt"`
}

func (s *Store) AddPendingCommand(c PendingCommand) error {
	return s.update(func(st *State) { st.PendingCommands = append(st.PendingCommands, c) })
}

func (s *Store) RemovePendingCommand(id string) error {
	return s.update(func(st *State) {
		st.PendingCommands = slices.DeleteFunc(st.PendingCommands, func(c PendingCommand) bool { return c.ID == id })
	})
}

// TakePendingCommands returns the recorded commands and clears them.
func (s *Store) TakePendingCommands() ([]PendingCommand, error) {
	var taken []PendingCommand
	err := s.update(func(st *State) {
		taken, st.PendingCommands = st.PendingCommands, nil
	})
	return taken, err
}
```

Add the field to `State` after `SelfUpdate`.

- [ ] **Step 3: Run tests** — `go test ./internal/state/ -v` → PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/state
git commit -m "feat(state): persist accepted restart/reboot command ids across the restart"
```

---

### Task 5: `machineinfo` system facts and network interfaces

**Files:**
- Create: `internal/machineinfo/netif.go`, `internal/machineinfo/netif_test.go`
- Create: `internal/machineinfo/system.go`

**Interfaces:**
- Produces:

```go
type NetInterface struct { Name, MAC string; IPv4, IPv6 []string }
func NetworkInterfaces() []NetInterface
func interfacesFrom(ifs []net.Interface, addrs func(net.Interface) ([]net.Addr, error)) []NetInterface
type SystemFacts struct { BootTime time.Time; CPU string; Cores int; MemoryMB uint64; SystemDriveTotalMB, SystemDriveFreeMB uint64 }
func CollectSystemFacts(now time.Time) SystemFacts
```

- [ ] **Step 1: Write the failing test** (`netif_test.go`, pure Go)

```go
package machineinfo

import (
	"net"
	"testing"
)

func TestInterfacesFromSkipsLoopbackAndDown(t *testing.T) {
	mac, _ := net.ParseMAC("a4:bb:6d:12:34:56")
	ifs := []net.Interface{
		{Index: 1, Name: "Loopback", Flags: net.FlagUp | net.FlagLoopback},
		{Index: 2, Name: "Ethernet", Flags: net.FlagUp, HardwareAddr: mac},
		{Index: 3, Name: "Wi-Fi", Flags: 0},
	}
	addrs := func(i net.Interface) ([]net.Addr, error) {
		if i.Name != "Ethernet" {
			return nil, nil
		}
		_, v4, _ := net.ParseCIDR("172.16.96.42/24")
		v4.IP = net.ParseIP("172.16.96.42")
		_, v6, _ := net.ParseCIDR("fe80::1/64")
		v6.IP = net.ParseIP("fe80::1")
		return []net.Addr{v4, v6}, nil
	}
	got := interfacesFrom(ifs, addrs)
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	e := got[0]
	if e.Name != "Ethernet" || e.MAC != "A4:BB:6D:12:34:56" || len(e.IPv4) != 1 || e.IPv4[0] != "172.16.96.42" || len(e.IPv6) != 1 {
		t.Fatalf("got %+v", e)
	}
}
```

Run: `go test ./internal/machineinfo/ -run InterfacesFrom -v` — Expected: FAIL.

- [ ] **Step 2: Implement `netif.go`**

```go
package machineinfo

import (
	"net"
	"strings"
)

// NetInterface is one up, non-loopback adapter for machine.info
// (CLIENT_WS_PROTOCOL.md §6.5).
type NetInterface struct {
	Name string   `json:"name"`
	MAC  string   `json:"mac,omitempty"`
	IPv4 []string `json:"ipv4"`
	IPv6 []string `json:"ipv6"`
}

func NetworkInterfaces() []NetInterface {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	return interfacesFrom(ifs, func(i net.Interface) ([]net.Addr, error) { return i.Addrs() })
}

func interfacesFrom(ifs []net.Interface, addrs func(net.Interface) ([]net.Addr, error)) []NetInterface {
	var out []NetInterface
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		n := NetInterface{Name: i.Name, MAC: strings.ToUpper(i.HardwareAddr.String()), IPv4: []string{}, IPv6: []string{}}
		list, _ := addrs(i)
		for _, a := range list {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if v4 := ipnet.IP.To4(); v4 != nil {
				n.IPv4 = append(n.IPv4, v4.String())
			} else {
				n.IPv6 = append(n.IPv6, ipnet.IP.String())
			}
		}
		out = append(out, n)
	}
	return out
}
```

- [ ] **Step 3: Implement `system.go`** (Windows API; no unit test — verified manually in Task 11)

```go
package machineinfo

import (
	"os"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// SystemFacts are the hardware/uptime facts of machine.info. Every field
// is best-effort: zero means "could not read it", and the caller omits it.
type SystemFacts struct {
	BootTime           time.Time
	CPU                string
	Cores              int
	MemoryMB           uint64
	SystemDriveTotalMB uint64
	SystemDriveFreeMB  uint64
}

var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGetTickCount64       = kernel32.NewProc("GetTickCount64")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

// memoryStatusEx mirrors MEMORYSTATUSEX.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func CollectSystemFacts(now time.Time) SystemFacts {
	f := SystemFacts{Cores: runtime.NumCPU()}

	if r, _, _ := procGetTickCount64.Call(); r != 0 {
		f.BootTime = now.Add(-time.Duration(r) * time.Millisecond).UTC().Truncate(time.Second)
	}

	if k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE); err == nil {
		if s, _, err := k.GetStringValue("ProcessorNameString"); err == nil {
			f.CPU = strings.TrimSpace(s)
		}
		k.Close()
	}

	m := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r != 0 {
		f.MemoryMB = m.TotalPhys / (1 << 20)
	}

	drive := os.Getenv("SystemDrive")
	if drive == "" {
		drive = "C:"
	}
	if p, err := windows.UTF16PtrFromString(drive + `\`); err == nil {
		var free, total, totalFree uint64
		if windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree) == nil {
			f.SystemDriveTotalMB, f.SystemDriveFreeMB = total/(1<<20), free/(1<<20)
		}
	}
	return f
}
```

(`golang.org/x/sys/windows/registry` is part of the already-required `golang.org/x/sys` module.)

- [ ] **Step 4: Run** — `go build ./... && go test ./internal/machineinfo/ -v` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/machineinfo
git commit -m "feat(machineinfo): boot time, cpu, memory, system drive and network interfaces"
```

---

### Task 6: Service ⇄ channel plumbing — capabilities, `emit`, buffer, `service.started`, `machine.info`, `session.changed`

**Files:**
- Create: `internal/service/clientevents.go`, `internal/service/clientevents_test.go`
- Modify: `internal/service/service.go` (fields), `internal/service/clientws.go` (identity + handler), `internal/service/sessionwatch.go` (replace the TODO), `internal/service/selfupdate.go` (remember the landed record)

**Interfaces:**
- Consumes: Tasks 1–5.
- Produces:

```go
func (u *Updater) capabilities() []string
func (u *Updater) emit(name string, payload any)
type clientHandler struct{ u *Updater }            // implements wsclient.Handler
func (u *Updater) machineInfo(sections []string) machineInfoPayload
func sessionChangedPayload(kinds []machineinfo.SessionChangeKind, c machineinfo.SessionChange, changed bool) wsclient.SessionChanged
// Updater fields:
wsSession    atomic.Pointer[wsclient.Session]
eventsMu     sync.Mutex
eventBuf     []bufferedEvent
startedSent  atomic.Bool
selfLanded   atomic.Pointer[state.SelfUpdate] // set by reconcileSelfUpdate on OutcomeLanded; read once in service.started
emitFn       func(name string, payload any) // test seam
systemFactsFn func(time.Time) machineinfo.SystemFacts // test seam
```

`Command` and `Notify` of `clientHandler` call `u.executeCommand` (Task 7) and `u.handleNotify` (Task 10); in this task they are one-line stubs that log at Debug, replaced later.

- [ ] **Step 1: Write the failing tests** (`clientevents_test.go`)

```go
package service

import (
	"testing"
	"time"

	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/policy"
	"emlyupdater/internal/state"
	"emlyupdater/internal/wsclient"
)

func TestCapabilitiesDeclareEverythingThisBuildImplements(t *testing.T) {
	u := &Updater{}
	got := map[string]bool{}
	for _, c := range u.capabilities() {
		got[c] = true
	}
	for _, list := range [][]string{wsclient.CommandNames, wsclient.EventNames, wsclient.TopicNames} {
		for _, n := range list {
			if !got[n] {
				t.Errorf("capability %s missing", n)
			}
		}
	}
}

// policy keeps its own copy of the command names; pin it to the wire list.
func TestPolicyKnowsExactlyTheWireCommands(t *testing.T) {
	known := policy.KnownClientWSCommands()
	if len(known) != len(wsclient.CommandNames) {
		t.Fatalf("policy %v vs wire %v", known, wsclient.CommandNames)
	}
	for i := range known {
		if known[i] != wsclient.CommandNames[i] {
			t.Fatalf("policy %v vs wire %v", known, wsclient.CommandNames)
		}
	}
}

func TestEventsBufferedUntilWelcome(t *testing.T) {
	u := &Updater{Log: testLogger(t)}
	u.emit(wsclient.EvtUpdateApplied, map[string]string{"target": "updater"})
	u.emit(wsclient.EvtUpdateStarted, map[string]string{"target": "emly"})
	var sent []string
	u.flushEvents(func(name string, _ any) error { sent = append(sent, name); return nil })
	if len(sent) != 2 || sent[0] != wsclient.EvtUpdateApplied {
		t.Fatalf("flushed %v", sent)
	}
	u.flushEvents(func(name string, _ any) error { t.Fatal("buffer not emptied"); return nil })
}

func TestEventBufferDropsOldest(t *testing.T) {
	u := &Updater{Log: testLogger(t)}
	for i := 0; i < eventBufferSize+5; i++ {
		u.emit(wsclient.EvtSessionChanged, i)
	}
	var first any
	n := 0
	u.flushEvents(func(_ string, p any) error {
		if n == 0 {
			first = p
		}
		n++
		return nil
	})
	if n != eventBufferSize || first != 5 {
		t.Fatalf("n=%d first=%v", n, first)
	}
}

func TestSessionChangedPayload(t *testing.T) {
	at := time.Date(2026, 9, 23, 8, 20, 29, 450e6, time.UTC)
	c := machineinfo.SessionChange{Kind: machineinfo.SessionUnlock, SessionID: 2, At: at, SessionUser: `CORP\m.rossi`,
		LoggedUser: machineinfo.UserSession{User: `CORP\m.rossi`, State: machineinfo.SessionActiveRDP}}
	p := sessionChangedPayload([]machineinfo.SessionChangeKind{machineinfo.SessionRemoteConnect, machineinfo.SessionUnlock}, c, true)
	if len(p.Events) != 2 || p.Events[0] != "remote-connect" || p.SessionID != 2 || !p.Changed ||
		p.LoggedUser == nil || p.LoggedUser.State != "active-rdp" || p.At != "2026-09-23T08:20:29.450Z" {
		t.Fatalf("payload = %+v", p)
	}
	nobody := sessionChangedPayload(nil, machineinfo.SessionChange{Kind: machineinfo.SessionLogoff}, true)
	if nobody.LoggedUser != nil {
		t.Fatalf("nobody logged on must omit logged_user: %+v", nobody)
	}
}

func TestServiceStartedReason(t *testing.T) {
	boot := time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		cmds   []state.PendingCommand
		landed *state.SelfUpdate
		start  time.Time
		want   string
	}{
		{"restart command", []state.PendingCommand{{Name: wsclient.CmdServiceRestart}}, nil, boot.Add(time.Hour), "command"},
		{"reboot command", []state.PendingCommand{{Name: wsclient.CmdMachineReboot}}, nil, boot.Add(time.Minute), "boot"},
		{"self update", nil, &state.SelfUpdate{FromVersion: "1.8.2"}, boot.Add(time.Hour), "self_update"},
		{"fresh boot", nil, nil, boot.Add(3 * time.Minute), "boot"},
		{"plain restart", nil, nil, boot.Add(3 * time.Hour), "unknown"},
	}
	for _, c := range cases {
		if got := serviceStartedReason(c.cmds, c.landed, boot, c.start); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}
```

(`testLogger` already exists in the package's tests — `sessionwatch_test.go` uses it.)

Run: `go test ./internal/service/ -run "Capabilities|PolicyKnows|EventBuffer|EventsBuffered|SessionChangedPayload|ServiceStartedReason" -v` — Expected: FAIL.

- [ ] **Step 2: Implement `clientevents.go`**

```go
package service

import (
	"context"
	"slices"
	"time"

	"emlyupdater/internal/machineinfo"
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
	p := wsclient.ServiceStarted{
		Reason:         serviceStartedReason(cmds, u.selfLanded, facts.BootTime, u.startedAt),
		StartedAt:      u.startedAt.UTC().Format(time.RFC3339),
		UpdaterVersion: version.Version,
	}
	if !facts.BootTime.IsZero() {
		p.BootTime = facts.BootTime.Format(time.RFC3339)
	}
	if u.selfLanded != nil && u.selfLanded.FromVersion != version.Version {
		p.PreviousVersion = u.selfLanded.FromVersion
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
```

Add `startedAt time.Time` to `Updater` too (set in `New` to `time.Now()`).

- [ ] **Step 3: The `machine.info` builder** (same file)

```go
type machineInfoPayload struct {
	HWID           string                     `json:"hwid,omitempty"`
	Hostname       string                     `json:"hostname,omitempty"`
	ADDomain       string                     `json:"ad_domain,omitempty"`
	Serial         string                     `json:"serial,omitempty"`
	Product        string                     `json:"product,omitempty"`
	OSVersion      string                     `json:"os_version,omitempty"`
	EMLyVersion    string                     `json:"emly_version,omitempty"`
	UpdaterVersion string                     `json:"updater_version,omitempty"`
	LoggedUser     *wsclient.UserSession      `json:"logged_user,omitempty"`
	BootTime       string                     `json:"boot_time,omitempty"`
	UptimeSeconds  int64                      `json:"uptime_seconds,omitempty"`
	Network        *networkInfo               `json:"network,omitempty"`
	Site           *siteInfo                  `json:"site,omitempty"`
	Config         *configInfo                `json:"config,omitempty"`
	Hardware       *hardwareInfo              `json:"hardware,omitempty"`
	EMLy           *emlyInfo                  `json:"emly,omitempty"`
	PendingUpdate  *wsclient.PendingInfo      `json:"pending_update,omitempty"`
	SelfUpdate     *selfUpdateInfo            `json:"self_update,omitempty"`
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
```

Add the seams `netInterfacesFn func() []machineinfo.NetInterface` and `emlyRunningFn func() bool` to `Updater`, and the `process` import.

Add a test for the builder's section filter:

```go
func TestMachineInfoSectionsFilter(t *testing.T) {
	u := newClientTestUpdater(t)
	p := u.machineInfo([]string{"network"})
	if p.Network == nil || p.Hardware != nil || p.HWID != "" || p.EMLy != nil {
		t.Fatalf("payload = %+v", p)
	}
}
```

with a helper, reused by Tasks 7–10:

```go
// newClientTestUpdater builds an Updater on the legacy policy (remote
// config off, see newTestUpdater), with every Windows-touching seam stubbed,
// a temp state file, EMLy not installed, and one cycle already run so
// u.cur is set.
func newClientTestUpdater(t *testing.T) *Updater {
	t.Helper()
	cfg := internalCfg(t, "internal")
	cfg.EMLyConfigFile = filepath.Join(t.TempDir(), "missing", "config.ini")
	u := newTestUpdater(t, cfg, dcNamed("DC-RM2"), ipsOf("172.16.96.50"))
	u.Store = &state.Store{Path: filepath.Join(t.TempDir(), "state.json")}
	u.loggedUserFn = func() machineinfo.UserSession { return machineinfo.UserSession{} }
	u.systemFactsFn = func(time.Time) machineinfo.SystemFacts { return machineinfo.SystemFacts{Cores: 4} }
	u.netInterfacesFn = func() []machineinfo.NetInterface { return []machineinfo.NetInterface{{Name: "Ethernet"}} }
	u.emlyRunningFn = func() bool { return false }
	u.startedAt = time.Now()
	u.cur.Store(u.beginCycle(context.Background(), false))
	return u
}
```

(`internalCfg`, `newTestUpdater`, `dcNamed`, `ipsOf`, `testLogger` are the existing helpers in `sourcepolicy_test.go`. If `beginCycle` already stores `u.cur` itself, the explicit `Store` is harmless.)

- [ ] **Step 4: Wire it**
  - `service.go` `Updater` struct: add the fields listed in **Interfaces** plus `startedAt`, `netInterfacesFn`, `emlyRunningFn`; `New` sets `startedAt: time.Now()`.
  - `clientws.go` `runClientWS`: build the identity as

    ```go
    		identity := u.clientWSIdentity()
    		identity.Protocol = wsclient.ProtocolV2
    		identity.Capabilities = u.capabilities()
    ```

    set `Handler: clientHandler{u}` on the `wsclient.Client`, and right after `err := client.Run(connCtx)` add `u.wsSession.Store(nil)`.
  - `selfupdate.go` `reconcileSelfUpdate`, case `OutcomeLanded`: before clearing the record, `landed := *rec; u.selfLanded.Store(&landed)`. The field is `selfLanded atomic.Pointer[state.SelfUpdate]` (not a plain pointer: it is written on the poll goroutine and read on the WS goroutine), so in `serviceStarted` read it once with `landed := u.selfLanded.Load()` and use `landed` where the code above says `u.selfLanded`.
  - `sessionwatch.go`: replace the whole `TODO(presence WS)` comment block with

    ```go
			// Push the result over the client channel (CLIENT_WS_PROTOCOL.md
			// §8.1). Not a source of truth: with the channel down the event is
			// buffered, and the next poll's X-EMLy-LoggedUser* headers carry
			// the same value anyway.
			u.emit(wsclient.EvtSessionChanged, sessionChangedPayload(kinds, got, changed))
    ```

    and move `kinds = nil` **after** that line (the payload needs the burst's kinds).
  - `clientHandler.Command`/`Notify` call `u.executeCommand` / `u.handleNotify`: add temporary stubs in `clientcmd.go` / `clientnotify.go` so the package compiles:

    ```go
    func (u *Updater) executeCommand(ctx context.Context, s *wsclient.Session, msg wsclient.Message, cmd wsclient.Command) {
    	u.Log.Debug("client command received, not implemented yet", "name", cmd.Name)
    }
    func (u *Updater) handleNotify(n wsclient.Notify) { u.Log.Debug("client notify received", "topic", n.Topic) }
    ```

- [ ] **Step 5: Update the session watcher test** — in `sessionwatch_test.go`'s `newSessionWatchUpdater`, set `emitFn: func(string, any) {}` so the watcher does not try to buffer through a nil logger path; add one assertion test:

```go
func TestWatchSessionsEmitsSessionChanged(t *testing.T) {
	user := machineinfo.UserSession{User: `TREGCC\mrossi`, State: machineinfo.SessionActiveConsole}
	u, out := newSessionWatchUpdater(t, &user)
	got := make(chan wsclient.SessionChanged, 1)
	u.emitFn = func(name string, p any) {
		if name == wsclient.EvtSessionChanged {
			got <- p.(wsclient.SessionChanged)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go u.watchSessions(ctx)
	u.queueSessionChange(machineinfo.SessionChange{Kind: machineinfo.SessionLogon, SessionID: 1})
	u.queueSessionChange(machineinfo.SessionChange{Kind: machineinfo.SessionUnlock, SessionID: 1})
	waitResolved(t, out)
	p := <-got
	if len(p.Events) != 2 || p.Events[1] != "unlock" || !p.Changed || p.LoggedUser.User != `TREGCC\mrossi` {
		t.Fatalf("payload = %+v", p)
	}
}
```

- [ ] **Step 6: Run** — `go build ./... && go test ./internal/service/ ./internal/wsclient/ -v` → PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/service
git commit -m "feat(service): v2 capabilities, event emitter with buffer, service.started, machine.info and session.changed"
```

---

### Task 7: Command execution — admission, dedupe, read-only verbs

**Files:**
- Modify: `internal/service/clientcmd.go` (replace the stub)
- Create: `internal/service/clientcmd_test.go`
- Modify: `internal/logging/logging.go` (events 924/925)

**Interfaces:**
- Consumes: Tasks 1–6; `winget.ListUpgradable` (merged branch); `resolveTarget`, `resolveUpdaterManifest`, `selfupdate.Decide`.
- Produces:

```go
func (u *Updater) executeCommand(ctx context.Context, s commandSession, msg wsclient.Message, cmd wsclient.Command)
type commandSession interface { Secure() bool; Ack(context.Context, string, wsclient.Ack) error; Result(context.Context, string, wsclient.Result) error; Close(websocket.StatusCode, string) }
type commandRing struct{...}  // 64 ids
func (u *Updater) emlyManifestCheck(ctx context.Context, cyc *cycleState) wsclient.ManifestCheck
func (u *Updater) updaterManifestCheck(ctx context.Context, cyc *cycleState) wsclient.ManifestCheck
func (u *Updater) listUpgradable(ctx context.Context) (any, *wsclient.ErrorBody, bool /*truncated*/)
// seams: listUpgradableFn func(context.Context) ([]winget.Package, error); emlyCheckFn/updaterCheckFn for tests
logging.EventClientCommand = 924, logging.EventClientCommandRefused = 925
```

`executeCommand` takes the `commandSession` interface (satisfied by `*wsclient.Session`) so tests can use a fake; change `clientHandler.Command` to pass `s` as that interface.

Admission, in order (first failure → `ack accepted:false` + event 925 at Warn, stop):
1. seen in ring → `ack {accepted:true, duplicate:true}`, then re-send the stored result if any; stop (not a refusal, no 925).
2. `wsclient.ValidateArgs` → its error.
3. expired: `expires_at` parses and is before `u.clock()`, **and** `|msg.TS − u.clock()| < 5m` (skip the check when the clocks disagree more than that or `ts` is missing — spec §5.1) → `expired`.
4. `!cyc.eff.Doc.ClientWS.Allows(name)` (no cycle yet → refuse) → `disabled_by_policy`.
5. `wsclient.Destructive(name) && !s.Secure()` → `insecure_transport`.
6. another command with the same name running → `busy`.
7. verb-specific admission (Task 8: reboot's `user_active`, destructive `busy` while installing).

Then `ack accepted:true` (+ event 924: Info for read-only verbs, `WarnEvent` to the Event Log for destructive ones, fields `name`, `id`, `issuedBy`), run, and for non-destructive verbs send `result` and store it in the ring.

- [ ] **Step 1: Write the failing tests** (`clientcmd_test.go`)

```go
package service

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"emlyupdater/internal/winget"
	"emlyupdater/internal/wsclient"
)

type fakeSession struct {
	secure  bool
	mu      sync.Mutex
	acks    []wsclient.Ack
	results []wsclient.Result
	closed  websocket.StatusCode
}

func (f *fakeSession) Secure() bool { return f.secure }
func (f *fakeSession) Ack(_ context.Context, _ string, a wsclient.Ack) error {
	f.mu.Lock(); defer f.mu.Unlock(); f.acks = append(f.acks, a); return nil
}
func (f *fakeSession) Result(_ context.Context, _ string, r wsclient.Result) error {
	f.mu.Lock(); defer f.mu.Unlock(); f.results = append(f.results, r); return nil
}
func (f *fakeSession) Close(code websocket.StatusCode, _ string) { f.closed = code }

func cmdMsg(name string, args string) (wsclient.Message, wsclient.Command) {
	now := time.Now().UTC()
	msg := wsclient.Message{Type: wsclient.TypeCommand, ID: wsclient.NewID(), TS: now.Format(time.RFC3339)}
	return msg, wsclient.Command{Name: name, Args: json.RawMessage(args), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}
}

// withPolicy installs a fresh cycle whose clientWs allows cmds.
func withPolicy(t *testing.T, u *Updater, cmds ...string) {
	t.Helper()
	cyc := u.beginCycle(context.Background(), false)
	cyc.eff.Doc.ClientWS.Enabled = true
	cyc.eff.Doc.ClientWS.Commands = cmds
	u.cur.Store(cyc)
}

func TestExecuteCommandMachineInfo(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdMachineInfo)
	s := &fakeSession{}
	msg, cmd := cmdMsg(wsclient.CmdMachineInfo, `{"sections":["network"]}`)
	u.executeCommand(context.Background(), s, msg, cmd)
	if len(s.acks) != 1 || !s.acks[0].Accepted || len(s.results) != 1 || s.results[0].Status != wsclient.ResultOK {
		t.Fatalf("acks=%+v results=%+v", s.acks, s.results)
	}
	if !strings.Contains(string(s.results[0].Payload), `"Ethernet"`) {
		t.Fatalf("payload = %s", s.results[0].Payload)
	}
}

func TestExecuteCommandRefusals(t *testing.T) {
	cases := []struct {
		name, args string
		allowed    []string
		secure     bool
		expired    bool
		want       string
	}{
		{wsclient.CmdMachineInfo, `{"sections":["gpu"]}`, []string{wsclient.CmdMachineInfo}, false, false, wsclient.ErrInvalidArgs},
		{"machine.format_disk", `{}`, nil, false, false, wsclient.ErrUnsupportedCommand},
		{wsclient.CmdMachineInfo, ``, nil, false, false, wsclient.ErrDisabledByPolicy},
		{wsclient.CmdMachineInfo, ``, []string{wsclient.CmdMachineInfo}, false, true, wsclient.ErrExpired},
		{wsclient.CmdServiceRestart, ``, []string{wsclient.CmdServiceRestart}, false, false, wsclient.ErrInsecureTransport},
	}
	for _, c := range cases {
		u := newClientTestUpdater(t)
		withPolicy(t, u, c.allowed...)
		s := &fakeSession{secure: c.secure}
		msg, cmd := cmdMsg(c.name, c.args)
		if c.expired {
			cmd.ExpiresAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
		}
		u.executeCommand(context.Background(), s, msg, cmd)
		if len(s.acks) != 1 || s.acks[0].Accepted || s.acks[0].Error.Code != c.want || len(s.results) != 0 {
			t.Errorf("%s: acks=%+v results=%+v, want refusal %s", c.name, s.acks, s.results, c.want)
		}
	}
}

// Clocks that disagree by more than 5 minutes cannot judge expiry.
func TestExpiryIgnoredWhenClocksDisagree(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdMachineInfo)
	s := &fakeSession{}
	msg, cmd := cmdMsg(wsclient.CmdMachineInfo, ``)
	msg.TS = time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	cmd.ExpiresAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	u.executeCommand(context.Background(), s, msg, cmd)
	if len(s.acks) != 1 || !s.acks[0].Accepted {
		t.Fatalf("acks = %+v", s.acks)
	}
}

func TestExecuteCommandDedupes(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdMachineInfo)
	s := &fakeSession{}
	msg, cmd := cmdMsg(wsclient.CmdMachineInfo, ``)
	u.executeCommand(context.Background(), s, msg, cmd)
	u.executeCommand(context.Background(), s, msg, cmd)
	if len(s.acks) != 2 || !s.acks[1].Duplicate || len(s.results) != 2 || string(s.results[0].Payload) != string(s.results[1].Payload) {
		t.Fatalf("acks=%+v results=%d", s.acks, len(s.results))
	}
}

func TestCommandRingEvictsOldest(t *testing.T) {
	var r commandRing
	for i := 0; i < commandRingSize+1; i++ {
		r.add(string(rune('A'+i%26)) + time.Duration(i).String())
	}
	if _, seen := r.lookup("A0s"); seen {
		t.Fatal("oldest id survived")
	}
}

func TestBusyWhileSameCommandRuns(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdAppsListUpgradable)
	release := make(chan struct{})
	u.listUpgradableFn = func(ctx context.Context) ([]winget.Package, error) { <-release; return nil, nil }
	s1, s2 := &fakeSession{}, &fakeSession{}
	m1, c1 := cmdMsg(wsclient.CmdAppsListUpgradable, ``)
	m2, c2 := cmdMsg(wsclient.CmdAppsListUpgradable, ``)
	done := make(chan struct{})
	go func() { u.executeCommand(context.Background(), s1, m1, c1); close(done) }()
	time.Sleep(50 * time.Millisecond)
	u.executeCommand(context.Background(), s2, m2, c2)
	close(release)
	<-done
	if s2.acks[0].Accepted || s2.acks[0].Error.Code != wsclient.ErrBusy {
		t.Fatalf("second = %+v", s2.acks)
	}
}

func TestListUpgradableErrorsAndTruncation(t *testing.T) {
	u := newClientTestUpdater(t)
	u.listUpgradableFn = func(context.Context) ([]winget.Package, error) { return nil, winget.ErrModuleNotInstalled }
	if _, e, _ := u.listUpgradable(context.Background()); e == nil || e.Code != wsclient.ErrWingetModuleMissing {
		t.Fatalf("module missing -> %+v", e)
	}
	u.listUpgradableFn = func(context.Context) ([]winget.Package, error) { return nil, winget.ErrPowerShellNotFound }
	if _, e, _ := u.listUpgradable(context.Background()); e == nil || e.Code != wsclient.ErrPowerShellNotFound {
		t.Fatalf("powershell -> %+v", e)
	}
	many := make([]winget.Package, 2000)
	for i := range many {
		many[i] = winget.Package{Name: strings.Repeat("n", 40), ID: "Vendor.App", InstalledVersion: "1.0", Available: "2.0", Source: "winget"}
	}
	u.listUpgradableFn = func(context.Context) ([]winget.Package, error) { return many, nil }
	payload, e, truncated := u.listUpgradable(context.Background())
	b, _ := json.Marshal(payload)
	if e != nil || !truncated || len(b) > resultPayloadBudget {
		t.Fatalf("e=%v truncated=%v size=%d", e, truncated, len(b))
	}
}
```

`cyc.eff.Doc` must be a value the cycle owns (mutating it must not change the stored snapshot). Check `policy.Effective`: if `Doc` is a pointer into the shared snapshot, copy it first (`doc := *cyc.eff.Doc; cyc.eff = &policy.Effective{...copy..., Doc: &doc}`). The manifest checks' network paths are not exercised in unit tests; the seams `emlyCheckFn`/`updaterCheckFn` exist for tests that need a canned `ManifestCheck`.

Run: `go test ./internal/service/ -run "ExecuteCommand|Expiry|CommandRing|Busy|ListUpgradable" -v` — Expected: FAIL.

- [ ] **Step 2: Implement `clientcmd.go`**

```go
package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/coder/websocket"

	"emlyupdater/internal/logging"
	"emlyupdater/internal/manifest"
	"emlyupdater/internal/selfupdate"
	"emlyupdater/internal/source"
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
)

type commandSession interface {
	Secure() bool
	Ack(ctx context.Context, replyTo string, a wsclient.Ack) error
	Result(ctx context.Context, replyTo string, r wsclient.Result) error
	Close(code websocket.StatusCode, reason string)
}

// commandRing remembers the last commandRingSize command IDs and their
// result, so a redelivered command is confirmed, not re-run (spec §5.5).
type commandRing struct {
	mu      sync.Mutex
	order   []string
	results map[string]*wsclient.Result
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
		r.order = r.order[1:]
	}
}

func (r *commandRing) lookup(id string) (*wsclient.Result, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	res, ok := r.results[id]
	return res, ok
}

func (r *commandRing) store(id string, res wsclient.Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.results[id]; ok {
		r.results[id] = &res
	}
}

func refuse(code, msg string) *wsclient.ErrorBody { return &wsclient.ErrorBody{Code: code, Message: msg} }

// executeCommand runs one command end to end: admission, ack, execution,
// result. It is called on its own goroutine per command by wsclient.
func (u *Updater) executeCommand(ctx context.Context, s commandSession, msg wsclient.Message, cmd wsclient.Command) {
	if prev, seen := u.commands.lookup(msg.ID); seen {
		_ = s.Ack(ctx, msg.ID, wsclient.Ack{Accepted: true, Duplicate: true})
		if prev != nil {
			_ = s.Result(ctx, msg.ID, *prev)
		}
		return
	}
	u.commands.add(msg.ID)

	if e := u.admitCommand(s, msg, cmd); e != nil {
		u.Log.WarnEvent(logging.EventClientCommandRefused, "client command refused",
			"name", cmd.Name, "id", msg.ID, "issuedBy", cmd.IssuedBy, "code", e.Code, "reason", e.Message)
		_ = s.Ack(ctx, msg.ID, wsclient.Ack{Accepted: false, Error: e})
		return
	}
	if !u.claimCommand(cmd.Name) {
		_ = s.Ack(ctx, msg.ID, wsclient.Ack{Accepted: false, Error: refuse(wsclient.ErrBusy, cmd.Name+" is already running")})
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

// listUpgradable runs winget (Microsoft.WinGet.Client) and fits the result
// under the frame budget, dropping packages from the end.
func (u *Updater) listUpgradable(ctx context.Context) (any, *wsclient.ErrorBody, bool) {
	ctx, cancel := context.WithTimeout(ctx, wingetTimeout)
	defer cancel()
	list := winget.ListUpgradable
	if u.listUpgradableFn != nil {
		list = u.listUpgradableFn
	}
	pkgs, err := list(ctx)
	switch {
	case errors.Is(err, winget.ErrModuleNotInstalled):
		return nil, refuse(wsclient.ErrWingetModuleMissing, err.Error()), false
	case errors.Is(err, winget.ErrPowerShellNotFound):
		return nil, refuse(wsclient.ErrPowerShellNotFound, err.Error()), false
	case ctx.Err() != nil:
		return nil, refuse(wsclient.ErrTimeout, "winget did not answer in time"), false
	case err != nil:
		return nil, refuse(wsclient.ErrInternal, err.Error()), false
	}
	type payload struct {
		Packages    []wsclient.UpgradablePackage `json:"packages"`
		CollectedAt string                       `json:"collected_at"`
	}
	p := payload{Packages: make([]wsclient.UpgradablePackage, 0, len(pkgs)), CollectedAt: u.clock().UTC().Format(time.RFC3339)}
	for _, k := range pkgs {
		p.Packages = append(p.Packages, wsclient.UpgradablePackage{Name: k.Name, ID: k.ID,
			InstalledVersion: k.InstalledVersion, AvailableVersion: k.Available, Source: k.Source})
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

// emlyManifestCheck is the dry run of spec §7.2: the same manifest
// resolution as a Cycle, stopping before any download. It is read-only
// (no state.json write, no download, no RunLoop wake-up), which is what
// makes it safe to run while a Cycle is in progress.
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
	src, m, target, err := u.resolveTarget(ctx, cyc, emly.Channel)
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
// would log and clear it - and nothing is launched or counted.
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
	_, m, servedBy, err := u.resolveUpdaterManifest(ctx, cyc)
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
```

Imports also include `emlyupdater/internal/state`. `cyc.eff.UpdaterEnabled` takes a `time.Time` (`policy/match.go`).

Add to `Updater`: `commands commandRing`, `runningMu sync.Mutex`, `running map[string]bool`, `listUpgradableFn func(context.Context) ([]winget.Package, error)`, `emlyCheckFn`, `updaterCheckFn func(context.Context, *cycleState) wsclient.ManifestCheck`. Until Task 8 lands, add in `clientpower.go`:

```go
func (u *Updater) admitDestructive(cmd wsclient.Command) *wsclient.ErrorBody { return nil }
func (u *Updater) runDestructive(ctx context.Context, s commandSession, msg wsclient.Message, cmd wsclient.Command) {
	_ = s.Result(ctx, msg.ID, wsclient.Result{Status: wsclient.ResultError, Error: refuse(wsclient.ErrUnsupportedCommand, "not implemented")})
}
```

`logging.go`, next to 920–923:

```go
	EventClientCommand        = 924 // a command received over the client channel was accepted (Event Log only for restart/reboot)
	EventClientCommandRefused = 925 // a command was refused (policy, transport, validation, busy)
```

- [ ] **Step 3: Run** — `go build ./... && go test ./internal/service/ -v` → PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/service internal/logging
git commit -m "feat(service): execute client ws commands - admission, dedupe, machine info, manifest dry runs, winget list"
```

---

### Task 8: Destructive commands — `service.restart`, `machine.reboot`

**Files:**
- Create: `internal/power/power.go`
- Modify: `internal/service/clientpower.go` (replace the stubs), `internal/service/service.go` (`installing` counter in `runSetupAndVerify`)
- Modify: `main.go` (hidden `restart-service`)
- Test: `internal/service/clientpower_test.go`

**Interfaces:**
- Produces: `power.Reboot(delay time.Duration) error`; `Updater.installing atomic.Int32`; seams `restartFn func() error`, `rebootFn func(time.Duration) error`; `main.go` subcommand `restart-service` (stop, then start).

Rules:
- `admitDestructive`: `u.installing.Load() > 0` → `busy`; `machine.reboot` with mode `skip` and `u.loggedUser().State` in {`active-console`, `active-rdp`} → `user_active`.
- `service.restart`: `AddPendingCommand` (failure → `result error internal`, stop) → `s.Close(websocket.StatusGoingAway, "service restarting")` → `restartFn()` (failure → `RemovePendingCommand`, log Error; the connection is already closed, the server times the command out).
- `machine.reboot`: delay = `args.Delay()`, raised to 60s if a user is active → `AddPendingCommand` → `rebootFn(delay)` (failure → `RemovePendingCommand` + `result error internal`). The connection stays open until Windows shuts the service down.

- [ ] **Step 1: Write the failing tests**

```go
package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/websocket"

	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/wsclient"
)

func TestDestructiveRefusedOnInsecureTransport(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdMachineReboot)
	rebooted := false
	u.rebootFn = func(time.Duration) error { rebooted = true; return nil }
	s := &fakeSession{secure: false}
	msg, cmd := cmdMsg(wsclient.CmdMachineReboot, `{}`)
	u.executeCommand(context.Background(), s, msg, cmd)
	cmds, _ := u.Store.TakePendingCommands()
	if rebooted || len(cmds) != 0 || s.acks[0].Error.Code != wsclient.ErrInsecureTransport {
		t.Fatalf("rebooted=%v pending=%v acks=%+v", rebooted, cmds, s.acks)
	}
}

func TestRebootPersistsIDAndRaisesDelayForActiveUser(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdMachineReboot)
	u.loggedUserFn = func() machineinfo.UserSession {
		return machineinfo.UserSession{User: `CORP\u`, State: machineinfo.SessionActiveConsole}
	}
	var got time.Duration
	u.rebootFn = func(d time.Duration) error { got = d; return nil }
	s := &fakeSession{secure: true}
	msg, cmd := cmdMsg(wsclient.CmdMachineReboot, `{"delay_seconds":10}`)
	u.executeCommand(context.Background(), s, msg, cmd)
	cmds, _ := u.Store.TakePendingCommands()
	if got != 60*time.Second || len(cmds) != 1 || cmds[0].ID != msg.ID || len(s.results) != 0 {
		t.Fatalf("delay=%v pending=%+v results=%+v", got, cmds, s.results)
	}
}

func TestRebootSkipWithActiveUserRefused(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdMachineReboot)
	u.loggedUserFn = func() machineinfo.UserSession {
		return machineinfo.UserSession{User: `CORP\u`, State: machineinfo.SessionActiveRDP}
	}
	u.rebootFn = func(time.Duration) error { t.Fatal("rebooted"); return nil }
	s := &fakeSession{secure: true}
	msg, cmd := cmdMsg(wsclient.CmdMachineReboot, `{"when_user_active":"skip"}`)
	u.executeCommand(context.Background(), s, msg, cmd)
	if s.acks[0].Accepted || s.acks[0].Error.Code != wsclient.ErrUserActive {
		t.Fatalf("acks = %+v", s.acks)
	}
}

func TestRebootFailureUnrecordsAndReportsError(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdMachineReboot)
	u.rebootFn = func(time.Duration) error { return errors.New("privilege not held") }
	s := &fakeSession{secure: true}
	msg, cmd := cmdMsg(wsclient.CmdMachineReboot, `{}`)
	u.executeCommand(context.Background(), s, msg, cmd)
	cmds, _ := u.Store.TakePendingCommands()
	if len(cmds) != 0 || len(s.results) != 1 || s.results[0].Error.Code != wsclient.ErrInternal {
		t.Fatalf("pending=%+v results=%+v", cmds, s.results)
	}
}

func TestServiceRestartClosesThenRestarts(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdServiceRestart)
	var closedFirst bool
	s := &fakeSession{secure: true}
	u.restartFn = func() error { closedFirst = s.closed == websocket.StatusGoingAway; return nil }
	msg, cmd := cmdMsg(wsclient.CmdServiceRestart, ``)
	u.executeCommand(context.Background(), s, msg, cmd)
	cmds, _ := u.Store.TakePendingCommands()
	if !closedFirst || len(cmds) != 1 || cmds[0].Name != wsclient.CmdServiceRestart {
		t.Fatalf("closedFirst=%v pending=%+v", closedFirst, cmds)
	}
}

func TestDestructiveBusyWhileInstalling(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdServiceRestart)
	u.installing.Add(1)
	u.restartFn = func() error { t.Fatal("restarted mid-install"); return nil }
	s := &fakeSession{secure: true}
	msg, cmd := cmdMsg(wsclient.CmdServiceRestart, ``)
	u.executeCommand(context.Background(), s, msg, cmd)
	if s.acks[0].Error.Code != wsclient.ErrBusy {
		t.Fatalf("acks = %+v", s.acks)
	}
}
```

Run: `go test ./internal/service/ -run "Destructive|Reboot|ServiceRestart" -v` — Expected: FAIL.

- [ ] **Step 2: `internal/power/power.go`**

```go
// Package power reboots the machine for the client channel's
// machine.reboot command. Windows only; not unit tested (it would reboot
// the test machine) - see AGENTS.md's manual verification list.
package power

import (
	"fmt"
	"time"

	"golang.org/x/sys/windows"
)

// Reboot asks Windows to restart after delay. Windows itself shows the
// countdown to the logged-on users, so there is no dialog of our own.
// Applications are closed without saving when the countdown ends: the user
// was warned for at least the delay.
func Reboot(delay time.Duration) error {
	if err := enableShutdownPrivilege(); err != nil {
		return err
	}
	reason := uint32(windows.SHTDN_REASON_MAJOR_APPLICATION | windows.SHTDN_REASON_MINOR_MAINTENANCE | windows.SHTDN_REASON_FLAG_PLANNED)
	if err := windows.InitiateSystemShutdownEx(nil, nil, uint32(delay/time.Second), true, true, reason); err != nil {
		return fmt.Errorf("InitiateSystemShutdownEx: %w", err)
	}
	return nil
}

// enableShutdownPrivilege enables SeShutdownPrivilege in this process's
// token: LocalSystem holds it, disabled by default.
func enableShutdownPrivilege() error {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token); err != nil {
		return fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer token.Close()
	name, _ := windows.UTF16PtrFromString("SeShutdownPrivilege")
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, name, &luid); err != nil {
		return fmt.Errorf("LookupPrivilegeValue: %w", err)
	}
	tp := windows.Tokenprivileges{PrivilegeCount: 1}
	tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
	if err := windows.AdjustTokenPrivileges(token, false, &tp, 0, nil, nil); err != nil {
		return fmt.Errorf("AdjustTokenPrivileges: %w", err)
	}
	return nil
}
```

(Compare with `enablePrivilege` in `internal/notify/toast_launch.go` and mirror it exactly — same API usage, different privilege name.)

- [ ] **Step 3: `clientpower.go`**

```go
package service

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sys/windows"

	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/power"
	"emlyupdater/internal/state"
	"emlyupdater/internal/wsclient"
)

const minRebootNoticeWithUser = 60 * time.Second

func userActive(s machineinfo.UserSession) bool {
	return s.State == machineinfo.SessionActiveConsole || s.State == machineinfo.SessionActiveRDP
}

func (u *Updater) admitDestructive(cmd wsclient.Command) *wsclient.ErrorBody {
	if !wsclient.Destructive(cmd.Name) {
		return nil
	}
	if u.installing.Load() > 0 {
		return refuse(wsclient.ErrBusy, "an installation is in progress")
	}
	if cmd.Name == wsclient.CmdMachineReboot {
		var a wsclient.RebootArgs
		_ = wsclient.DecodeArgs(cmd.Args, &a)
		if a.Mode() == wsclient.WhenUserActiveSkip && userActive(u.loggedUser()) {
			return refuse(wsclient.ErrUserActive, "a user is logged on and when_user_active is skip")
		}
	}
	return nil
}

func (u *Updater) runDestructive(ctx context.Context, s commandSession, msg wsclient.Message, cmd wsclient.Command) {
	rec := state.PendingCommand{ID: msg.ID, Name: cmd.Name, AcceptedAt: u.clock().UTC()}
	fail := func(err error) {
		u.Log.Error("client command failed", "name", cmd.Name, "id", msg.ID, "error", err.Error())
		_ = u.Store.RemovePendingCommand(msg.ID)
		_ = s.Result(ctx, msg.ID, wsclient.Result{Status: wsclient.ResultError, Error: refuse(wsclient.ErrInternal, err.Error())})
	}
	if err := u.Store.AddPendingCommand(rec); err != nil {
		fail(err)
		return
	}
	switch cmd.Name {
	case wsclient.CmdServiceRestart:
		s.Close(websocket.StatusGoingAway, "service restarting")
		if err := u.restart(); err != nil {
			u.Log.Error("service restart could not be launched", "id", msg.ID, "error", err.Error())
			_ = u.Store.RemovePendingCommand(msg.ID)
		}
	case wsclient.CmdMachineReboot:
		var a wsclient.RebootArgs
		_ = wsclient.DecodeArgs(cmd.Args, &a)
		delay := time.Duration(a.Delay()) * time.Second
		if userActive(u.loggedUser()) && delay < minRebootNoticeWithUser {
			delay = minRebootNoticeWithUser
		}
		if err := u.reboot(delay); err != nil {
			fail(err)
			return
		}
		u.Log.Warn("machine reboot scheduled", "id", msg.ID, "delay", delay.String())
	}
}

func (u *Updater) reboot(d time.Duration) error {
	if u.rebootFn != nil {
		return u.rebootFn(d)
	}
	return power.Reboot(d)
}

// restart launches `<this exe> restart-service` detached and never waits
// on it, for the same reason selfupdate.Launch never does: its first act is
// to stop this service, whose stop handler waits for RunLoop to return.
func (u *Updater) restart() error {
	if u.restartFn != nil {
		return u.restartFn()
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "restart-service")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP, HideWindow: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
```

Mirror the exact `SysProcAttr` of `selfupdate.Launch` (read it) rather than the flags above if they differ.

Add to `Updater`: `installing atomic.Int32`, `restartFn func() error`, `rebootFn func(time.Duration) error`. In `service.go` `runSetupAndVerify`, wrap the `installer.Run` call: `u.installing.Add(1); defer u.installing.Add(-1)`. In `selfupdate.go` `applySelfUpdate`, `u.installing.Add(1)` right before `selfupdate.Launch` (never decremented on success: the service is about to stop; decrement on a failed launch).

- [ ] **Step 4: `main.go`** — add a hidden case next to `show-toast`:

```go
	case "restart-service":
		// Internal: launched detached by the service itself for the client
		// channel's service.restart command. Stop waits for the service to
		// finish stopping (60s), then start brings it back.
		if err := cmdStop(); err != nil {
			return err
		}
		return cmdStart()
```

(match the surrounding `switch` style — if cases call `os.Exit`/log instead of returning, follow that.)

- [ ] **Step 5: Run** — `go build ./... && go test ./... ` → PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/power internal/service main.go
git commit -m "feat(service): service.restart and machine.reboot over the client channel (wss only)"
```

---

### Task 9: Update events — available / started / applied / failed

**Files:**
- Modify: `internal/service/service.go` (`Cycle`, `install`), `internal/service/selfupdate.go`
- Create: `internal/service/clientupdates.go`, `internal/service/clientupdates_test.go`

**Interfaces:**
- Produces:

```go
func (u *Updater) announceUpdate(mc wsclient.ManifestCheck)   // gate: once per target+version per process
type updateEvent struct { Target, FromVersion, ToVersion string; Forced bool; Attempt int; Trigger string; DurationMS int64; Reinstalled bool; WillRetry bool; Error *wsclient.ErrorBody } // json snake_case, omitempty except target/to_version
func installFailureCode(err error) string
// Updater fields: announced map[string]string (poll goroutine only), wakeReason string (set by RunLoop, read by Cycle)
```

- [ ] **Step 1: Write the failing tests**

```go
package service

import (
	"errors"
	"testing"

	"emlyupdater/internal/wsclient"
)

func TestAnnounceUpdateOncePerVersion(t *testing.T) {
	u := newClientTestUpdater(t)
	var got []string
	u.emitFn = func(name string, p any) { got = append(got, name+":"+p.(wsclient.ManifestCheck).AvailableVersion) }
	u.announceUpdate(wsclient.ManifestCheck{Target: "emly", AvailableVersion: "3.5.0"})
	u.announceUpdate(wsclient.ManifestCheck{Target: "emly", AvailableVersion: "3.5.0"})
	u.announceUpdate(wsclient.ManifestCheck{Target: "updater", AvailableVersion: "1.9.1"})
	u.announceUpdate(wsclient.ManifestCheck{Target: "emly", AvailableVersion: "3.5.1"})
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
}

func TestInstallFailureCode(t *testing.T) {
	if c := installFailureCode(errors.New("post-install verification failed: config.ini reports 3.4.1, expected 3.5.0")); c != "version_mismatch" {
		t.Errorf("verification -> %s", c)
	}
	if c := installFailureCode(errors.New("EMLy_Setup.exe exited with code 2 (see x.log)")); c != "setup_exit_code" {
		t.Errorf("exit code -> %s", c)
	}
}
```

Run: `go test ./internal/service/ -run "AnnounceUpdate|InstallFailureCode" -v` — Expected: FAIL.

- [ ] **Step 2: Implement `clientupdates.go`**

```go
package service

import (
	"strings"

	"emlyupdater/internal/wsclient"
)

type updateEvent struct {
	Target      string              `json:"target"`
	FromVersion string              `json:"from_version,omitempty"`
	ToVersion   string              `json:"to_version"`
	Forced      bool                `json:"forced,omitempty"`
	Attempt     int                 `json:"attempt,omitempty"`
	Trigger     string              `json:"trigger,omitempty"`
	DurationMS  int64               `json:"duration_ms,omitempty"`
	Reinstalled bool                `json:"reinstalled,omitempty"`
	WillRetry   bool                `json:"will_retry,omitempty"`
	Error       *wsclient.ErrorBody `json:"error,omitempty"`
}

// announceUpdate emits update.available once per target and version for
// the life of the process (spec §8.2): a machine waiting three days for
// EMLy to close does not repeat it every cycle. Poll goroutine only.
func (u *Updater) announceUpdate(mc wsclient.ManifestCheck) {
	if u.announced == nil {
		u.announced = map[string]string{}
	}
	if u.announced[mc.Target] == mc.AvailableVersion {
		return
	}
	u.announced[mc.Target] = mc.AvailableVersion
	u.emit(wsclient.EvtUpdateAvailable, mc)
}

// installFailureCode maps installer errors onto spec §8.5's codes. The
// installer package reports by message, not by sentinel; this is the one
// place that reads it.
func installFailureCode(err error) string {
	if strings.Contains(err.Error(), "post-install verification failed") {
		return "version_mismatch"
	}
	return "setup_exit_code"
}
```

- [ ] **Step 3: Hooks in `service.go`**
  - `Cycle`: `trigger := u.wakeReason; if trigger == "" { trigger = "cycle" }` at the top. In the resume branch (`resuming pending update`) set `trigger = "resume"` before `apply`. Pass `trigger` down: `apply(ctx, cyc, p, emly)` → store it in a field `u.cycleTrigger` for `install` to read (both run on the poll goroutine), rather than changing three signatures.
  - After `u.Log.InfoEvent(logging.EventUpdateFound, ...)`:

    ```go
	enabled, _ := cyc.eff.UpdaterEnabled(cyc.host.Now)
	mc := wsclient.ManifestCheck{Target: "emly", Channel: emly.Channel, AvailableVersion: target.Version,
		UpdateAvailable: true, Critical: forced, MinRequiredVersion: m.MinRequiredVersion,
		Decision: emlyDecision(true, forced, u.emlyRunning(), !enabled),
		Source: u.serverRef(cyc, sourceURL(src)), CheckedAt: u.clock().UTC().Format(time.RFC3339)}
	if !emly.FreshInstall {
		mc.InstalledVersion = emly.InstalledVersion
	}
	u.announceUpdate(mc)
    ```

  - `install`: before the first `runSetupAndVerify`: `started := u.clock(); from := <the installed version before this install>` — pass `emly.InstalledVersion` (omit when `emly.FreshInstall`) — and

    ```go
	u.emit(wsclient.EvtUpdateStarted, updateEvent{Target: "emly", FromVersion: from, ToVersion: p.Version,
		Forced: p.Forced, Attempt: 1, Trigger: u.cycleTrigger})
    ```

    Track `reinstalled := false`, set `true` when entering the clean-install branch. On the final failure (the `ErrorEvent(logging.EventInstallFailed, "EMLy clean install failed", ...)` path) emit:

    ```go
		u.emit(wsclient.EvtUpdateFailed, updateEvent{Target: "emly", FromVersion: from, ToVersion: p.Version,
			Attempt: 2, WillRetry: true, Error: &wsclient.ErrorBody{Code: installFailureCode(err), Message: err.Error()}})
    ```

    After `u.Log.InfoEvent(logging.EventInstallOK, ...)`:

    ```go
	u.emit(wsclient.EvtUpdateApplied, updateEvent{Target: "emly", FromVersion: from, ToVersion: p.Version,
		Forced: p.Forced, Attempt: attempt, DurationMS: u.clock().Sub(started).Milliseconds(), Reinstalled: reinstalled})
    ```

    (`attempt` = 1, or 2 after the clean-install retry.) The "refusing to install" checksum branch at the top emits `update.failed` with code `checksum_mismatch`, `WillRetry: true`.
- [ ] **Step 4: Hooks in `selfupdate.go`**
  - `selfUpdate`, after `EventSelfUpdateFound`: `u.announceUpdate(wsclient.ManifestCheck{Target: "updater", InstalledVersion: running, AvailableVersion: m.Version, UpdateAvailable: true, Decision: "install_next_cycle", CheckedAt: ...})`.
  - `applySelfUpdate`, immediately before `selfupdate.Launch`: `u.emit(wsclient.EvtUpdateStarted, updateEvent{Target: "updater", FromVersion: version.Version, ToVersion: m.Version, Attempt: attempt, Trigger: "cycle"})`. The event is likely still buffered when the service stops — that is acceptable (spec §8.3: "l'ultimo messaggio che la vecchia build riesce a mandare"); do not block the launch on it.
  - `applySelfUpdate` failures after download (signature refused etc.) and `selfUpdate`'s `GiveUp` branch: emit `update.failed` with `Target: "updater"`, code `signature_invalid` / `checksum_mismatch` / `gave_up`, `WillRetry` = `!GiveUp`.
  - `reconcileSelfUpdate`: `OutcomeLanded` → `u.emit(EvtUpdateApplied, updateEvent{Target: "updater", FromVersion: rec.FromVersion, ToVersion: running, Attempt: rec.Attempts})`; `OutcomeMissed` → `u.emit(EvtUpdateFailed, updateEvent{Target: "updater", FromVersion: running, ToVersion: rec.Version, Attempt: rec.Attempts, WillRetry: rec.Attempts < selfupdate.MaxAttempts, Error: &wsclient.ErrorBody{Code: "version_mismatch", Message: "the launched setup did not leave the new version running"}})`. Both go to the buffer (the channel is not up yet at the first cycle) and are flushed after `service.started` — that is `TestEventsBufferedUntilWelcome`'s scenario.

- [ ] **Step 5: Run** — `go build ./... && go test ./... ` → PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/service
git commit -m "feat(service): update.available/started/applied/failed events for EMLy and the updater"
```

---

### Task 10: Notify → jittered wake of `RunLoop`

**Files:**
- Modify: `internal/service/clientnotify.go` (replace the stub), `internal/service/service.go` (`RunLoop`)
- Test: `internal/service/clientnotify_test.go`

**Interfaces:**
- Produces: `u.wake chan string` (buffer 1, reason); `u.forceConfig atomic.Bool`; `func (u *Updater) handleNotify(n wsclient.Notify)`; `func (u *Updater) scheduleWake(reason string, maxJitter time.Duration)`; seams `jitterFn func(max time.Duration) time.Duration`, `afterFunc func(time.Duration, func())`.

- [ ] **Step 1: Write the failing tests**

```go
package service

import (
	"encoding/json"
	"testing"
	"time"

	"emlyupdater/internal/wsclient"
)

func notifyOf(t *testing.T, topic string, payload any) wsclient.Notify {
	t.Helper()
	b, _ := json.Marshal(payload)
	return wsclient.Notify{Topic: topic, Payload: b}
}

func newNotifyUpdater(t *testing.T) (*Updater, *[]time.Duration) {
	u := newClientTestUpdater(t)
	withPolicy(t, u)
	u.wake = make(chan string, 1)
	var delays []time.Duration
	u.jitterFn = func(max time.Duration) time.Duration { return max }
	u.afterFunc = func(d time.Duration, f func()) { delays = append(delays, d); f() }
	return u, &delays
}

func TestReleasePublishedWakesWithMinimumJitter(t *testing.T) {
	u, delays := newNotifyUpdater(t)
	u.handleNotify(notifyOf(t, wsclient.TopicReleasePublished,
		wsclient.ReleasePublished{Target: "updater", Version: "99.0.0", JitterSeconds: 10}))
	if len(*delays) != 1 || (*delays)[0] != 60*time.Second {
		t.Fatalf("delays = %v", *delays)
	}
	if r := <-u.wake; r != "notify" {
		t.Fatalf("reason = %s", r)
	}
}

func TestReleasePublishedForOlderVersionIgnored(t *testing.T) {
	u, delays := newNotifyUpdater(t)
	u.handleNotify(notifyOf(t, wsclient.TopicReleasePublished,
		wsclient.ReleasePublished{Target: "updater", Version: "0.0.1", JitterSeconds: 600}))
	if len(*delays) != 0 {
		t.Fatalf("woke for an older release: %v", *delays)
	}
}

func TestConfigPublishedForcesRefresh(t *testing.T) {
	u, delays := newNotifyUpdater(t)
	u.handleNotify(notifyOf(t, wsclient.TopicConfigPublished, wsclient.ConfigPublished{Revision: 1 << 40, JitterSeconds: 30}))
	if !u.forceConfig.Load() || len(*delays) != 1 || (*delays)[0] != 30*time.Second {
		t.Fatalf("force=%v delays=%v", u.forceConfig.Load(), *delays)
	}
}

func TestNotifyCoalescesWakes(t *testing.T) {
	u, _ := newNotifyUpdater(t)
	n := notifyOf(t, wsclient.TopicReleasePublished, wsclient.ReleasePublished{Target: "updater", Version: "99.0.0", JitterSeconds: 60})
	u.handleNotify(n)
	u.handleNotify(n)
	u.handleNotify(n)
	if len(u.wake) != 1 {
		t.Fatalf("pending wakes = %d, want 1", len(u.wake))
	}
}
```

Run: `go test ./internal/service/ -run "ReleasePublished|ConfigPublished|NotifyCoalesces" -v` — Expected: FAIL.

- [ ] **Step 2: Implement `clientnotify.go`**

```go
package service

import (
	"encoding/json"
	"math/rand/v2"
	"time"

	"emlyupdater/internal/manifest"
	"emlyupdater/internal/version"
	"emlyupdater/internal/wsclient"
)

const minReleaseJitter = 60 * time.Second

// handleNotify turns a notify into a wake-up of RunLoop after a random
// delay (spec §9). It never runs a cycle itself (R-LOOP) and never trusts
// the payload beyond deciding whether waking up is worth it: the cycle
// re-reads the manifest or the document and validates it as always.
func (u *Updater) handleNotify(n wsclient.Notify) {
	switch n.Topic {
	case wsclient.TopicReleasePublished:
		var p wsclient.ReleasePublished
		if json.Unmarshal(n.Payload, &p) != nil || !u.releaseConcernsMe(p) {
			return
		}
		jitter := time.Duration(p.JitterSeconds) * time.Second
		if jitter < minReleaseJitter {
			jitter = minReleaseJitter
		}
		u.Log.Info("release announced over the client channel, waking the update loop",
			"target", p.Target, "version", p.Version, "maxDelay", jitter.String())
		u.scheduleWake("notify", jitter)
	case wsclient.TopicConfigPublished:
		var p wsclient.ConfigPublished
		if json.Unmarshal(n.Payload, &p) != nil {
			return
		}
		if cyc := u.cur.Load(); cyc != nil && p.Revision <= cyc.snap.Revision() {
			return
		}
		u.forceConfig.Store(true)
		u.scheduleWake("notify", time.Duration(max(p.JitterSeconds, 0))*time.Second)
	default:
		u.Log.Debug("unknown notify topic ignored", "topic", n.Topic)
	}
}

func (u *Updater) releaseConcernsMe(p wsclient.ReleasePublished) bool {
	switch p.Target {
	case "updater":
		newer, err := manifest.Less(version.Version, p.Version)
		return err == nil && newer
	case "emly":
		cyc := u.cur.Load()
		if cyc == nil {
			return false
		}
		emly := u.Cfg.ResolveEMLyWithChannel(cyc.eff.Doc.Updater.Channel())
		if p.Channel != "" && p.Channel != emly.Channel {
			return false
		}
		newer, err := manifest.Less(emly.InstalledVersion, p.Version)
		return err == nil && newer
	}
	return false
}

// scheduleWake wakes RunLoop after a random delay in [0, maxJitter]. The
// wake channel holds one reason: extra notifies before RunLoop picks it up
// collapse into that one, so a burst never queues several cycles.
func (u *Updater) scheduleWake(reason string, maxJitter time.Duration) {
	jitter := u.jitterFn
	if jitter == nil {
		jitter = func(m time.Duration) time.Duration {
			if m <= 0 {
				return 0
			}
			return time.Duration(rand.Int64N(int64(m) + 1))
		}
	}
	after := u.afterFunc
	if after == nil {
		after = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
	}
	after(jitter(maxJitter), func() {
		select {
		case u.wake <- reason:
		default:
		}
	})
}
```

- [ ] **Step 3: `RunLoop`** (`service.go`)
  - `New`: `wake: make(chan string, 1)`.
  - Loop top, replace `u.refreshConfig(ctx, false)` with `u.refreshConfig(ctx, u.forceConfig.Swap(false))`.
  - The select:

    ```go
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
    ```

    `wakeReason` is read by `Cycle` (Task 9) on the same goroutine.

- [ ] **Step 4: Run** — `go build ./... && go test ./... ` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/service
git commit -m "feat(service): release/config notifies wake the update loop after a random delay"
```

---

### Task 11: Documentation and manual verification list

**Files:**
- Modify: `AGENTS.md` (and `CLAUDE.md` if the winget merge added one — keep them consistent), `internal/machineinfo/sessionchange.go` (comment), `internal/wsclient/client.go` (package doc)

- [ ] **Step 1: `sessionchange.go`** — replace "The values are this package's own labels, not yet a wire contract: nothing sends them to the API (see Updater.watchSessions)." with: "The values are a wire contract with the API since protocol v2 of the client channel: they travel verbatim in session.changed's `events` (emly-go-api/CLIENT_WS_PROTOCOL.md §8.1). Renaming one is a protocol change."

- [ ] **Step 2: `wsclient` package doc** — the first paragraph now says the connection also carries protocol v2 (commands, events, notify), the reference is `emly-go-api/CLIENT_WS_PROTOCOL.md`, and `protocol.go`/`id.go` mirror the API's `internal/clientproto` by hand.

- [ ] **Step 3: `AGENTS.md`**
  - Architecture block: `wsclient/` (v2: handshake negotiation, `Handler`/`Session`), `power/`, `winget/` (from the merge), and `service/clientcmd.go`, `clientevents.go`, `clientnotify.go`, `clientpower.go`, `clientupdates.go`.
  - New conventions (bullets, same voice as the file):
    - **Protocol v2 is negotiated, not assumed** — identity carries `protocol`/`capabilities`; nothing is executed before `welcome`; a v1 server sees a v1 client.
    - **The channel wakes, it never runs a cycle** — notifies go through `u.wake` with jitter (≥60s for releases); manifest commands are read-only dry runs and may overlap a `Cycle`.
    - **Commands are allowlisted by the document, destructive ones need TLS** — `clientWs.commands` (default read-only); `service.restart`/`machine.reboot` refused on `ws://` (`insecure_transport`); IDs written to `state.json` `pendingCommands` before acting, reported in `service.started`. `state.json` now holds **three** independent lifecycles.
    - **Events are best-effort** — buffered in memory (32) while the channel is down, never persisted; the poll path remains the truth.
    - **A command/event/field added to the protocol touches both repos** — `wsclient/protocol.go` here, `internal/clientproto` in the API, and `CLIENT_WS_PROTOCOL.md`; `policy.knownClientWSCommands` is pinned to `wsclient.CommandNames` by a test.
    - **`SessionChangeKind` values are a wire contract.**
  - Logs table: events 924 (command accepted; Event Log only for restart/reboot) and 925 (command refused).
  - "Manual verification (admin required)" subsection for the client channel:
    1. With a document enabling `clientWs` and `commands` incl. `machine.reboot`, over `wss://`: issue `machine.info` via `POST /v2/client/{id}/commands`, confirm `done` with the payload.
    2. `apps.list_upgradable` on a machine without Microsoft.WinGet.Client → `failed` with `winget_module_missing`; with the module → package list.
    3. `service.restart` → connection closes 1001, service restarts, `service.started` with `reason: command` lists the id, command `done`.
    4. `machine.reboot {"delay_seconds":120}` with a user logged on → Windows countdown shown, reboot, `service.started reason: boot`, command `done`. Same with `when_user_active: skip` → refused `user_active`.
    5. Same reboot through a `ws://` mirror → refused `insecure_transport`, nothing happens.
    6. RDP reconnect → `session.changed` with `active-rdp` in `GET /v2/client/{id}/events`, `updater_clients.logged_user_state` updated without `last_seen_at` moving.
    7. `POST /v2/client/notify` `release.published` for the current updater version + 1 → loop woken within the jitter, `update.available`/`started` events, then `update.applied` from the new build.

- [ ] **Step 4: Final verification and commit**

Run: `go vet ./... && go build -ldflags "-s -w" -o build\bin\emly-updater.exe . && go test ./...`
Expected: all PASS, binary builds.

```bash
git add AGENTS.md CLAUDE.md internal/machineinfo/sessionchange.go internal/wsclient/client.go
git commit -m "docs: client ws protocol v2 conventions, events and manual verification"
```
