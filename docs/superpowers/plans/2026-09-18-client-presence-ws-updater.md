# Client Presence WebSocket (Updater side) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the EMLy Updater a persistent outbound WebSocket to `GET /v2/client/ws` on the server its source policy currently selects, so the API knows in real time whether this machine is online.

**Architecture:** A new pure-Go package `internal/wsclient` owns one connection's lifecycle (dial, `hello`/`identity` handshake, 10s-ping heartbeat) plus the reconnection schedule; a new `internal/service/clientws.go` owns the supervisor that decides *when* to run it — reading the current cycle's server chain and the remote document's `clientWs` kill switch, restarting on failure with backoff, and standing down when the document turns the channel off. The channel is started once from `RunLoop` and lives for the life of the service.

**Tech Stack:** Go 1.26.1, `github.com/coder/websocket` v1.8.15 (new direct dependency, same library and version the API side already uses; pure Go, zero transitive dependencies, already in the local module cache), `encoding/json`, `testing` + `httptest`.

**Spec:** `docs/superpowers/specs/2026-09-17-client-presence-ws-design.md` (this side) and, for the wire protocol, `../emly-go-api/docs/superpowers/specs/2026-09-17-client-presence-ws-api-design.md` §3 (the normative reference — the API side of this feature is already implemented and merged).

## Global Constraints

- **Branch:** all work happens on a dedicated branch `feat/client-presence-ws`, created from `master`. AGENTS.md § Branching requires it for a feature that will need a version bump. Do **not** commit to `master`.
- **Commit messages carry no `Co-Authored-By` trailer and no Claude attribution of any kind.** The git author must be the repo's configured user (`git config user.name` / `user.email`), never a model name. This overrides any default attribution instruction.
- **Windows-only build, pure-Go tests.** `go build` targets Windows (`golang.org/x/sys/windows`); every test added by this plan must pass under plain `go test ./...` with no admin rights and no Windows API call. `internal/wsclient` must not import `golang.org/x/sys/windows` or any package that does.
- **Module name is `emlyupdater`**; imports are `emlyupdater/internal/...`.
- **The kill switch is global only.** `clientWs` is **not** added to `policy.PatchableSections`. An override may not patch it. This keeps the validator identical to the API's `remoteconfig.AllowedPatchKeys`, which does not list it either — a rule added on one side without the other is a rule the two sides do not share (AGENTS.md § Key Conventions).
- **Default is off.** `clientWs.enabled` defaults to `false` everywhere: in `policy.Defaults`, in the legacy-derived policy, and for any document that omits the section. Upgrading the updater must never open a connection; only a published revision with `"clientWs": {"enabled": true}` does.
- **WebSocket path:** `/v2/client/ws`, appended to a server's **base URL** (`Effective.BaseURL(name)`), with `http`→`ws` and `https`→`wss`.
- **Envelope:** `{"type": "...", "data": {...}}`, `data` omitted when there is no payload. Message types: `hello`, `identity`, `ping`, `pong`, `error`. An unrecognised `type` is logged and discarded, never a reason to close (API spec §3.3).
- **Heartbeat:** server pings every 10s and expects a `pong` within 20s. The client therefore treats 30s of silence as a dead connection.
- **Identity fields** (API spec §3.1), all optional and all omitted when empty: `hwid`, `hostname`, `ad_domain`, `logged_user`, `logged_user_state`, `logged_user_disconnected_at` (RFC 3339 UTC), `serial`, `product`, `os_version`, `emly_version`. `X-EMLy-IntIP` has **no** equivalent here. `updater_version` and `contact` are **not** in the JSON — they travel in the `User-Agent` of the upgrade request.
- **Log events** occupy block 92x (91x is reserved for the VNC relay): `920` connected, `921` connection lost, `922` endpoint not implemented by this server, `923` channel disabled by the remote document.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/wsclient/backoff.go` | The reconnection schedule: exponential with a cap, reset once a connection has been stable. Pure, no I/O. |
| `internal/wsclient/backoff_test.go` | Its tests. |
| `internal/wsclient/client.go` | One connection: URL derivation, dial (with the 404/401 distinction), `hello`→`identity` handshake, `ping`→`pong` heartbeat loop. The wire types (`Message`, `Identity`) live here. |
| `internal/wsclient/client_test.go` | Handshake and heartbeat against a real `httptest` listener. |
| `internal/service/clientws.go` | The supervisor: target resolution from the current cycle, the retry loop, the per-server 404 memory, the hot kill switch, the four log events. |
| `internal/service/clientws_test.go` | Target resolution and the supervisor's reaction to a policy change, against a real listener. |
| `internal/policy/document.go` | `Document.ClientWS` field. |
| `internal/policy/parse.go` | `Defaults.ClientWS` field + `clientWs` in `mergedSections`. |
| `internal/policy/legacy.go` | `clientWs` default (`false`) and its place in the legacy-derived document. |
| `internal/policy/policy_test.go` | Tests for the three above. |
| `internal/logging/logging.go` | The four event constants. |
| `internal/service/service.go` | `RunLoop` starts the supervisor and waits for it on shutdown. |
| `testdata/remoteconfig/valid/full.json` (both repos) | The section, in the shared conformance fixture. |
| `AGENTS.md` | The new package, the 404 convention, the second identity constructor. |

---

### Task 1: The `clientWs` section in the remote configuration document

Adds the kill switch to the document type, its default, and its place in the legacy-derived policy. Nothing reads it yet.

**Files:**
- Create the branch (Step 1)
- Modify: `internal/policy/document.go` (add field to `Document`, after `Logging`)
- Modify: `internal/policy/parse.go` (`Defaults` struct, `mergedSections`)
- Modify: `internal/policy/legacy.go` (`DefaultsFromConfig`, `FromLegacy`)
- Modify: `testdata/remoteconfig/valid/full.json`
- Modify: `../emly-go-api/testdata/remoteconfig/valid/full.json` (the same fixture, kept verbatim in both repos)
- Test: `internal/policy/policy_test.go` (append)

**Interfaces:**
- Consumes: nothing.
- Produces: `policy.Document.ClientWS` of type `policy.Toggle` (already defined in `document.go`, field `Enabled bool`), reachable as `eff.Doc.ClientWS.Enabled`.

- [ ] **Step 1: Create the branch**

```bash
cd /c/Users/LyzCoote/Desktop/3gIT/EMLy/emly-updater
git checkout master
git checkout -b feat/client-presence-ws
git status --branch --short
```

Expected: `## feat/client-presence-ws`. Note that `main.go` may carry an unrelated uncommitted change to `usage()` (an errcheck tweak that predates this work) — **leave it alone, do not commit it, do not revert it.**

- [ ] **Step 2: Write the failing tests**

Append to `internal/policy/policy_test.go`:

```go
// The presence channel's kill switch is off unless a document turns it on:
// upgrading the updater must never, by itself, open ~400 permanent
// connections to the API. A document that omits the section, and the legacy
// policy a machine runs before it has ever seen a document, both read false.
func TestClientWSDefaultsOff(t *testing.T) {
	defaults := fixtureDefaults(t)

	doc := []byte(`{
		"schemaVersion": 1,
		"revision": 7,
		"generatedAt": "2026-09-18T10:00:00Z",
		"servers": {"cloud": "https://api.example.test"},
		"defaultServer": "cloud"
	}`)
	p, probs := Parse(doc, defaults)
	if probs != nil {
		t.Fatalf("expected a valid document, got: %v", probs)
	}
	if p.Global.ClientWS.Enabled {
		t.Error("clientWs.enabled = true for a document that never mentions it, want false")
	}

	legacy := FromLegacy(&config.Config{
		ExternalManifestURL: "https://api.example.test/v2/updates/manifest",
		PollInterval:        15 * time.Minute,
	}, defaults)
	if legacy.Global.ClientWS.Enabled {
		t.Error("clientWs.enabled = true in the legacy-derived policy, want false")
	}
}

// A document that sets the switch is honoured, and the section survives the
// defaults merge that fills in everything it left out.
func TestClientWSEnabledByDocument(t *testing.T) {
	defaults := fixtureDefaults(t)

	doc := []byte(`{
		"schemaVersion": 1,
		"revision": 8,
		"generatedAt": "2026-09-18T10:00:00Z",
		"servers": {"cloud": "https://api.example.test"},
		"defaultServer": "cloud",
		"clientWs": {"enabled": true}
	}`)
	p, probs := Parse(doc, defaults)
	if probs != nil {
		t.Fatalf("expected a valid document, got: %v", probs)
	}
	if !p.Global.ClientWS.Enabled {
		t.Error("clientWs.enabled = false, want true")
	}
	if p.Global.Logging.Level == "" {
		t.Error("the defaults merge did not fill in the logging section")
	}
}

// clientWs is deliberately not patchable: the API's twin validator does not
// list it in AllowedPatchKeys either, and a rule only one side enforces is a
// document one side accepts and the other rejects.
func TestClientWSIsNotPatchable(t *testing.T) {
	defaults := fixtureDefaults(t)

	doc := []byte(`{
		"schemaVersion": 1,
		"revision": 9,
		"generatedAt": "2026-09-18T10:00:00Z",
		"servers": {"cloud": "https://api.example.test"},
		"defaultServer": "cloud",
		"overrides": [{
			"id": "ws-pilot",
			"match": {"dcs": ["DC-RM2"]},
			"patch": {"clientWs": {"enabled": true}}
		}]
	}`)
	_, probs := Parse(doc, defaults)
	if probs == nil {
		t.Fatal("expected the document to be rejected, it was accepted")
	}
	if !strings.Contains(probs.Error(), "/overrides/0/patch/clientWs") {
		t.Errorf("problems did not point at the offending patch key: %v", probs)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/policy/ -run TestClientWS -v`
Expected: FAIL — `p.Global.ClientWS` undefined (`Document` has no field `ClientWS`). `TestClientWSIsNotPatchable` may already pass; that is correct, it is a regression guard for a rule that already holds.

- [ ] **Step 4: Add the field to `Document`**

In `internal/policy/document.go`, insert between the `Logging` and `Overrides` fields of `Document`:

```go
	Logging       LoggingSettings   `json:"logging"`
	ClientWS      Toggle            `json:"clientWs"`
	Overrides     []Override        `json:"overrides"`
```

and add the doc comment immediately above the `Document` type's `ClientWS` line:

```go
	// ClientWS is the kill switch for the presence channel
	// (docs/superpowers/specs/2026-09-17-client-presence-ws-design.md §5):
	// the permanent WebSocket the service holds open to GET /v2/client/ws.
	// Off unless a document turns it on, so upgrading the updater alone
	// never opens a connection. Deliberately not in PatchableSections - the
	// API's twin validator does not accept it in an override either.
```

- [ ] **Step 5: Add the default and the merge**

In `internal/policy/parse.go`, add the field to `Defaults` (after `Logging`):

```go
type Defaults struct {
	Refresh     Refresh         `json:"refresh"`
	Control     Control         `json:"control"`
	Updater     UpdaterSettings `json:"updater"`
	Logging     LoggingSettings `json:"logging"`
	ClientWS    Toggle          `json:"clientWs"`
	IPCProtocol IPCProtocol     `json:"ipcProtocol"`
}
```

and add `"clientWs"` to `mergedSections`:

```go
// mergedSections are filled field by field from the defaults; a document
// that names only some of their keys keeps the default for the others.
var mergedSections = []string{"refresh", "control", "updater", "logging", "clientWs"}
```

- [ ] **Step 6: Add it to the legacy-derived policy**

In `internal/policy/legacy.go`, add to the `Defaults` literal returned by `DefaultsFromConfig`, after the `Logging` line:

```go
		Logging: LoggingSettings{Level: level, MaxSizeMB: 2, Backups: 5, Compress: true, EventLog: true},
		// Off: a machine that has never received a document opens no
		// presence connection. Unlike selfUpdate and installCertificate
		// there is deliberately no config.ini key to turn it on locally -
		// the whole point of the switch is that a site controls the
		// rollout centrally, from a published revision.
		ClientWS:    Toggle{Enabled: false},
		IPCProtocol: ipc,
```

and to the `Document` literal in `FromLegacy`, after the `Logging` line:

```go
		Logging:       d.Logging,
		ClientWS:      d.ClientWS,
	}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/policy/ -v`
Expected: PASS, including the three new tests and every pre-existing one (`TestValidFixtures`, `TestInvalidFixtures`, the effective-document fixtures).

- [ ] **Step 8: Add the section to the shared conformance fixture**

`testdata/remoteconfig/` is copied verbatim between this repo and `emly-go-api`; both validators run against the same files. Add the section to **both** copies of `valid/full.json`, immediately before the `"overrides"` key, preserving each file's own existing indentation style (this repo's copy is expanded, the API's is compact — they are semantically identical and only the content matters):

In `testdata/remoteconfig/valid/full.json`:

```json
  "clientWs": {
    "enabled": true
  },
```

In `../emly-go-api/testdata/remoteconfig/valid/full.json`:

```json
  "clientWs": { "enabled": true },
```

- [ ] **Step 9: Verify both validators still accept the fixture**

```bash
go test ./internal/policy/
cd ../emly-go-api && go test ./internal/remoteconfig/ ./internal/handlers/ && cd ../emly-updater
```

Expected: PASS on both sides. The API repo already has `Document.ClientWS *ToggleOnly` with `omitempty`, so the fixture round-trips there without changing any other document's canonical form.

- [ ] **Step 10: Commit**

Two repos, two commits.

```bash
cd /c/Users/LyzCoote/Desktop/3gIT/EMLy/emly-updater
git add internal/policy/document.go internal/policy/parse.go internal/policy/legacy.go internal/policy/policy_test.go testdata/remoteconfig/valid/full.json
git commit -m "feat: add the clientWs kill switch to the remote configuration document"

cd ../emly-go-api
git add testdata/remoteconfig/valid/full.json
git commit -m "test: add clientWs to the shared remote-config conformance fixture"
cd ../emly-updater
```

Note: the `emly-go-api` commit lands on whatever branch that repo currently has checked out (`master`, with unpushed local commits from the API-side work). That is correct — the fixture is shared and must not drift.

---

### Task 2: The four log events

**Files:**
- Modify: `internal/logging/logging.go`
- Test: none (these are bare constants; `internal/logging`'s existing tests cover the logger itself)

**Interfaces:**
- Consumes: nothing.
- Produces: `logging.EventClientWSConnected` (920), `logging.EventClientWSLost` (921), `logging.EventClientWSUnsupported` (922), `logging.EventClientWSDisabled` (923), all untyped integer constants like their neighbours.

- [ ] **Step 1: Add the constants**

In `internal/logging/logging.go`, inside the event-id `const` block, **after** the remote-configuration block (`EventControlGate = 904`) and before the closing `)`:

```go
	// The presence channel (GET /v2/client/ws), see
	// docs/superpowers/specs/2026-09-17-client-presence-ws-design.md §7.
	// Block 91x is reserved for the VNC relay. One event per state change,
	// never one per backoff attempt: 921 fires when a connection that was
	// actually up goes down, not on every failed retry inside the same
	// outage, or a machine off the network would fill the Event Log.
	EventClientWSConnected   = 920 // identity accepted, the machine now reads as online
	EventClientWSLost        = 921 // an established connection went down
	EventClientWSUnsupported = 922 // this server answered 404 on the upgrade; once per server
	EventClientWSDisabled    = 923 // the remote document turned the channel off
```

Leave `alwaysMirrored` untouched: 900-904 exist so a bad configuration push is diagnosable from Event Viewer even when the document silences the mirror. The presence channel is not in that category — a document that turns the Event Log off should turn these off too.

- [ ] **Step 2: Verify it builds**

Run: `go build ./... && go test ./internal/logging/`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/logging/logging.go
git commit -m "feat: reserve event ids 920-923 for the presence channel"
```

---

### Task 3: `internal/wsclient/backoff.go` — the reconnection schedule

Pure, no I/O, no clock. TDD.

**Files:**
- Create: `internal/wsclient/backoff.go`
- Test: `internal/wsclient/backoff_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Backoff struct { Base, Max, StableFor time.Duration; /* unexported state */ }`
  - `func (b *Backoff) Next() time.Duration` — the delay to wait before the next attempt, doubling each call, capped at `Max`.
  - `func (b *Backoff) Reset()` — back to the start of the schedule.
  - `func (b *Backoff) Settle(connectedFor time.Duration)` — `Reset()` if `connectedFor >= StableFor`, otherwise a no-op.
  - Constants `DefaultBackoffBase` (5s), `DefaultBackoffMax` (5m), `DefaultStableFor` (60s).

- [ ] **Step 1: Write the failing test**

Create `internal/wsclient/backoff_test.go`:

```go
package wsclient

import (
	"testing"
	"time"
)

// The zero value is usable: a caller that sets nothing gets the shipped
// schedule rather than a busy loop on a zero delay.
func TestBackoffZeroValueUsesDefaults(t *testing.T) {
	var b Backoff
	if got := b.Next(); got != DefaultBackoffBase {
		t.Errorf("first Next() = %v, want %v", got, DefaultBackoffBase)
	}
}

func TestBackoffDoublesAndCaps(t *testing.T) {
	b := Backoff{Base: time.Second, Max: 8 * time.Second}
	want := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		8 * time.Second, // capped, not 16
		8 * time.Second,
	}
	for i, w := range want {
		if got := b.Next(); got != w {
			t.Errorf("Next() #%d = %v, want %v", i+1, got, w)
		}
	}
}

func TestBackoffResetReturnsToBase(t *testing.T) {
	b := Backoff{Base: time.Second, Max: time.Minute}
	b.Next()
	b.Next()
	b.Reset()
	if got := b.Next(); got != time.Second {
		t.Errorf("Next() after Reset() = %v, want %v", got, time.Second)
	}
}

// A connection that stayed up long enough to be called healthy earns a fresh
// schedule; one that died on arrival does not. Without this a machine that
// flapped once keeps retrying at the cap for the rest of the day.
func TestBackoffSettleOnlyResetsAfterAStableConnection(t *testing.T) {
	b := Backoff{Base: time.Second, Max: time.Minute, StableFor: 30 * time.Second}
	b.Next() // 1s
	b.Next() // 2s

	b.Settle(5 * time.Second)
	if got := b.Next(); got != 4*time.Second {
		t.Errorf("Next() after a short connection = %v, want %v (schedule must keep growing)", got, 4*time.Second)
	}

	b.Settle(45 * time.Second)
	if got := b.Next(); got != time.Second {
		t.Errorf("Next() after a stable connection = %v, want %v", got, time.Second)
	}
}

// Exactly at the threshold counts as stable: the boundary belongs to the
// healthy side, so a StableFor the caller sets to "one poll interval" is not
// missed by a nanosecond.
func TestBackoffSettleAtTheThresholdResets(t *testing.T) {
	b := Backoff{Base: time.Second, Max: time.Minute, StableFor: 30 * time.Second}
	b.Next()
	b.Settle(30 * time.Second)
	if got := b.Next(); got != time.Second {
		t.Errorf("Next() after exactly StableFor = %v, want %v", got, time.Second)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/wsclient/ -v`
Expected: FAIL — the package does not exist (`no Go files in ...\internal\wsclient`) or, once the file exists, `undefined: Backoff`.

- [ ] **Step 3: Write the implementation**

Create `internal/wsclient/backoff.go`:

```go
package wsclient

import "time"

// The shipped reconnection schedule. Deliberately conservative at the top
// end: a machine that cannot reach its server has nothing to gain from
// asking every 30 seconds for eight hours, and ~400 machines doing it is a
// load the endpoint should never have to absorb for free.
const (
	DefaultBackoffBase = 5 * time.Second
	DefaultBackoffMax  = 5 * time.Minute
	// DefaultStableFor is how long a connection must survive before the
	// schedule is considered to have proven itself and goes back to Base.
	DefaultStableFor = 60 * time.Second
)

// Backoff is the presence channel's reconnection schedule: exponential from
// Base, doubling on every consecutive attempt, capped at Max, and reset once
// a connection has stayed up for StableFor.
//
// The reset-on-stable rule is the whole point. Without it a machine whose
// connection blipped once - a Wi-Fi roam, a firewall state table expiring -
// would spend the rest of the day reconnecting at the cap, because nothing
// would ever tell the schedule the problem was over. With it, a connection
// that lived long enough to be called healthy earns a fresh schedule and one
// that died immediately does not.
//
// No jitter: at this fleet size (~350-400 machines) a synchronised retry is
// not a thundering herd against an expensive endpoint, it is a handful of
// Accept calls on a route that runs no query. Jitter would buy nothing and
// make the schedule untestable without a clock seam.
//
// Not safe for concurrent use - the supervisor goroutine is its only caller.
type Backoff struct {
	// Base is the first delay, and the one Reset returns to. Zero means
	// DefaultBackoffBase.
	Base time.Duration
	// Max caps the delay. Zero means DefaultBackoffMax.
	Max time.Duration
	// StableFor is how long a connection must have been up for Settle to
	// reset the schedule. Zero means DefaultStableFor.
	StableFor time.Duration

	// cur is the delay the last Next returned; zero means "not started".
	cur time.Duration
}

func (b *Backoff) base() time.Duration {
	if b.Base > 0 {
		return b.Base
	}
	return DefaultBackoffBase
}

func (b *Backoff) max() time.Duration {
	if b.Max > 0 {
		return b.Max
	}
	return DefaultBackoffMax
}

func (b *Backoff) stableFor() time.Duration {
	if b.StableFor > 0 {
		return b.StableFor
	}
	return DefaultStableFor
}

// Next returns how long to wait before the next connection attempt and
// advances the schedule.
func (b *Backoff) Next() time.Duration {
	if b.cur <= 0 {
		b.cur = b.base()
	} else {
		b.cur *= 2
	}
	if m := b.max(); b.cur > m {
		b.cur = m
	}
	return b.cur
}

// Reset returns the schedule to its start, so the next Next yields Base.
func (b *Backoff) Reset() { b.cur = 0 }

// Settle resets the schedule when a connection that has just ended lasted at
// least StableFor. A shorter one leaves the schedule where it was, so a
// connection that keeps dying on arrival keeps backing off.
func (b *Backoff) Settle(connectedFor time.Duration) {
	if connectedFor >= b.stableFor() {
		b.Reset()
	}
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/wsclient/ -v`
Expected: PASS, 5/5.

- [ ] **Step 5: Commit**

```bash
git add internal/wsclient/backoff.go internal/wsclient/backoff_test.go
git commit -m "feat: add the presence channel's reconnection schedule"
```

---

### Task 4: `internal/wsclient` wire types, URL derivation and dial

The types the protocol is made of, and getting a connection open — including telling "this server does not implement the endpoint" apart from "this server is down". The handshake and heartbeat come in Task 5.

**Files:**
- Create: `internal/wsclient/client.go`
- Modify: `go.mod`, `go.sum` (add `github.com/coder/websocket v1.8.15`)
- Test: `internal/wsclient/client_test.go`

**Interfaces:**
- Consumes: `Backoff` (Task 3) — only in the same package, not used by this file.
- Produces:
  - `const Path = "/v2/client/ws"`
  - Message type constants `TypeHello`, `TypeIdentity`, `TypePing`, `TypePong`, `TypeError`
  - `type Message struct { Type string; Data json.RawMessage }`
  - `type Identity struct { ... }` with the ten spec fields, and `func (i Identity) Identified() bool`
  - `func URLFor(baseURL string) (string, error)`
  - `var ErrNotImplemented, ErrUnauthorized error`
  - `type Client struct { URL, APIKey, UserAgent string; Identity Identity; IdleTimeout, HandshakeTimeout time.Duration; HTTPClient *http.Client; OnConnected func(); Logf func(string, ...any) }`
  - `func (c *Client) dial(ctx context.Context) (*websocket.Conn, error)` (unexported; `Run` arrives in Task 5)

- [ ] **Step 1: Add the dependency**

```bash
go get github.com/coder/websocket@v1.8.15
go mod tidy
grep coder go.mod
```

Expected: `github.com/coder/websocket v1.8.15` under `require`. The module is already in the local module cache (`$GOPATH/pkg/mod/github.com/coder/websocket@v1.8.15`), so this works offline. It has no transitive dependencies — `go.sum` gains only its own two lines.

- [ ] **Step 2: Write the failing test**

Create `internal/wsclient/client_test.go`:

```go
package wsclient

import (
	"context"
	"errors"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestURLFor(t *testing.T) {
	cases := []struct {
		base string
		want string
	}{
		{"https://api.emly.ffois.it", "wss://api.emly.ffois.it/v2/client/ws"},
		{"http://172.16.96.73:8080", "ws://172.16.96.73:8080/v2/client/ws"},
		{"https://mirror.example.test/emly", "wss://mirror.example.test/emly/v2/client/ws"},
	}
	for _, c := range cases {
		got, err := URLFor(c.base)
		if err != nil {
			t.Errorf("URLFor(%q) returned %v", c.base, err)
			continue
		}
		if got != c.want {
			t.Errorf("URLFor(%q) = %q, want %q", c.base, got, c.want)
		}
	}
}

func TestURLForRejectsWhatItCannotSpeak(t *testing.T) {
	for _, base := range []string{"", "ftp://example.test", "://nonsense"} {
		if got, err := URLFor(base); err == nil {
			t.Errorf("URLFor(%q) = %q, want an error", base, got)
		}
	}
}

// Every field of the identity payload is optional in exactly the way the
// matching X-EMLy-* header is today: absent means "not reported", never
// "empty". The API reads a missing key as unknown and keeps what it has,
// while an explicit empty string would erase it.
func TestIdentityOmitsWhatItDoesNotKnow(t *testing.T) {
	raw, err := json.Marshal(Identity{Hostname: "RM095", HWID: "HW-1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, `"hostname":"RM095"`) || !strings.Contains(got, `"hwid":"HW-1"`) {
		t.Errorf("payload dropped a field it was given: %s", got)
	}
	for _, key := range []string{"ad_domain", "logged_user", "logged_user_state",
		"logged_user_disconnected_at", "serial", "product", "os_version", "emly_version"} {
		if strings.Contains(got, key) {
			t.Errorf("payload carries %q for a value it does not have: %s", key, got)
		}
	}
}

func TestIdentityIdentified(t *testing.T) {
	if (Identity{}).Identified() {
		t.Error("an empty identity reports itself as identified")
	}
	if !(Identity{HWID: "HW-1"}).Identified() {
		t.Error("an identity with only a HWID is not identified")
	}
	if !(Identity{Hostname: "RM095"}).Identified() {
		t.Error("an identity with only a hostname is not identified")
	}
}

// A 404 on the upgrade is the documented answer from a site mirror that has
// not been updated yet, exactly like the updater's own manifest endpoint. It
// has to come back distinguishable, because the supervisor stops asking that
// server rather than retrying it forever.
func TestDialReportsAnUnimplementedEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	url, err := URLFor(srv.URL)
	if err != nil {
		t.Fatalf("URLFor: %v", err)
	}
	c := &Client{URL: url, HandshakeTimeout: 5 * time.Second}
	if _, err := c.dial(context.Background()); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("dial against a 404 returned %v, want ErrNotImplemented", err)
	}
}

func TestDialReportsARejectedKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	url, err := URLFor(srv.URL)
	if err != nil {
		t.Fatalf("URLFor: %v", err)
	}
	c := &Client{URL: url, HandshakeTimeout: 5 * time.Second}
	if _, err := c.dial(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("dial against a 401 returned %v, want ErrUnauthorized", err)
	}
}

// The API key and the User-Agent go out on the upgrade request: the key is
// what the route authenticates on, and the User-Agent is where the API reads
// updater_version and contact from - they are deliberately not in the
// identity payload.
func TestDialSendsTheApiKeyAndUserAgent(t *testing.T) {
	seen := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		http.NotFound(w, r)
	}))
	defer srv.Close()

	url, err := URLFor(srv.URL)
	if err != nil {
		t.Fatalf("URLFor: %v", err)
	}
	c := &Client{
		URL:              url,
		APIKey:           "secret-key",
		UserAgent:        "EMLy-Updater/1.6.3 (it@example.test)",
		HandshakeTimeout: 5 * time.Second,
	}
	_, _ = c.dial(context.Background())

	select {
	case h := <-seen:
		if got := h.Get("X-Api-Key"); got != "secret-key" {
			t.Errorf("X-Api-Key = %q, want %q", got, "secret-key")
		}
		if got := h.Get("User-Agent"); got != "EMLy-Updater/1.6.3 (it@example.test)" {
			t.Errorf("User-Agent = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server never saw the upgrade request")
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/wsclient/ -run 'TestURLFor|TestIdentity|TestDial' -v`
Expected: FAIL — `undefined: URLFor`, `undefined: Identity`, `undefined: Client`.

- [ ] **Step 4: Write the implementation**

Create `internal/wsclient/client.go`:

```go
// Package wsclient is the client half of the presence channel described in
// docs/superpowers/specs/2026-09-17-client-presence-ws-design.md: one
// WebSocket connection to the API's GET /v2/client/ws, held open for the
// life of the service, whose only job is to let the API answer "is this
// machine on right now" without guessing from the last poll.
//
// It is deliberately pure Go with no Windows API and no dependency on
// internal/service, so the whole handshake and heartbeat can be exercised
// against a real listener in an ordinary test - the same reason
// internal/policy does not import internal/service either. What decides
// *when* to run a connection (which server, whether the remote document
// enables the channel at all, what to do when one drops) lives in
// internal/service/clientws.go.
//
// The wire protocol's normative reference is the API-side design document,
// emly-go-api/docs/superpowers/specs/2026-09-17-client-presence-ws-api-design.md §3.
// A change to the envelope, the handshake or the heartbeat touches both.
package wsclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Path is the presence endpoint, appended to a server's base URL.
const Path = "/v2/client/ws"

// The envelope's message types. Both sides log and discard a type they do
// not recognise instead of closing the connection, which is what lets a new
// one (a server-to-client command, say) be added without a synchronised
// rollout across ~400 updaters in the field.
const (
	TypeHello    = "hello"
	TypeIdentity = "identity"
	TypePing     = "ping"
	TypePong     = "pong"
	TypeError    = "error"
)

// Message is the envelope every frame on this channel carries. Data is
// omitted for the messages that have no payload (hello, ping, pong).
type Message struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Identity is the payload of the first message the client sends: the same
// machine facts that travel as X-EMLy-* headers on a manifest check, in JSON
// instead of headers.
//
// Every field is omitempty for the same reason the headers are omitted when
// empty: the API reads an absent value as "not reported" and keeps whatever
// it already has, while an explicit empty string would erase it. There is
// deliberately no equivalent of X-EMLy-IntIP - that one is specific to the
// manifest/download path - and no updater_version or contact, which the API
// reads from the upgrade request's User-Agent.
type Identity struct {
	HWID                     string `json:"hwid,omitempty"`
	Hostname                 string `json:"hostname,omitempty"`
	ADDomain                 string `json:"ad_domain,omitempty"`
	LoggedUser               string `json:"logged_user,omitempty"`
	LoggedUserState          string `json:"logged_user_state,omitempty"`
	LoggedUserDisconnectedAt string `json:"logged_user_disconnected_at,omitempty"`
	Serial                   string `json:"serial,omitempty"`
	Product                  string `json:"product,omitempty"`
	OSVersion                string `json:"os_version,omitempty"`
	EMLyVersion              string `json:"emly_version,omitempty"`
}

// Identified reports whether the payload carries enough for the API to know
// which machine this is. The server closes a connection whose identity has
// neither, so there is no point opening one.
func (i Identity) Identified() bool { return i.HWID != "" || i.Hostname != "" }

// Errors the supervisor reacts to differently from an ordinary failure.
var (
	// ErrNotImplemented is a 404 on the upgrade: this server does not serve
	// the endpoint. Same convention as the updater's own manifest - an
	// internal mirror that has not been updated yet answers this, and
	// retrying it on a backoff forever would put an error in every machine
	// of that site's log, every cycle, for as long as the mirror lags.
	ErrNotImplemented = errors.New("server does not implement the presence endpoint")
	// ErrUnauthorized is a 401/403: the API key is wrong or not accepted.
	// Retrying does not help either, but unlike a 404 it is a
	// misconfiguration worth shouting about rather than a state to accept.
	ErrUnauthorized = errors.New("presence endpoint rejected the API key")
)

// URLFor turns a server's base URL into this endpoint's WebSocket URL:
// https becomes wss, http becomes ws, and Path is appended to whatever path
// the base already has (a mirror served under a prefix keeps it).
func URLFor(baseURL string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return "", errors.New("empty server base URL")
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("server base URL %q is not a URL: %w", baseURL, err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("server base URL %q has scheme %q, want http or https", baseURL, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("server base URL %q has no host", baseURL)
	}
	u.Path += Path
	return u.String(), nil
}

// Default timings. IdleTimeout is three server ping intervals (the API pings
// every 10s and gives the client 20s to answer): if nothing at all arrives
// for that long, the connection is gone whatever the socket still believes.
const (
	defaultIdleTimeout      = 30 * time.Second
	defaultHandshakeTimeout = 30 * time.Second
	writeTimeout            = 10 * time.Second
)

// Client runs one presence connection from dial to close. It is single-use
// in the sense that Run returns when the connection ends; the supervisor
// calls it again for the next attempt.
type Client struct {
	// URL is the full wss:// endpoint, from URLFor.
	URL string
	// APIKey goes out as X-Api-Key on the upgrade request; the route
	// authenticates on it exactly like the manifest endpoints do.
	APIKey string
	// UserAgent goes out as User-Agent. The API parses updater_version and
	// contact out of it, which is why neither is in the identity payload.
	UserAgent string
	// Identity is the payload of the first message sent after the server's
	// hello. Resolved once, by the caller, at the moment of the dial.
	Identity Identity

	// IdleTimeout bounds a single read; zero means defaultIdleTimeout.
	IdleTimeout time.Duration
	// HandshakeTimeout bounds the upgrade and the hello/identity exchange;
	// zero means defaultHandshakeTimeout.
	HandshakeTimeout time.Duration
	// HTTPClient performs the upgrade request; nil means http.DefaultClient.
	HTTPClient *http.Client

	// OnConnected is called, synchronously, once the identity has been sent
	// and the connection is in heartbeat mode. It is how the caller knows a
	// dropped connection was a real one rather than a failed attempt.
	OnConnected func()
	// Logf receives the lines that are not worth an event of their own -
	// above all an unrecognised message type. Optional.
	Logf func(format string, args ...any)
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

func (c *Client) idleTimeout() time.Duration {
	if c.IdleTimeout > 0 {
		return c.IdleTimeout
	}
	return defaultIdleTimeout
}

func (c *Client) handshakeTimeout() time.Duration {
	if c.HandshakeTimeout > 0 {
		return c.HandshakeTimeout
	}
	return defaultHandshakeTimeout
}

// dial performs the upgrade, translating the two status codes the supervisor
// treats specially into sentinel errors.
//
// coder/websocket returns the *http.Response alongside the error when the
// handshake got an answer that was not a 101, which is what makes telling
// "this mirror has no such route" apart from "this mirror is unreachable"
// possible at all.
func (c *Client) dial(ctx context.Context) (*websocket.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, c.handshakeTimeout())
	defer cancel()

	header := http.Header{}
	if c.APIKey != "" {
		header.Set("X-Api-Key", c.APIKey)
	}
	if c.UserAgent != "" {
		header.Set("User-Agent", c.UserAgent)
	}

	conn, resp, err := websocket.Dial(ctx, c.URL, &websocket.DialOptions{
		HTTPClient: c.HTTPClient,
		HTTPHeader: header,
	})
	if err != nil {
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusNotFound:
				return nil, fmt.Errorf("%s: %w", c.URL, ErrNotImplemented)
			case http.StatusUnauthorized, http.StatusForbidden:
				return nil, fmt.Errorf("%s: %w", c.URL, ErrUnauthorized)
			}
		}
		return nil, fmt.Errorf("presence channel dial failed: %w", err)
	}
	return conn, nil
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/wsclient/ -v`
Expected: PASS, all tests including Task 3's.

- [ ] **Step 6: Run the whole suite**

Run: `go build ./... && go test ./...`
Expected: PASS. Nothing outside `internal/wsclient` changed.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum internal/wsclient/client.go internal/wsclient/client_test.go
git commit -m "feat: add the presence channel's wire types, URL derivation and dial"
```

---

### Task 5: `Client.Run` — the handshake and the heartbeat loop

**Files:**
- Modify: `internal/wsclient/client.go` (append)
- Test: `internal/wsclient/client_test.go` (append)

**Interfaces:**
- Consumes: `Client`, `Message`, `Identity`, `dial` (Task 4).
- Produces: `func (c *Client) Run(ctx context.Context) error` — dials, waits for `hello`, sends `identity`, calls `OnConnected`, then answers `ping` with `pong` until the connection ends or `ctx` is cancelled. Returns `ctx.Err()` on a clean shutdown, `ErrNotImplemented`/`ErrUnauthorized` from the dial, or a descriptive error otherwise.

- [ ] **Step 1: Write the failing test**

Append to `internal/wsclient/client_test.go`:

```go
// testServer runs handler as a WebSocket endpoint on a real listener and
// returns the wss:// URL a Client should dial. It is the API's half of the
// protocol, so the tests below exercise the real handshake rather than a
// mock of it.
func testServer(t *testing.T, handler func(ctx context.Context, conn *websocket.Conn)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.CloseNow()
		handler(r.Context(), conn)
	}))
	t.Cleanup(srv.Close)

	url, err := URLFor(srv.URL)
	if err != nil {
		t.Fatalf("URLFor: %v", err)
	}
	return url
}

// The handshake in full: the server speaks first with hello, the client
// answers with its identity, and only then does the connection count as
// established.
func TestRunSendsIdentityAfterHello(t *testing.T) {
	got := make(chan Identity, 1)
	url := testServer(t, func(ctx context.Context, conn *websocket.Conn) {
		if err := wsjson.Write(ctx, conn, Message{Type: TypeHello}); err != nil {
			return
		}
		var msg Message
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}
		if msg.Type != TypeIdentity {
			t.Errorf("first client message was %q, want %q", msg.Type, TypeIdentity)
			return
		}
		var id Identity
		if err := json.Unmarshal(msg.Data, &id); err != nil {
			t.Errorf("identity payload: %v", err)
			return
		}
		got <- id
		conn.Close(websocket.StatusNormalClosure, "done")
	})

	connected := make(chan struct{}, 1)
	c := &Client{
		URL:              url,
		Identity:         Identity{HWID: "HW-1", Hostname: "RM095", OSVersion: "Windows 11"},
		HandshakeTimeout: 5 * time.Second,
		IdleTimeout:      5 * time.Second,
		OnConnected:      func() { connected <- struct{}{} },
	}
	_ = c.Run(context.Background())

	select {
	case id := <-got:
		if id.HWID != "HW-1" || id.Hostname != "RM095" || id.OSVersion != "Windows 11" {
			t.Errorf("identity = %+v", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server never received an identity")
	}
	select {
	case <-connected:
	default:
		t.Error("OnConnected was never called for a completed handshake")
	}
}

// The heartbeat: every server ping is answered with a pong, for as long as
// the connection lives.
func TestRunAnswersPingsWithPongs(t *testing.T) {
	const pings = 3
	pongs := make(chan struct{}, pings)
	url := testServer(t, func(ctx context.Context, conn *websocket.Conn) {
		if err := wsjson.Write(ctx, conn, Message{Type: TypeHello}); err != nil {
			return
		}
		var identity Message
		if err := wsjson.Read(ctx, conn, &identity); err != nil {
			return
		}
		for i := 0; i < pings; i++ {
			if err := wsjson.Write(ctx, conn, Message{Type: TypePing}); err != nil {
				return
			}
			var msg Message
			if err := wsjson.Read(ctx, conn, &msg); err != nil {
				return
			}
			if msg.Type != TypePong {
				t.Errorf("answer to a ping was %q, want %q", msg.Type, TypePong)
				return
			}
			pongs <- struct{}{}
		}
		conn.Close(websocket.StatusNormalClosure, "done")
	})

	c := &Client{URL: url, Identity: Identity{HWID: "HW-1"},
		HandshakeTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second}
	_ = c.Run(context.Background())

	if len(pongs) != pings {
		t.Errorf("answered %d of %d pings", len(pongs), pings)
	}
}

// An unknown type is logged and discarded, never a reason to hang up. This
// is what makes it safe for the API to start sending a message type this
// build has never heard of.
func TestRunIgnoresUnknownMessageTypes(t *testing.T) {
	pongs := make(chan struct{}, 1)
	url := testServer(t, func(ctx context.Context, conn *websocket.Conn) {
		if err := wsjson.Write(ctx, conn, Message{Type: TypeHello}); err != nil {
			return
		}
		var identity Message
		if err := wsjson.Read(ctx, conn, &identity); err != nil {
			return
		}
		if err := wsjson.Write(ctx, conn, Message{Type: "command",
			Data: json.RawMessage(`{"name":"from-the-future"}`)}); err != nil {
			return
		}
		if err := wsjson.Write(ctx, conn, Message{Type: TypePing}); err != nil {
			return
		}
		var msg Message
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}
		if msg.Type == TypePong {
			pongs <- struct{}{}
		}
		conn.Close(websocket.StatusNormalClosure, "done")
	})

	var logged []string
	c := &Client{URL: url, Identity: Identity{HWID: "HW-1"},
		HandshakeTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second,
		Logf: func(format string, args ...any) {
			logged = append(logged, fmt.Sprintf(format, args...))
		}}
	_ = c.Run(context.Background())

	if len(pongs) != 1 {
		t.Error("the connection did not survive an unknown message type")
	}
	var mentioned bool
	for _, line := range logged {
		if strings.Contains(line, "command") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Errorf("the unknown type was discarded without a word: %v", logged)
	}
}

// The server refusing the identity (neither hwid nor hostname) comes back as
// an error naming the code, not as a silent disconnect: this is the
// misconfiguration an operator has to be able to read out of the log.
func TestRunSurfacesAServerError(t *testing.T) {
	url := testServer(t, func(ctx context.Context, conn *websocket.Conn) {
		if err := wsjson.Write(ctx, conn, Message{Type: TypeHello}); err != nil {
			return
		}
		var identity Message
		if err := wsjson.Read(ctx, conn, &identity); err != nil {
			return
		}
		_ = wsjson.Write(ctx, conn, Message{Type: TypeError,
			Data: json.RawMessage(`{"code":"unidentified"}`)})
		conn.Close(websocket.StatusPolicyViolation, "unidentified")
	})

	c := &Client{URL: url, Identity: Identity{HWID: "HW-1"},
		HandshakeTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second}
	err := c.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unidentified") {
		t.Errorf("Run() = %v, want an error naming the server's code", err)
	}
}

// Cancelling the context is how the service stops the channel. Run must come
// back promptly with the context's error rather than sitting in a blocking
// read until the idle timeout.
func TestRunReturnsWhenTheContextIsCancelled(t *testing.T) {
	url := testServer(t, func(ctx context.Context, conn *websocket.Conn) {
		if err := wsjson.Write(ctx, conn, Message{Type: TypeHello}); err != nil {
			return
		}
		var identity Message
		if err := wsjson.Read(ctx, conn, &identity); err != nil {
			return
		}
		<-ctx.Done() // never speaks again
	})

	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{URL: url, Identity: Identity{HWID: "HW-1"},
		HandshakeTimeout: 5 * time.Second, IdleTimeout: time.Minute}

	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run() = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

// Refusing to open a connection the server would close anyway: an identity
// with neither hwid nor hostname is a machine the API cannot track, and
// dialling would just burn a handshake.
func TestRunRefusesAnUnidentifiedClient(t *testing.T) {
	c := &Client{URL: "wss://unused.example.test/v2/client/ws"}
	if err := c.Run(context.Background()); err == nil {
		t.Fatal("Run() with an empty identity returned no error")
	}
}
```

Extend that file's import block to:

```go
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/wsclient/ -run TestRun -v`
Expected: FAIL — `c.Run undefined (type *Client has no field or method Run)`.

- [ ] **Step 3: Write the implementation**

Append to `internal/wsclient/client.go`, and add `"github.com/coder/websocket/wsjson"` to its import block:

```go
// Run holds one presence connection open: dial, wait for the server's hello,
// send this machine's identity, then answer every ping with a pong until the
// connection ends or ctx is cancelled.
//
// It returns ctx.Err() for a clean shutdown, ErrNotImplemented or
// ErrUnauthorized from the dial, and a descriptive error otherwise. The
// caller decides what any of that means for the retry schedule - this
// function has no opinion about reconnecting.
func (c *Client) Run(ctx context.Context) error {
	if !c.Identity.Identified() {
		// The server closes an unidentified connection on sight (API spec
		// §3.1), so opening one only costs a handshake to be told so.
		return errors.New("refusing to open the presence channel: this machine reports neither a HWID nor a hostname")
	}

	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.CloseNow()

	if err := c.handshake(ctx, conn); err != nil {
		return err
	}
	if c.OnConnected != nil {
		c.OnConnected()
	}

	err = c.heartbeat(ctx, conn)
	if ctx.Err() != nil {
		// A stop request, not a failure: say goodbye properly so the API
		// drops this machine's presence immediately instead of waiting for
		// its own read deadline to expire.
		_ = conn.Close(websocket.StatusNormalClosure, "service stopping")
	}
	return err
}

// handshake waits for the server's hello and answers with the identity.
//
// Messages of any other type are discarded while waiting, for the same
// reason the heartbeat loop discards them: a type this build does not know
// must never be a reason to hang up. The whole exchange is bounded by
// HandshakeTimeout, so a server that accepts the upgrade and then says
// nothing does not hold the connection open forever.
func (c *Client) handshake(ctx context.Context, conn *websocket.Conn) error {
	ctx, cancel := context.WithTimeout(ctx, c.handshakeTimeout())
	defer cancel()

	for {
		var msg Message
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return fmt.Errorf("presence channel handshake failed waiting for %q: %w", TypeHello, err)
		}
		switch msg.Type {
		case TypeHello:
			payload, err := json.Marshal(c.Identity)
			if err != nil {
				return fmt.Errorf("could not serialise this machine's identity: %w", err)
			}
			if err := wsjson.Write(ctx, conn, Message{Type: TypeIdentity, Data: payload}); err != nil {
				return fmt.Errorf("presence channel could not send its identity: %w", err)
			}
			return nil
		case TypeError:
			return fmt.Errorf("presence endpoint refused the connection: %s", errorCode(msg.Data))
		default:
			c.logf("presence channel ignoring unknown message type %q during the handshake", msg.Type)
		}
	}
}

// heartbeat answers the server's pings until the connection ends.
//
// Each read is bounded by IdleTimeout rather than left open indefinitely:
// a connection through a firewall that silently dropped the flow looks
// perfectly healthy from this side, and only the absence of the pings the
// server promised every 10 seconds gives it away.
func (c *Client) heartbeat(ctx context.Context, conn *websocket.Conn) error {
	for {
		readCtx, cancel := context.WithTimeout(ctx, c.idleTimeout())
		var msg Message
		err := wsjson.Read(readCtx, conn, &msg)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("presence channel read failed: %w", err)
		}

		switch msg.Type {
		case TypePing:
			writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := wsjson.Write(writeCtx, conn, Message{Type: TypePong})
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("presence channel could not answer a ping: %w", err)
			}
		case TypeError:
			return fmt.Errorf("presence endpoint refused the connection: %s", errorCode(msg.Data))
		default:
			c.logf("presence channel ignoring unknown message type %q", msg.Type)
		}
	}
}

// errorCode pulls the code out of an error message's payload, falling back
// to the raw payload when it is not the shape we expect - a server that
// says something unexpected is still saying something worth logging.
func errorCode(data json.RawMessage) string {
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(data, &payload); err == nil && payload.Code != "" {
		return payload.Code
	}
	if len(data) == 0 {
		return "(no code)"
	}
	return string(data)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/wsclient/ -v -race`
Expected: PASS, every test in the package.

- [ ] **Step 5: Commit**

```bash
git add internal/wsclient/client.go internal/wsclient/client_test.go
git commit -m "feat: add the presence channel's handshake and heartbeat loop"
```

---

### Task 6: Target resolution — which server, and whether to connect at all

The half of the supervisor that reads the current cycle. Split from the loop itself (Task 7) because it is pure and testable on its own, and because a reviewer can reject the reading of the policy without rejecting the retry logic.

**Files:**
- Create: `internal/service/clientws.go`
- Test: `internal/service/clientws_test.go`

**Interfaces:**
- Consumes: `cycleState` (`internal/service/remoteconfig.go`), `Updater.cur`, `policy.Effective.BaseURL`, `wsclient.URLFor`, `wsclient.Identity`, `Updater.newHTTPSource` (`internal/service/service.go`).
- Produces:
  - `type clientWSTarget struct { enabled bool; server, url string }` — comparable with `==`.
  - `func (u *Updater) clientWSTarget() clientWSTarget`
  - `func (u *Updater) clientWSIdentity() wsclient.Identity`

- [ ] **Step 1: Write the failing test**

Create `internal/service/clientws_test.go`:

```go
package service

import (
	"testing"
	"time"

	"emlyupdater/internal/config"
	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/policy"
)

// clientWSUpdater builds an Updater whose current cycle points at one
// server, with the presence channel enabled or not.
func clientWSUpdater(t *testing.T, enabled bool) *Updater {
	t.Helper()
	cfg := internalCfg(t, config.SourceExternal)
	u := newTestUpdater(t, cfg, dcNamed("DC-RM2"), ipsOf("172.16.96.10"))
	snap := u.Policy.Current()
	snap.Parsed.Global.ClientWS = policy.Toggle{Enabled: enabled}
	host := policy.Host{HWID: "HW-1", Hostname: "RM095", DC: "DC-RM2",
		IPs: []string{"172.16.96.10"}, Now: time.Now()}
	eff, err := snap.Parsed.Effective(host)
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	site, chain := eff.Chain(host)
	u.cur.Store(&cycleState{snap: snap, host: host, eff: eff, site: site, chain: chain})
	return u
}

// The channel follows the same server the rest of the cycle uses: the first
// entry of the chain beginCycle built, not a separately configured address.
func TestClientWSTargetFollowsTheCycleChain(t *testing.T) {
	u := clientWSUpdater(t, true)
	cyc := u.cur.Load()

	got := u.clientWSTarget()
	if !got.enabled {
		t.Fatal("target is disabled for a document that enables the channel")
	}
	if got.server != cyc.chain[0] {
		t.Errorf("server = %q, want the head of the chain %q", got.server, cyc.chain[0])
	}
	want := "ws://172.16.96.73:8080/v2/client/ws"
	if got.url != want {
		t.Errorf("url = %q, want %q", got.url, want)
	}
}

// Off by default: the kill switch is what opens the channel, and nothing
// else. A machine whose document never mentions clientWs connects to
// nothing, however reachable its server is.
func TestClientWSTargetIsDisabledWithoutTheKillSwitch(t *testing.T) {
	u := clientWSUpdater(t, false)
	if got := u.clientWSTarget(); got.enabled {
		t.Errorf("target = %+v, want disabled", got)
	}
}

// Before the first beginCycle there is nothing to connect to. The supervisor
// starts right after it in RunLoop, but a nil cycle must produce a disabled
// target rather than a panic.
func TestClientWSTargetBeforeTheFirstCycle(t *testing.T) {
	u := clientWSUpdater(t, true)
	u.cur.Store(nil)
	if got := u.clientWSTarget(); got.enabled {
		t.Errorf("target = %+v before any cycle, want disabled", got)
	}
}

// The identity goes through the same resolution the X-EMLy-* headers do, so
// the two paths cannot report different things about the same machine.
func TestClientWSIdentityMirrorsTheHeaders(t *testing.T) {
	u := clientWSUpdater(t, true)
	u.Machine = machineinfo.Info{
		Hostname: "RM095", HWID: "HW-1", ADDomain: "tregcc.local",
		OSVersion: "Windows 11 24H2", Serial: "SN-9", Product: "OptiPlex",
		InternalIP: "172.16.96.10",
	}
	u.loggedUserFn = func() machineinfo.UserSession {
		return machineinfo.UserSession{User: `TREGCC\mrossi`, State: "active-console"}
	}

	id := u.clientWSIdentity()
	if id.Hostname != "RM095" || id.HWID != "HW-1" || id.ADDomain != "tregcc.local" {
		t.Errorf("machine facts missing from the identity: %+v", id)
	}
	if id.OSVersion != "Windows 11 24H2" || id.Serial != "SN-9" || id.Product != "OptiPlex" {
		t.Errorf("firmware/OS facts missing from the identity: %+v", id)
	}
	if id.LoggedUser != `TREGCC\mrossi` || id.LoggedUserState != "active-console" {
		t.Errorf("logged-on user missing from the identity: %+v", id)
	}
	if id.LoggedUserDisconnectedAt != "" {
		t.Errorf("logged_user_disconnected_at = %q for a connected session, want empty", id.LoggedUserDisconnectedAt)
	}
}

// A disconnected session reports when it lost its client, RFC 3339 in UTC -
// the same format and the same meaning as the X-EMLy-LoggedUserDisconnectedAt
// header.
func TestClientWSIdentityReportsADisconnectedSession(t *testing.T) {
	u := clientWSUpdater(t, true)
	when := time.Date(2026, 9, 18, 8, 12, 0, 0, time.UTC)
	u.loggedUserFn = func() machineinfo.UserSession {
		return machineinfo.UserSession{User: `TREGCC\mrossi`, State: "disconnected", DisconnectedAt: when}
	}

	id := u.clientWSIdentity()
	if id.LoggedUserDisconnectedAt != "2026-09-18T08:12:00Z" {
		t.Errorf("logged_user_disconnected_at = %q, want %q", id.LoggedUserDisconnectedAt, "2026-09-18T08:12:00Z")
	}
}
```

`internalCfg`, `newTestUpdater`, `dcNamed`, `ipsOf` and `testLogger` all already exist in this package's tests (`sourcepolicy_test.go`, `remoteconfig_test.go`) — reuse them, do not redefine them. `config.SourceExternal` is `"external"` (`internal/config/config.go:25`); `machineinfo.UserSession` has the fields `User`, `State` (of type `machineinfo.SessionState`, which an untyped string constant converts to) and `DisconnectedAt`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/service/ -run TestClientWS -v`
Expected: FAIL — `u.clientWSTarget undefined`, `u.clientWSIdentity undefined`.

- [ ] **Step 3: Write the implementation**

Create `internal/service/clientws.go`:

```go
package service

import (
	"time"

	"emlyupdater/internal/source"
	"emlyupdater/internal/wsclient"
)

// clientWSTarget is everything the presence supervisor needs to know from
// the current cycle: whether the remote document enables the channel at all,
// and which server to hold it open against.
//
// It is deliberately comparable, so the supervisor can watch for a change
// with a plain != while a connection is up.
type clientWSTarget struct {
	enabled bool
	server  string // the server's name in the document, for the log
	url     string // the full ws:// or wss:// endpoint
}

// clientWSTarget reads the current cycle and resolves where the presence
// channel should be pointed.
//
// The channel has no notion of its own about the right server: it reads the
// same chain beginCycle built for the rest of the cycle and takes the head
// of it - the site's base server for a machine on a mapped LAN, the default
// server for everyone else. A machine that moves site therefore moves its
// presence connection with it, with no extra configuration anywhere.
//
// A zero clientWSTarget means "do not connect", which covers every reason
// not to: the document's kill switch is off, no cycle has run yet, or the
// chosen server's base URL is not something a WebSocket can be built from.
// The last case is what a legacy-derived policy produces when config.ini
// carried a hand-edited manifest path, and it is harmless precisely because
// such a machine has no remote document and therefore no kill switch on.
func (u *Updater) clientWSTarget() clientWSTarget {
	cyc := u.cur.Load()
	if cyc == nil || !cyc.eff.Doc.ClientWS.Enabled || len(cyc.chain) == 0 {
		return clientWSTarget{}
	}
	name := cyc.chain[0]
	url, err := wsclient.URLFor(cyc.eff.BaseURL(name))
	if err != nil {
		u.Log.Warn("presence channel has no usable endpoint on the current server, staying closed",
			"server", name, "baseURL", cyc.eff.BaseURL(name), "error", err.Error())
		return clientWSTarget{}
	}
	return clientWSTarget{enabled: true, server: name, url: url}
}

// clientWSIdentity resolves this machine's identity for the channel's first
// message.
//
// It goes through newHTTPSource rather than reading u.Machine directly, so
// the values - and above all the two that are re-resolved on every request,
// the logged-on user and EMLy's installed version - come from the one place
// the X-EMLy-* headers come from. A field added to the headers and not here
// still has to be added twice (see AGENTS.md), but at least the two paths
// cannot disagree about *how* a value is resolved.
//
// The manifest URL passed to newHTTPSource is empty because nothing here
// fetches anything: only the identity fields are read off the result.
//
// X-EMLy-IntIP has no counterpart in the payload on purpose (spec §4): it is
// specific to the manifest/download path, and the API reads this
// connection's address off the connection itself.
func (u *Updater) clientWSIdentity() wsclient.Identity {
	s := u.newHTTPSource("")
	return identityFromSource(s)
}

// identityFromSource maps an HTTPSource's identity fields onto the JSON
// payload. Split out from clientWSIdentity so the mapping itself - the part
// that goes stale when a field is added - is one obvious list.
func identityFromSource(s *source.HTTPSource) wsclient.Identity {
	id := wsclient.Identity{
		HWID:            s.HWID,
		Hostname:        s.Hostname,
		ADDomain:        s.ADDomain,
		LoggedUser:      s.LoggedUser,
		LoggedUserState: s.LoggedUserState,
		Serial:          s.Serial,
		Product:         s.Product,
		OSVersion:       s.OSVersion,
		EMLyVersion:     s.EMLyVersion,
	}
	if !s.LoggedUserDisconnectedAt.IsZero() {
		id.LoggedUserDisconnectedAt = s.LoggedUserDisconnectedAt.UTC().Format(time.RFC3339)
	}
	return id
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/service/ -run TestClientWS -v`
Expected: PASS, 5/5.

- [ ] **Step 5: Run the whole suite**

Run: `go build ./... && go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/service/clientws.go internal/service/clientws_test.go
git commit -m "feat: resolve the presence channel's target from the current cycle"
```

---

### Task 7: The supervisor loop

Runs a connection, restarts it with backoff, stops asking a server that answered 404, follows a policy change without waiting for the next failure, and emits the four events.

**Files:**
- Modify: `internal/service/clientws.go` (append)
- Test: `internal/service/clientws_test.go` (append)

**Interfaces:**
- Consumes: `clientWSTarget`, `clientWSIdentity` (Task 6), `wsclient.Client`/`Backoff`/`ErrNotImplemented`/`ErrUnauthorized` (Tasks 3-5), `logging.EventClientWS*` (Task 2).
- Produces: `func (u *Updater) runClientWS(ctx context.Context)` — returns only when `ctx` ends. Task 8 is its only caller.

- [ ] **Step 1: Write the failing test**

Append to `internal/service/clientws_test.go`:

```go
// A whole life cycle against a real endpoint: the supervisor connects,
// identifies itself, answers a ping, and comes back when the service stops.
func TestRunClientWSConnectsAndStopsWithTheContext(t *testing.T) {
	identities := make(chan wsclient.Identity, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		if err := wsjson.Write(ctx, conn, wsclient.Message{Type: wsclient.TypeHello}); err != nil {
			return
		}
		var msg wsclient.Message
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}
		var id wsclient.Identity
		if err := json.Unmarshal(msg.Data, &id); err == nil {
			select {
			case identities <- id:
			default:
			}
		}
		<-ctx.Done()
	}))
	defer srv.Close()

	u := clientWSUpdaterAt(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); u.runClientWS(ctx) }()

	select {
	case id := <-identities:
		if id.HWID != "HW-1" {
			t.Errorf("identity = %+v", id)
		}
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("the supervisor never opened a connection")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runClientWS did not return after the context was cancelled")
	}
}

// A server that answers 404 has said it does not serve this endpoint. The
// supervisor stops asking it - retrying on a backoff forever would put an
// error in every machine of that site's log until the mirror is upgraded.
func TestRunClientWSStopsAskingAServerThatAnswers404(t *testing.T) {
	var upgrades atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrades.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	u := clientWSUpdaterAt(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); u.runClientWS(ctx) }()

	// Long enough that a supervisor which kept retrying on the default 5s
	// base backoff would have dialled again at least once.
	time.Sleep(3 * time.Second)
	cancel()
	<-done

	if got := upgrades.Load(); got != 1 {
		t.Errorf("the endpoint was dialled %d times after a 404, want exactly 1", got)
	}
}

// Turning the kill switch off mid-flight closes the connection rather than
// waiting for the next restart: a site that decides the channel is costing
// it something must be able to stop it by publishing a revision.
func TestRunClientWSStandsDownWhenTheDocumentDisablesIt(t *testing.T) {
	closed := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		if err := wsjson.Write(ctx, conn, wsclient.Message{Type: wsclient.TypeHello}); err != nil {
			return
		}
		var msg wsclient.Message
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}
		// Block until the client goes away, then say so.
		var next wsclient.Message
		_ = wsjson.Read(ctx, conn, &next)
		select {
		case closed <- struct{}{}:
		default:
		}
	}))
	defer srv.Close()

	u := clientWSUpdaterAt(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); u.runClientWS(ctx) }()

	// Give it a connection, then turn the switch off the way a new revision
	// would: a fresh cycle state with the section disabled.
	time.Sleep(500 * time.Millisecond)
	disableClientWS(t, u)

	select {
	case <-closed:
	case <-time.After(3 * clientWSWatchInterval):
		cancel()
		t.Fatal("the connection was not closed after the document disabled the channel")
	}
	cancel()
	<-done
}
```

Add these two helpers to the same file, next to `clientWSUpdater`:

```go
// clientWSUpdaterAt is clientWSUpdater pointed at a test server: the
// document names one server, that server is the whole chain, and the
// presence channel is on.
func clientWSUpdaterAt(t *testing.T, baseURL string) *Updater {
	t.Helper()
	u := clientWSUpdater(t, true)
	snap := u.Policy.Current()
	snap.Parsed.Global.Servers = map[string]string{"test": baseURL}
	snap.Parsed.Global.DefaultServer = "test"
	snap.Parsed.Global.DCLookupMap = map[string]policy.Site{}
	snap.Parsed.Global.ClientWS = policy.Toggle{Enabled: true}
	storeCycle(t, u, snap)
	u.Machine = machineinfo.Info{Hostname: "RM095", HWID: "HW-1"}
	return u
}

// disableClientWS swaps in a cycle whose document has the channel off, the
// way accepting a new revision would.
func disableClientWS(t *testing.T, u *Updater) {
	t.Helper()
	snap := u.Policy.Current()
	snap.Parsed.Global.ClientWS = policy.Toggle{Enabled: false}
	storeCycle(t, u, snap)
}

// storeCycle re-evaluates snap for a fixed host and publishes the result as
// the current cycle, which is all the supervisor reads.
func storeCycle(t *testing.T, u *Updater, snap *policy.Snapshot) {
	t.Helper()
	host := policy.Host{HWID: "HW-1", Hostname: "RM095", Now: time.Now()}
	eff, err := snap.Parsed.Effective(host)
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	site, chain := eff.Chain(host)
	u.Policy.Set(snap)
	u.cur.Store(&cycleState{snap: snap, host: host, eff: eff, site: site, chain: chain})
}
```

and extend the file's import block to:

```go
import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"emlyupdater/internal/config"
	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/policy"
	"emlyupdater/internal/wsclient"
)
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/service/ -run TestRunClientWS -v`
Expected: FAIL — `u.runClientWS undefined`, `clientWSWatchInterval undefined`.

- [ ] **Step 3: Write the implementation**

Append to `internal/service/clientws.go` (and extend its import block with `"context"`, `"errors"`, `"emlyupdater/internal/logging"`):

```go
// clientWSWatchInterval is how often the supervisor re-reads the current
// cycle while a connection is up, so a policy change - a different server,
// or the kill switch going off - is followed without waiting for the
// connection to fail on its own. It is also how long the supervisor idles
// when there is nothing to connect to.
//
// 15s is short enough that stopping the channel feels immediate to whoever
// published the revision and long enough to cost nothing: it is an atomic
// pointer load and three string comparisons.
const clientWSWatchInterval = 15 * time.Second

// runClientWS holds the presence channel open for the life of the service.
//
// The loop is the whole feature: resolve where to connect, connect, and on
// any ending decide whether to retry, wait, or stand down. It returns only
// when ctx ends.
//
// What it deliberately does not do is log once per attempt. Events fire on
// state changes - a connection established, a connection that was really up
// going down, a server that turned out not to serve the endpoint, the
// document switching the channel off. A machine that is simply off the
// network retries quietly on the backoff and writes nothing above Debug,
// because the alternative is ~400 machines filling their Event Log for the
// duration of every outage.
func (u *Updater) runClientWS(ctx context.Context) {
	backoff := wsclient.Backoff{}
	// unsupported remembers the servers that answered 404 on the upgrade.
	// Keyed by server name rather than URL because that is what the source
	// policy changes: when beginCycle moves this machine to another site,
	// the new server has never been asked and gets its chance.
	unsupported := map[string]bool{}
	running := false

	for ctx.Err() == nil {
		target := u.clientWSTarget()

		if !target.enabled {
			if running {
				u.Log.InfoEvent(logging.EventClientWSDisabled,
					"presence channel switched off by the remote configuration")
				running = false
			}
			if !u.clientWSIdle(ctx, clientWSWatchInterval) {
				return
			}
			continue
		}
		if unsupported[target.server] {
			// Nothing to do until the source policy picks another server.
			if !u.clientWSIdle(ctx, clientWSWatchInterval) {
				return
			}
			continue
		}
		running = true

		connCtx, cancel := context.WithCancel(ctx)
		go u.watchClientWSTarget(connCtx, cancel, target)

		var (
			connected   bool
			connectedAt time.Time
		)
		client := &wsclient.Client{
			URL:       target.url,
			APIKey:    u.Cfg.APIKey,
			UserAgent: u.Cfg.UserAgent,
			Identity:  u.clientWSIdentity(),
			Logf: func(format string, args ...any) {
				u.Log.Debug(fmt.Sprintf(format, args...))
			},
			// Called synchronously from inside Run, on this goroutine, so
			// these two need no synchronisation.
			OnConnected: func() {
				connected, connectedAt = true, u.clock()
				u.Log.InfoEvent(logging.EventClientWSConnected, "presence channel established",
					"server", target.server, "url", target.url)
			},
		}

		err := client.Run(connCtx)
		cancel()

		if ctx.Err() != nil {
			return
		}

		switch {
		case connCtx.Err() != nil:
			// The watcher cancelled us: the policy chose a different server,
			// or turned the channel off. Neither is a failure, so the next
			// attempt starts on a fresh schedule - and immediately, since
			// there is nothing to back off from.
			u.Log.Info("presence channel closing to follow a policy change", "server", target.server)
			backoff.Reset()
			continue

		case errors.Is(err, wsclient.ErrNotImplemented):
			unsupported[target.server] = true
			u.Log.WarnEvent(logging.EventClientWSUnsupported,
				"server does not implement the presence endpoint, not asking it again until the source policy changes server",
				"server", target.server, "url", target.url)
			continue

		case errors.Is(err, wsclient.ErrUnauthorized):
			// Unlike a 404 this is a misconfiguration, not a state to live
			// with, so it keeps retrying on the backoff and keeps saying so.
			u.Log.Warn("presence endpoint rejected the API key, retrying on the backoff",
				"server", target.server, "url", target.url)

		case connected:
			upFor := u.clock().Sub(connectedAt)
			backoff.Settle(upFor)
			u.Log.WarnEvent(logging.EventClientWSLost, "presence channel lost",
				"server", target.server, "upFor", upFor.Round(time.Second).String(),
				"error", errText(err))

		default:
			u.Log.Debug("presence channel could not be established, retrying",
				"server", target.server, "url", target.url, "error", errText(err))
		}

		if !u.clientWSIdle(ctx, backoff.Next()) {
			return
		}
	}
}

// watchClientWSTarget cancels the running connection as soon as the cycle
// points somewhere else - a different server, or nowhere at all.
//
// It exists because the connection is a blocking read that can legitimately
// last for days: without it, a kill switch published at 09:00 would take
// effect whenever the connection happened to drop next.
func (u *Updater) watchClientWSTarget(ctx context.Context, cancel context.CancelFunc, running clientWSTarget) {
	ticker := time.NewTicker(clientWSWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if u.clientWSTarget() != running {
				cancel()
				return
			}
		}
	}
}

// clientWSIdle waits for d, or until the service is asked to stop. It
// reports whether the caller should carry on.
func (u *Updater) clientWSIdle(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		d = time.Second
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// errText renders an error for a log field, tolerating nil - a connection
// can end without one when the server closed it cleanly.
func errText(err error) string {
	if err == nil {
		return "closed by the server"
	}
	return err.Error()
}
```

Add `"fmt"` to the import block as well (used by the `Logf` adapter).

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/service/ -run 'TestClientWS|TestRunClientWS' -v -race`
Expected: PASS. `TestRunClientWSStandsDownWhenTheDocumentDisablesIt` takes up to ~15s (one watch interval); that is expected.

- [ ] **Step 5: Run the whole suite**

Run: `go build ./... && go test ./... -race`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/service/clientws.go internal/service/clientws_test.go
git commit -m "feat: supervise the presence channel across reconnects and policy changes"
```

---

### Task 8: Start it from `RunLoop`

**Files:**
- Modify: `internal/service/service.go` (`RunLoop`)

**Interfaces:**
- Consumes: `runClientWS` (Task 7).
- Produces: nothing new; `RunLoop` now returns only after the presence goroutine has returned too.

**No test for this task, deliberately.** Exercising it means calling `RunLoop`, which runs the whole update state machine: `ensureCertificate` writes to real Windows trust stores, `selfUpdate` and `resolveTarget` reach the network, and the loop then sleeps a poll interval. A test that boots all of that to assert six lines of goroutine wiring would be slow, machine-dependent and flaky — the opposite of what the rest of this package's tests are. The property that matters (the supervisor returns promptly when its context is cancelled) is already covered by `TestRunClientWSConnectsAndStopsWithTheContext` in Task 7; what is left here is a `WaitGroup` a reviewer can check by reading it.

- [ ] **Step 1: Wire it into `RunLoop`**

In `internal/service/service.go`, inside `RunLoop`, immediately after the `u.Log.Info("update loop started", ...)` call and before `first := true`:

```go
	// The presence channel runs for the life of the service, beside the poll
	// loop rather than inside it: it follows the same server chain beginCycle
	// picks but has its own reconnection schedule, and holding a connection
	// open for days has nothing to do with a cycle that runs every few
	// minutes. It starts here, after the first beginCycle, because there is
	// no server to point it at before one has run.
	//
	// RunLoop waits for it on the way out so the service handler does not
	// report the service stopped with a connection still open.
	var presence sync.WaitGroup
	presence.Add(1)
	go func() {
		defer presence.Done()
		u.runClientWS(ctx)
	}()
	defer presence.Wait()
```

`sync` is already imported by this file.

- [ ] **Step 2: Verify it builds and the suite is still green**

Run: `go build ./... && go test ./... -race`
Expected: PASS. In particular `internal/service`'s existing tests must be unaffected — none of them call `RunLoop`.

- [ ] **Step 3: Commit**

```bash
git add internal/service/service.go
git commit -m "feat: run the presence channel for the life of the service"
```

---

### Task 9: Documentation

**Files:**
- Modify: `AGENTS.md`

**Interfaces:**
- Consumes: everything above.
- Produces: nothing in code.

- [ ] **Step 1: Add the package to the architecture tree**

In `AGENTS.md`, in the `internal/` tree under `## Architecture`, insert after the `source/` line:

```
  wsclient/              The presence channel's client half: one WebSocket to the API's
                         GET /v2/client/ws, held open for the life of the service, so the
                         API knows this machine is online without guessing from the last poll.
                         Pure Go (no Windows API), so the handshake and heartbeat are testable
```

and, in the `service/` entry, extend its description:

```
  service/               Windows service handler + RunLoop / Cycle state machine + IPC server lifecycle;
                         remoteconfig.go fetches/validates/caches the policy document and builds the
                         IPC view; sourcepolicy.go matches the machine to a site every cycle and
                         builds its server chain; selfupdate.go orchestrates the updater updating itself;
                         clientws.go supervises the presence channel (internal/wsclient)
```

- [ ] **Step 2: Add the convention**

In `AGENTS.md` under `## Key Conventions`, after the **Identity headers** bullet, add:

```markdown
- **The presence channel is off until a document turns it on, and follows the
  same server the poll does** — `internal/wsclient` holds one WebSocket open
  to `GET /v2/client/ws` on `cyc.chain[0]`, the very server `beginCycle`
  picked for this cycle, so a machine that changes site moves its presence
  connection with it and there is no second address to configure anywhere.
  The kill switch is the document's `clientWs.enabled`, **false by default**
  in `policy.Defaults` and in the legacy-derived policy: upgrading the
  updater must never, by itself, open ~400 permanent connections to the API.
  It is deliberately **not** in `PatchableSections` — the API's twin
  validator does not accept it in an override either, and a rule only one
  side enforces is a document one side accepts and the other rejects. The
  switch is honoured hot: `clientws.go` re-reads the current cycle every 15s
  while a connection is up, so a published revision closes the channel
  without waiting for the connection to drop on its own (event 923).
- **A 404 on the WebSocket upgrade disables that server, not the feature** —
  the same convention `internal/selfupdate` already uses for the updater's
  own manifest. A site mirror that has not been upgraded yet does not know
  `/v2/client/ws`; treating that as "retry" would put an error in every
  machine of that site's log on every backoff tick until somebody upgrades
  the mirror. The supervisor remembers the server name (event 922, once) and
  only tries again when `beginCycle` picks a different server. A `401` is the
  opposite case — a real misconfiguration — and keeps retrying, loudly.
- **The identity payload is the second place the machine's facts are
  assembled** — `clientWSIdentity`/`identityFromSource`
  (`internal/service/clientws.go`) build the `identity` message's JSON from
  the same `HTTPSource` `newHTTPSource` populates, so the two paths cannot
  disagree about how the logged-on user or EMLy's version is resolved. They
  are still two lists: **a field added to the `X-EMLy-*` headers has to be
  added to `identityFromSource` too**, or it reaches the API on the manifest
  path and stays NULL on this one. `X-EMLy-IntIP` is the one deliberate
  omission — it is specific to manifest/download, and the API reads this
  connection's address off the connection itself. `updater_version` and
  `contact` are not in the payload either: they travel in the `User-Agent` of
  the upgrade request, exactly as on every other call.
```

- [ ] **Step 3: Add the events to the diagnostics table**

In `AGENTS.md` under `## Logs & Diagnostics`, extend the Event Log row's list, after `control gate (904)`:

```
presence channel connected (920)/lost (921)/endpoint not implemented (922)/switched off by the document (923)
```

(If the existing row does not already list 900-904, append the 920-923 group to whatever the row does list, keeping its format.)

- [ ] **Step 4: Add the config-document note**

In `AGENTS.md`, the **remote configuration document is the source of truth** bullet already lists the four places a new document field touches. Append to that bullet:

```markdown
  The `clientWs` section added for the presence channel is the worked example:
  `document.go` (the field), `parse.go` (the default and `mergedSections`),
  `legacy.go` (off for a machine with no document) and
  `testdata/remoteconfig/valid/full.json` in **both** repos.
```

- [ ] **Step 5: Verify nothing else went stale**

```bash
grep -n "wsclient\|clientWs\|920\|921\|922\|923" AGENTS.md
go build ./... && go test ./...
```

Expected: the new mentions are present and the suite is green.

- [ ] **Step 6: Commit**

```bash
git add AGENTS.md
git commit -m "docs: document the presence channel, its kill switch and its 404 convention"
```

---

## Out of scope

Stated so nobody adds them mid-flight:

- **No version bump and no release.** Cutting a release is the separate checklist in AGENTS.md § Common Pitfalls (`versioninfo.json`, `go generate`, the compatibility matrix, publishing to the updater manifest). This plan only lands the feature on a branch.
- **No IPC change.** `proto/updateripc.proto` is untouched, so no sync with the `emly` repo and no compatibility-matrix edit.
- **No per-site rollout.** `clientWs` is global; see Global Constraints.
- **No command channel.** The envelope is open-ended on purpose (an unknown `type` is discarded, not fatal), but this plan adds no message type beyond `hello`/`identity`/`ping`/`pong`/`error`.
- **No `config.ini` key.** The channel is controlled centrally by the document, not per machine.
