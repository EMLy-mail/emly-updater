# AryxD Agent (EMLyUpdater) - Agent Instructions

## Build & Test

```powershell
# Generate version resources (requires goversioninfo) and propagate
# versioninfo.json's version into version_generated.go, installer.iss and
# config.default.ini (see tools/genversion)
go generate

# Build (output in build\bin\ or build\)
go build -ldflags "-s -w" -o build\bin\emly-updater.exe .

# Run all tests
go test ./...

# Build the InnoSetup installer (requires Inno Setup 6 installed)
# Only after the Go binary is built
iscc installer\installer.iss
```

- **Windows-only** - the binary uses `golang.org/x/sys/windows`; do not attempt to build or test on Linux/macOS.
- All tests are pure-Go (no Windows API calls); `go test ./...` works in CI without admin rights.
- Regenerating `internal/ipc/ipcpb` from `proto/updateripc.proto` requires `protoc` + `protoc-gen-go` (`go generate ./internal/ipc/ipcpb`); neither is needed for a normal build or `go test ./...` since the generated file is committed.

## Architecture

```
main.go                  Subcommands: install | uninstall | start | stop | run (foreground debug) | show-toast (internal, see notify/) |
                         restart-service (internal: detached stop+start for the client channel's service.restart, see service/clientpower.go) |
                         products [--check] (read-only: detection, pending and attempts per product; --check also asks the manifest, never downloads) |
                         tray (the per-user notification-area icon, see internal/tray) |
                         apply-settings (admin: write config.ini keys from the tray and restart the service)
proto/                   updateripc.proto - IPC wire schema, manually synced with the emly repo
tools/genversion/        go generate helper: propagates versioninfo.json's version everywhere else
internal/
  config/                INI loader for the *bootstrap* config (never written at runtime);
                         reset.go rewrites config.ini from this build's defaults on
                         upgrade (previous kept as config.prev.ini); paths.go owns all %ProgramData%\EMLyUpdater\* paths + ExeDir helpers
  policy/                The remote configuration document (GET /v2/config): types, all-or-nothing
                         validation, JSON Merge Patch overrides evaluated per host, the
                         last-known-good cache, and the atomic Snapshot the service reads.
                         legacy.go derives the default policy from config.ini's [source] keys
  source/                Source interface + HTTPSource (with User-Agent / X-Api-Key / X-EMLy-* headers) + Resolver (retry/backoff)
  wsclient/              The presence channel's client half: one WebSocket to the API's
                         GET /v2/client/ws, held open for the life of the service, so the
                         API knows this machine is online without guessing from the last poll.
                         On top of that, protocol v2 (negotiated in welcome): server->client
                         commands, client->server events, server->client notifies -
                         Handler/Session run the dispatch, protocol.go/id.go mirror the
                         API's internal/clientproto by hand. Pure Go (no Windows API), so the
                         handshake, heartbeat and v2 dispatch are all testable
  machineinfo/           The X-EMLy-* identity values: the machine facts collected once at startup
                         (hostname, HWID, AD domain, internal IP, firmware serial + product number)
                         and LoggedUser, resolved per request; domaincontroller.go finds the nearest DC
  manifest/              JSON manifest parse/compare (go-version for semver); updater.go is the updater's own release manifest
  download/              Download manager: Ensure = fetch+SHA256 verify; atomic writes. Prefix keeps
                         EMLy's cache and the updater's own from sweeping each other away (other
                         products get their own `downloads\<slug>\` subdirectory);
                         pacer.go waits out the server's 429s (Retry-After + jitter)
  authenticode/          WinVerifyTrust + signer-thumbprint pinning, for the updater's own setup
  selfupdate/            The self-update rules (Reconcile/Decide, pure) + the detached setup launch
  product/               What a product is, with no network I/O: Product (slug, InstallDir, ExeName,
                         channel, Installer spec, Legacy) and Detect, the closed `ini`/`file`/`exe`
                         version-source chain with three outcomes (installed / absent / unknown)
  installer/             Silent setup drivers behind `Driver` (`For(Spec)`): `inno` (EMLy,
                         /VERYSILENT + /FORCEUPGRADE) and `nsis` (`/S /D=<dir>`, uninstall
                         `uninstall.exe /S _?=<dir>`); CheckNotUserWritable gates every uninstaller
  service/               Windows service handler + RunLoop / Cycle state machine + IPC server lifecycle;
                         remoteconfig.go fetches/validates/caches the policy document and builds the
                         IPC view; sourcepolicy.go matches the machine to a site every cycle and
                         builds its server chain; selfupdate.go orchestrates the updater updating itself;
                         clientws.go supervises the presence channel (internal/wsclient) and its welcome
                         burst; clientcmd.go admits/dispatches v2 commands (dedupe ring, read-only
                         handlers, manifest dry runs); clientevents.go builds/buffers/emits v2 events
                         (service.started, machine.info, session.changed payloads); clientnotify.go
                         turns release.published/config.published into a jittered RunLoop wake;
                         clientpower.go is service.restart/machine.reboot's admission, destructivePending
                         state machine and execution (internal/power);
                         product.go/products.go are the per-product engine (installProduct, driverFor,
                         the sequential round cycleProducts, unavailable/attempt bookkeeping);
                         report.go backs the `products` subcommand
  state/                 state.json: pending update entry, written atomically, survives reboots
  logging/               Two sinks: lumberjack rolling file + Windows Event Log; exe-side log
  notify/                WTS warning dialog + update-complete toast launcher (SYSTEM -> user-session hop) in the active user session;
                         session_exec.go runs a process as a session's user and captures its output (RunInSession)
  toast/                 Notification-area balloon (Shell_NotifyIcon) with EMLy's icon; runs inside the user session, launched via `show-toast`
  progresswin/           Download/install progress window (raw Win32, EMLy's icon, not closable); runs inside the user session,
                         launched via `show-progress` by notify.OpenProgressWindow and driven through its stdin (JSON lines);
                         service/progress.go decides when it opens/closes. Gated ONLY by config.ini [progressWindow] enabled
  process/               Kernel wait on EMLy process handle + TerminateProcess for forced updates
  power/                 InitiateSystemShutdownEx for the client channel's machine.reboot command;
                         Windows' own countdown to logged-on users, not a dialog of our own; not
                         unit tested (it would reboot the test machine), see manual verification below
  assoc/                 HKLM file-association self-heal after install
  cert/                  Embedded 3gIT code-signing certificate + install into Root/TrustedPublisher (machine + console user)
  ipc/                   Named-pipe server exposing SystemInfo/ADStatus/Config to the EMLy client (protobuf)
  tray/                  `tray` subcommand: notification-area icon + settings window (windigo) in each user's
                         session. Reads config.ini and the effective policy (service.Updater.Prepare, read-only),
                         writes config.ini only through the elevated `apply-settings`, wakes the service with
                         service.CheckNowControl, dry-runs the manifest per product (CheckProduct/CheckUpdater)
  winget/                Read-only listing of winget-upgradable packages via the Microsoft.WinGet.Client
                         PowerShell module (JSON, never `winget upgrade` text); CLI in tools/winget-update-parser
```

See [README.md](README.md) for the full update-state-machine table and update-sources description.

## Deployment topology — there are no mirrors

Read this before designing anything that moves bytes over the network.

- **There is exactly one server.** `emly-go-api`, its MySQL database and the self-hosted S3
  that stores the setup binaries all run on the **same VM**, at one site. The `baseServer`, `backupServer` and
  `defaultServer` entries in the remote configuration are **different addresses (IPs/hostnames)
  of that same machine**, not independent mirrors. They are reachability alternatives: a
  different route or name when one fails. They give no extra capacity, no data redundancy
  and no per-site cache. Fetching "from a fallback" still loads the same box and the same link.
- **"Mirror" in code, comments and README is legacy wording.** It describes what the source
  chain *could* support. It is not what is deployed. Do not propose "let each site's mirror
  serve it" as a fix: that mirror does not exist.
- **The bottleneck is the MPLS link, and everything crosses it.** About 350 clients across 3
  sites, plus PCs working from home, all reach that one server over MPLS; home PCs come in over
  MPLS too, not over a separate internet path. Treat the whole fleet as one shared pipe, and
  do not assume any client group has a cheaper route. A
  fleet-wide download of a 5–10 MB setup (EMLy or EMLyUpdater) saturates it: per-client
  throughput has been seen dropping to ~200 KB/s.
- **Consequence for design:** anything that makes many machines act at once must be paced
  centrally by the server. That includes downloads after a release, notify-triggered wake-ups
  and config fetches. Client-side jitter alone does not bound concurrency on a shared link.
  Updates can be urgent and must install as soon as the link allows, so "download days early,
  install later" is not an acceptable answer.

## Product name vs. technical identifiers

The product is called **AryxD Agent**: the distribution, monitoring and
management agent for 3gIT's supported products: EMLy, plus any other product the
remote configuration's `products` section describes (see *Products other than EMLy*). The rename is **display-only**. Every name another component or an
existing install relies on keeps the old `EMLyUpdater` spelling, and must not
be renamed without a coordinated migration:

| Identifier | Value | Who depends on it |
|---|---|---|
| Windows service name | `EMLyUpdater` (`service.Name`) | `emly` (`app_updater_status.go`: registry key + SCM query), SCM upgrade in place |
| Named pipe | `\.\pipe\EMLyUpdater` | `emly` (`updateripc/dial.go`, hardcoded) |
| Singleton mutex | `Global\EMLyUpdaterSingleton` | an older service still running during an upgrade |
| Data directory | `%ProgramData%\EMLyUpdater` | config/state/cache of every installed machine; `emly` checks it exists |
| Install directory / exe | `%ProgramFiles%\EMLyUpdater\EMLyUpdater.exe` | `emly` checks the folder; the service's `ImagePath`; `PrepareToInstall` stops the *old* exe by this path |
| Setup file name | `EMLyUpdater_Installer_<ver>.exe` | `emly-go-api` (`updates/updater.route.go`) builds it for self-update; CI globs it |
| Inno Setup `AppId` | `EMLyUpdater` (pinned explicitly) | the uninstall key; a new AppId installs side by side instead of upgrading |
| Event Log source | `EMLyUpdater` | registered at `install`; existing event-log filters/SIEM rules |
| User-Agent | `EMLy-Updater/<ver> (...)` | `emly-go-api` (`updaterclient.go` regex) identifies updater requests by it |
| Download prefix | `EMLyUpdater-` | cache cleanup in `internal/download` |

What *is* renamed: the service display name and description, `versioninfo.json`
`ProductName`/`FileDescription`, the installer's `AppName`/`AppVerName`
(Programs and Features), toast titles, CLI messages, and the first log lines of
every run (`logIdentity` in `main.go`), which state the agent's roles and that
EMLy is a product it distributes.

## Key Conventions

- **The remote configuration document is the source of truth, `config.ini` is
  only the bootstrap** - `internal/policy` owns the document served by
  `GET /v2/config`: servers, `dcLookupMap`, poll interval, kill switches,
  logging, and the per-host `overrides`. It is validated all-or-nothing (a
  single bad field rejects the whole document, event 902) and cached in
  `%ProgramData%\EMLyUpdater\remote-config.json`; the cache is the policy when
  the endpoint is unreachable, and `policy.FromLegacy` derives an equivalent
  document from `config.ini`'s `[source]` keys when no document has ever been
  accepted - which is what makes this a no-op on a machine that never reaches
  the API. A document is only accepted when its `revision` is >= the cached
  one, so a lagging mirror cannot roll a machine back. Adding a field to the
  document touches `internal/policy/document.go`, its validation in
  `parse.go`, the defaults in `legacy.go`, **and** the shared fixtures under
  `testdata/remoteconfig/` (see below).
  The `clientWs` section added for the presence channel is the worked example:
  `document.go` (the field), `parse.go` (the default and `mergedSections`),
  `legacy.go` (off for a machine with no document) and
  `testdata/remoteconfig/valid/full.json` in **both** repos.
- **The validation fixtures are shared with the API repo** -
  `testdata/remoteconfig/` (`valid/`, `invalid/` with the expected problem
  paths, `effective/` with document + host + expected result) is copied
  verbatim between this repo and `emly-go-api`, the way
  `proto/updateripc.proto` is. Both sides' validators run against the same
  files; that is the only thing keeping the two implementations equal, since
  there is no shared Go module. A rule added on one side without its fixture
  is a rule the other side does not have.
- **Identity headers: machine facts are collected once, the logged-on user
  every time** - `internal/machineinfo` supplies the `X-EMLy-*` headers every
  request carries (`Hostname`, `HWID`, `ADDomain`, `IntIP`, `Serial`,
  `Product`, `LoggedUser`, `LoggedUserState`, `LoggedUserDisconnectedAt`). `Collect()` runs once in `service.New` because the
  AD domain and the firmware strings each cost a PowerShell spawn and none of
  them change while the service runs. `LoggedUser` is the exception and is
  **not** part of `machineinfo.Info`: it is re-resolved in `newHTTPSource`, so
  it is at most one poll cycle stale instead of frozen at boot (when nobody is
  usually logged on yet). It enumerates WTS sessions rather than reusing
  `notify.ConsoleUserSID`, which only ever names the physical console and so
  would report the wrong person - or nobody - on a machine being used over
  RDP. The same lookup also sends `LoggedUserState` (`active-console`,
  `active-rdp`, `disconnected`) and, for a disconnected session only,
  `LoggedUserDisconnectedAt` (RFC 3339 UTC, from `WTSSessionInfoEx`): a
  disconnected session is still reported as the logged user, because for
  inventory it answers "whose machine is this", and the state is what keeps
  the server from reading it as a live presence. The state values are a wire
  contract with the API. `X-EMLy-AppVersion` is the other per-request header and
  comes from EMLy's own `config.ini` (`GUI_SEMVER`) through
  `Cfg.ResolveEMLy`, re-read on every source because a setup this updater
  runs changes it; a machine with no EMLy installed sends nothing rather than
  the `0.0.0` fresh-install sentinel, which is an internal comparison value
  and not a release. An unset value sends **no header at all**, never an empty one: the API
  reads a missing header as "unknown" and keeps what it has, while an empty
  string would erase it.
  `X-EMLy-InstalledProducts` (`emly=3.5.0,foo=1.2.0`, sorted by slug) is the
  **one exception** to that rule, because the API treats it as the complete
  inventory and decides from it which dashboard users see the machine: nil
  sends no header ("unknown"), an empty non-nil map sends an **empty header**
  ("nothing installed", every product dropped). `Updater.installedProducts`
  runs `product.Detect` on EMLy **and on every product in the document, enabled
  or not**; `installed(v)` goes in the map, `absent` stays out, and **one single
  `unknown` makes the whole result nil** (no header), because an inventory that
  omits a product reads as that product being uninstalled. For EMLy the chain is
  one `ini` read of `config.ini` and, unlike `ResolveEMLy`, tells "config.ini
  does not exist" (EMLy absent) apart from "exists but unreadable / no
  `GUI_SEMVER`" (unknown). Never collapse the two: an empty inventory sent
  by mistake removes the machine from its owners' dashboard. EMLy is listed
  under slug `emly` with its `GUI_SEMVER`, never `0.0.0`. The WS `identity`
  carries the same inventory as `installed_products`, a `*map[string]string`
  so `{}` survives `omitempty`. Not to be confused with `X-EMLy-Product`
  (firmware SKU). `X-EMLy-AppVersion` still reports EMLy only.
- **Products other than EMLy are data, EMLy is the implicit product** -
  the document's `products` section (`internal/policy/products.go`, keyed by
  slug, `emly` forbidden, patchable by overrides, shared fixtures under
  `testdata/remoteconfig/` like everything else in the document) defines them;
  `product.Product` is what the engine works on. The design is
  `docs/superpowers/specs/2026-09-30-multi-product-agent-design.md`.
  - **One sequential round per cycle** (`cycleProducts`, `products.go`):
    enabled document products sorted by slug, **EMLy last** because it is the
    one that can block on `WaitForExit` and must not delay the others. The
    destructive gate is re-checked before each product, the cycle trigger is
    reset per product, and only EMLy's error fails the cycle - a generic
    product's error is logged and the round moves on. Never two downloads or
    two setups at once on a machine.
  - **`Legacy` is what makes EMLy EMLy**: file-association repair after the
    install, localized critical warning and "app open" messages (`LANGUAGE`
    from EMLy's config.ini; other products are Italian only, like the progress
    window), the channel from EMLy's config.ini with `updater.channelOverride`
    winning, fresh install when absent, blocking `WaitForExit`, the clean
    retry with uninstall on every cycle with no attempt cap, `/FORCEUPGRADE`,
    the `update.*` WS events with `target: "emly"`, and the historical manifest
    path `/v2/updates/manifest`. A generic product gets none of these.
  - **Detection has three outcomes and only `installed` can lead to an
    install.** The chain is tried in order: a source whose file is missing
    falls through to the next; the first `installed(v)` wins; if a source
    exists but is broken (unreadable, no key, unparsable) and no later one
    answers, the result is `unknown`, **not** `absent`. `absent` means every
    source was missing. `unknown` skips the product (one Warn per session) and
    silences the inventory; it never installs, so a transient read error cannot
    trigger a reinstall over live data. `installWhenAbsent` defaults to false:
    a product is only updated where it already is. A detect `path` must stay
    inside `installDir`: relative, no `..` segment, and no `:` at all - that
    rules out drive-relative paths (`C:version.txt` resolves against drive
    C:'s current directory, not `installDir`) and NTFS streams
    (`version.txt:x`). Both validators (this repo's `internal/policy` and
    `emly-go-api`'s `internal/remoteconfig`) enforce it, with a shared fixture.
  - **`enabled` switches updates, not detection.** A disabled product is still
    detected and still in the inventory, so the dashboard keeps showing it.
  - **Not blocking when the app is open.** A generic product whose exe is
    running keeps its pending entry and the round goes on; the user gets one
    "update pending" notification per version (in-memory set, recorded only
    when a box was actually shown, so nobody-logged-on retries next cycle), and
    `watchProductExit` starts one goroutine per product that waits for the
    app to exit (`process.WaitForExitUnder`, scoped like below) and then
    wakes `RunLoop` with reason `product-exit` (reported as trigger `cycle`).
    The goroutine **never installs**: the woken cycle does, so the "one setup
    at a time" rule holds. If EMLy is blocking the cycle in `WaitForExit` at
    that moment, the wake waits until EMLy is done. A forced update still
    warns and terminates, like EMLy.
  - **A product's process is matched by name *and* location.** Its `exeName`
    comes from the document and the service is SYSTEM, so "is it running" and
    the forced kill (`process.IsRunningUnder` / `TerminateAllUnder`) count only
    instances whose full image path (`QueryFullProcessImageNameW`) lies inside
    its `installDir` - case-insensitive, on a separator boundary, so
    `C:\3gIT\X` does not claim `C:\3gIT\XY\a.exe`. A process whose path
    cannot be read is not the product's. EMLy keeps the name-only
    `IsRunning` / `TerminateAll` / `WaitForExit`.
  - **A 404 or an empty release means "nothing for this product", not an
    error**: the product is marked unavailable for 6 hours, in memory, with a
    single Info line. For products the chain gets `defaultServer` appended
    before concluding, since a site's server may simply not have the product's
    manifest yet. EMLy's 404 behaviour is unchanged.
  - **Three failed attempts per version and the agent stops** (products only):
    `attempts`/`gaveUp` in `state.json`, one Error log, and the product is left
    alone until the manifest offers a **different** version, which resets the
    count - the same scheme as `selfUpdate`. EMLy keeps retrying every cycle.
  - **`cleanReinstall` defaults to false**, and the second attempt then
    re-downloads and re-runs the setup *without* uninstalling. An NSIS
    uninstaller typically removes `$INSTDIR`; for 3g-RocketChat that would
    delete the IT-managed `config.ini` (recreated from defaults by the setup)
    and the user's `data\`. Turn it on per product only when the installer is
    known to keep user data. EMLy always does the clean retry.
  - **`state.json` layout**: EMLy's pending entry stays in the historical
    `pending` field, byte-identical to before, so rolling the agent back does
    not lose a downloaded EMLy install; every other product is in `products`,
    per slug, with `attempts`/`gaveUp`. Rolling back to an older agent does
    not merely ignore `products`: its first read-modify-write of `state.json`
    drops the field, losing the products' pending entries and attempt counts.
    Harmless - they are re-downloaded, and the count starts over.
  - **Download managers**: EMLy keeps `downloads\` itself (prefix `EMLy-`);
    every other product has its own `downloads\<slug>\` subdirectory (prefix
    `<slug>-`), all sharing the single `Pacer`. A subdirectory and not just a
    prefix in one directory: slugs may contain `-`, so `a-` is a prefix of
    `a-b-1.0.0-setup.exe` and product `a`'s cleanup would delete product
    `a-b`'s pending setup.
  - **The retry's re-download never wipes before it succeeds** (products
    only): the cached setup is moved aside, the fresh fetch attempted, and on
    failure (a 429 from a busy server, offline) the old copy and the pending
    entry - `attempts` included - stay, so the retry runs the cached copy and
    the 3-attempt cap keeps counting. Wiping first would reset the count on
    every refused re-download. EMLy keeps its wipe-then-fetch order.
    `service.Updater.install` is a thin wrapper over `installProduct`
    (`internal/service/product.go`).
- **The presence channel is off until a document turns it on, and follows the
  same server the poll does** — `internal/wsclient` holds one WebSocket open
  to `GET /v2/client/ws` on `cyc.chain[0]`, the very server `beginCycle`
  picked for this cycle, so a machine that changes site moves its presence
  connection with it and there is no second address to configure anywhere.
  The kill switch is the document's `clientWs.enabled`, **false by default**
  in `policy.Defaults` and in the legacy-derived policy: upgrading the
  updater must never, by itself, open ~400 permanent connections to the API.
  It is in `PatchableSections`, same as the API's `AllowedPatchKeys`, so a
  site can pilot the channel on a handful of hosts before flipping it
  fleet-wide — `match: {hostnames: [...]}` or `match: {hwids: [...]}`,
  `patch: {clientWs: {enabled: true}}`, global switch left off. The switch
  is honoured hot: `clientws.go` re-reads the current cycle every 15s
  while a connection is up, so a published revision closes the channel
  without waiting for the connection to drop on its own (event 923).
- **A 404 on the WebSocket upgrade disables that server for an hour, not the
  feature, and not permanently** — this is its own convention, not the one
  `internal/source` defines: `internal/source` falls through to the chain's
  backups on a 404, and re-evaluates from scratch on the next poll; the
  presence supervisor does neither. It stays pinned to `chain[0]` (see
  `clientWSTarget`'s doc comment for why - a machine's telemetry has to live
  on the same instance that serves it) and remembers the 404 in `unsupported`
  (`internal/service/clientws.go`) for `clientWSUnsupportedRetryAfter` (1h),
  logging event 922 only the first time a server is marked, not on every
  re-mark - a mirror lagging for days must not refill the Event Log every
  hour. The mark expiring is what keeps a transient 404 (an ingress or load
  balancer mid-deploy) from blinding a machine until its service restarts;
  `beginCycle` picking a different server still clears it sooner, same as
  before. A `401` or `403` is the opposite case — a real misconfiguration, or
  the API's rate limiter answering a ban (see `wsclient.Backoff`'s doc
  comment) — and keeps retrying, loudly, without a memo.
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
  the upgrade request, exactly as on every other call. **HWID and hostname
  are the one deliberate exception that duplicates *into* headers instead** —
  `wsclient.Client.dial` also sends `X-EMLy-HWID`/`X-EMLy-Hostname` (from the
  same `Identity`), even though the rest of it only travels in the
  post-handshake payload. That is not a violation of "identity travels in
  the payload, not headers" to clean up: the API's ban list can only enforce
  a ban by HWID or hostname on a route it has not upgraded yet, since the
  identity payload does not exist before the handshake completes, and these
  two headers are what let such a ban reach this connection at all. It is
  duplication for enforcement, the same reason `updater_version` already
  travels in the User-Agent rather than only the payload.
- **Protocol v2 is negotiated, not assumed** — `wsclient.Identity.Protocol`/`Capabilities`
  go out on connect (`ProtocolV2`, `Updater.capabilities()` — every command, event and
  topic name this build implements, deduplicated since `machine.info` names both a command
  and an event); nothing v2 (a command, an event, a notify) is sent or accepted before the
  server's `welcome` negotiates it, so a server still on v1 just sees an ordinary v1
  presence connection, unaware anything else was ever offered.
- **The welcome burst runs off the connection's read goroutine, never on it** —
  `clientHandler.Welcome` (`internal/service/clientevents.go`) only starts `welcomeBurst` on
  its own goroutine and returns immediately: it is called from the connection's read loop,
  the same one answering pings, and the burst itself can take several 10s send timeouts -
  `service.started` first (built once per process and cached via `sync.Once`, since
  `TakePendingCommands` is destructive and a retry must report the same
  `completed_commands`), then the pre-connection event buffer flushed, then the session
  published for `emit` to use, then a second flush for whatever was emitted in that gap,
  finally `machine.info`. A `gen` counter (`nextWSGeneration`/`storeWSSession`/
  `clearWSSession`) ties a burst to the specific connection attempt that started it, so a
  burst still running for a connection `runClientWS` has already torn down can never publish
  a session object nobody can reach any more.
- **The channel wakes, it never runs a cycle** — `handleNotify`
  (`internal/service/clientnotify.go`) only ever calls `scheduleWake`, which delivers into
  `u.wake` (a 1-slot channel: a burst of notifies collapses into a single wake) after a
  random delay - at least 60s (`minReleaseJitter`) for `release.published`, whatever
  `jitter_seconds` the document sends for `config.published`. `emly.manifest.check`/
  `updater.manifest.check` are the read-only counterpart: dry runs via
  `resolveTargetWith`/`resolveUpdaterManifestWith` with `notePreferred=false` - no download,
  no `state.json` write, no preferred-server change, no presence-channel wake - which is
  what makes it safe for them to run while a `Cycle` is already in progress instead of
  answering `busy`.
- **Commands are allowlisted by the document, destructive ones need TLS** — `admitCommand`
  (`internal/service/clientcmd.go`) refuses anything outside the effective document's
  `clientWs.commands` (`disabled_by_policy`; `policy.DefaultClientWSCommands` is read-only
  only) before it refuses `service.restart`/`machine.reboot` over `ws://`
  (`insecure_transport`, `commandSession.Secure()`). A destructive command's ID is written to
  `state.json`'s `pendingCommands` *before* acting (`runDestructive`,
  `internal/service/clientpower.go`) - the connection it arrived on may not survive the
  action itself - and reported back in the next `service.started`'s `completed_commands`; a
  64-entry dedupe ring (`commandRing`), seeded non-destructively from `state.json` at startup
  (`seedCommandRing`), stops a redelivered command from being re-run.
  `destructivePending`/`destructiveMu` make "start an install" and "commit a destructive
  command" mutually exclusive (`beginInstall`/`commitDestructive`): `Cycle`, `install` and
  `applySelfUpdate` all skip while one is pending (`installing` spans the whole `install()`
  and the self-update launch, staying above zero after a successful launch), and a second
  destructive command reaching `admitDestructive` is refused `busy`. The pending flag
  auto-expires if the reboot/restart never actually completes - the requested delay plus 5
  minutes for a reboot, 3 minutes for a restart - logging event 926, so an aborted shutdown
  or a `restart-service` child that died cannot wedge the machine `busy` forever.
  `machine.reboot` uses Windows' own `InitiateSystemShutdownEx` countdown (no WTS dialog of
  the updater's own - see `internal/power`), forcing apps closed once it elapses, raised to
  at least 60s whenever a user is actually active regardless of the requested delay;
  `service.restart` launches the hidden `restart-service` subcommand detached (never
  waited on, same reason `selfupdate.Launch` never waits either), which logs to the normal
  ProgramData log/Event Log on its own and retries `cmdStart` three times, 5s apart.
  `state.json` now holds **three** independent lifecycles: the pending EMLy update, the
  updater's own self-update record, and pending commands.
- **Events are best-effort** — `emit` (`internal/service/clientevents.go`) buffers in memory
  (32 events, oldest dropped first) whenever there is no live v2 session, and flushes on the
  next welcome; nothing is ever written to disk, and a service restart loses whatever was
  buffered. The poll path is unaffected either way - it was the source of truth for updates
  before this channel existed and still is.
- **A command/event/field added to the protocol touches both repos** —
  `wsclient/protocol.go` here (`Cmd*`/`Evt*`/`Topic*` names and the `CommandNames`/
  `EventNames`/`TopicNames` slices), `internal/clientproto` in `emly-go-api`, and
  `emly-go-api/CLIENT_WS_PROTOCOL.md`. `policy.knownClientWSCommands` is pinned to
  `wsclient.CommandNames` by a test, so the document's allowlist vocabulary cannot drift
  from the wire names silently.
- **`SessionChangeKind` values are a wire contract** — since protocol v2 they travel
  verbatim in `session.changed`'s `events` (`internal/machineinfo/sessionchange.go`,
  CLIENT_WS_PROTOCOL.md §8.1); renaming one is a protocol change, not a local refactor.
- **`config.ini` is never written at runtime** - the source decision lives in
  memory and in the log (event 700), nowhere else. The service never writes
  that file: `config.Reset` (on install) and `config.WriteEdits` (the tray's
  elevated `apply-settings`, i.e. an administrator editing it) are its only
  writers. `config.SetPrimary` is gone: a config file the service rewrites is a
  config file with two owners and no truth.
- **Fail-open, always** - an unreachable endpoint, a `204`, a `304`, a stale
  cache: none of them pause anything. Only an explicit, valid
  `control.updater.enabled = false` does, and even then the config fetch, IPC
  and the self-heal steps keep running, because that is what can un-pause the
  machine. An expired `until` re-enables the block on its own.
- **The local clock is only trusted for `until` and staleness** - staleness is
  measured from the cache's own `fetchedAt` (local clock at both ends, so the
  error cancels), never from the document's `generatedAt`. PC clocks drift by
  years.
- **The remote document can only narrow the compiled-in IPC compatibility** -
  `ipc.EffectiveCompat` intersects `ipcProtocol` with the constants in
  `internal/ipc/version.go`: it may disable a protocol version or raise
  `emly.min`, never enable a version this binary does not implement nor lower
  the minimum below what the code requires. `emly.max` is informational, so
  the document replaces it in either direction.
- **Config is never shipped** - `config.default.ini` is embedded via `//go:embed` and written to `%ProgramData%\EMLyUpdater\config.ini` only when the file is absent. Per-machine edits do **not** survive upgrades: `cmdInstall` calls `config.Reset`, which backs the existing file up to `config.prev.ini` and rewrites `config.ini` from this build's embedded defaults. A setting that must persist has to be re-applied after the upgrade.
- **ProgramData survives uninstall** - `cmdUninstall` deletes the service but never removes `%ProgramData%\EMLyUpdater`. The InnoSetup `[UninstallRun]` block does the same.
- **Exe-dir log is preserved on uninstall** - `cmdUninstall` copies `<ExeDir>\updater.log` to `%ProgramData%\EMLyUpdater\logs\updater-final.log` before the InnoSetup uninstaller can delete the exe directory.
- **SHA256 is mandatory** - a setup whose checksum is missing or wrong is never executed. This applies to resumed pending installs too (re-verified before use).
- **The Updater's target version always wins over what's already installed** - `installProduct` (`internal/service/product.go`; `Updater.install` is a thin wrapper for EMLy) trusts the product's `Detect` chain (for EMLy, config.ini's `GUI_SEMVER`; the required result is `installed(v)` with `v >= target`), not the setup's own exit code. If a run doesn't leave the chain reporting the target version - setup failure or a clean exit that still doesn't match (e.g. EMLy's installer treating a stale/inconsistent prior install as already current; NSIS can also exit 0 on a partial failure) - the retry depends on the product: EMLy, and any product with `cleanReinstall`, wipes the existing install through `driverFor(p).Uninstall()` (EMLy's own `unins*.exe`, or NSIS's `uninstall.exe`; best-effort, a missing uninstaller is not an error) and runs the setup once against a clean slate; other products skip the uninstall and only re-download and re-run the setup. The uninstall step is additionally refused, with a Warn, when `installer.CheckNotUserWritable` does not pass (see Common Pitfalls); the reinstall proceeds without it. Still mismatched after that → the pending entry stays and the whole thing is retried on the next poll cycle (products: up to 3 attempts per version, see *Products other than EMLy*).
- **Atomic state writes** - `state.Store` writes to a temp file then renames, so a crash mid-write cannot corrupt the pending entry.
- **The update source is decided every cycle, from the policy** - `beginCycle`
  (`internal/service/sourcepolicy.go`) resolves the nearest domain controller and this
  machine's local addresses, matches them against the effective document's `dcLookupMap`
  (DC name is a key *and* a local IP is inside one of that site's subnets), and builds the
  server chain: the site's `baseServer` as the resolver's primary, its `backupServer` list
  as ordered one-shot fallbacks. No site matched (DC not in the map, no local IP on its
  subnets, machine off the domain) means `defaultServer`, alone. The public API is **not**
  appended implicitly any more - a site that wants it lists it in `backupServer`; the
  legacy-derived policy does, so nothing changes for a machine with no document. The
  decision is logged as event 700 when it *changes* (plus once at startup), not on every
  poll, so a stable machine does not emit it 96 times a day. Because it runs per cycle, a
  laptop that changes subnet follows it without a service restart.
- **A failed DC lookup at boot is retried before it is believed** - `resolveDC`
  (`internal/service/sourcepolicy.go`) retries `DsGetDcName` `updater.dcLookupRetry.attempts`
  times, `delaySeconds` apart (6 × 5s by default), because at boot the service can start
  before the network/DNS/netlogon are ready and the API then reports the domain as
  non-existent - which the site match cannot tell apart from "this machine is off the domain"
  and would pin the machine to `defaultServer` for the whole run. The retry is gated on
  `machineinfo.DomainJoined` (cached `Win32_ComputerSystem.Domain`, no network needed), so a
  workgroup machine does not sit out the window, and on `ctx` so a stop request during it is
  honoured. This is why `beginCycle` is first called from `RunLoop` and not from `service.New`: `New` runs
  before `svc.Run` reports the service started, and blocking there is what the SCM start
  timeout counts against. `cmdInstall` also registers the service as **delayed auto-start**
  with `Dnscache` + `LanmanWorkstation` dependencies for the same reason; `Netlogon` is
  deliberately not a dependency, as it is Manual on non-domain machines and would block the
  service from starting there at all. The retry window applies to the **first**
  lookup only: later cycles get a single attempt, since a machine that has moved
  must not stall a cycle waiting out a doomed retry, and the cached DC answer is
  only re-queried when the local addresses change or it is an hour old.
- **`config.Load` reads the ini file with `IgnoreInlineComment: true`** - without it, ini.v1
  treats a bare `;` anywhere in a value as the start of an inline comment and silently truncates
  the rest of the line, no error. `defaultMappingDCSubnets` now uses `|` between DC entries, but
  the legacy `;` delimiter written by previous releases is still accepted, so this isn't
  hypothetical: a carried-over two-site value like `DC-RM2:...;DC-CB:...` would load as just
  `DC-RM2:...`, and every site after the first would look "not configured" with nothing in the
  log to explain why - `Load`'s comments live on their own line, never after a value on the same
  line, so this is safe for the whole file, not just this one key.
- **An unreachable primary address doesn't fail the cycle** - `source.Resolver`
  (`internal/source/resolver.go`) takes an ordered `Fallbacks` list, each tried once (no
  retries) after `Primary` exhausts its attempts. `Updater.newResolver` fills it from the
  site's `backupServer` list: the site match can be correct while the site's primary address
  is unroutable, misresolved or firewalled. (In the real deployment every entry is the same
  machine; see *Deployment topology*.) A fallback is used for that fetch only and never changes the
  policy, so the next cycle still tries the site's own server first.
- **A setup download refused with 429 is waited out, never retried early** - the API caps
  concurrent installer downloads with one pool of slots shared by EMLy and the updater
  (`emly-go-api` `internal/downloadqueue`, spec `2026-09-30-download-queue-updater-spec.md`
  there) and answers the overflow `429` + `Retry-After` + `{"error":"download queue full",...}`.
  `HTTPSource.FetchSetup` turns that into a `*source.RetryLaterError` (wait: header → body
  `retry_after` → 60s; non-positive/non-numeric skipped). `download.Manager.fetch`
  (`internal/download/pacer.go`) then waits `Retry-After` + 0–30s jitter and retries, at most
  `MaxRetryLater` (5) requests per `Ensure`, and returns at once when the wait exceeds
  `MaxInCycleWait` (5 min) rather than stalling the poll loop. The deadline is kept in a
  `download.Pacer` that both managers share, in memory only: a later cycle, a notify wake-up
  or the other product's download does not ask before it has passed. `download.IsQueueFull`
  is what `Cycle` and `applySelfUpdate` check to log at **info** and report nothing - no
  `update cycle failed`, no `update.failed` - because a full queue is the server pacing the
  fleet over the MPLS, not a failure. A 429 without that body (the API's per-IP rate
  limiters) is paced the same way but still ends as an ordinary download failure.
- **The setup download has an inactivity timeout, never a total one** - the API bounds a
  download's total duration itself (10 min by default, raisable from the dashboard up to 24 h),
  so a client cap shorter than that would truncate exactly the slow-but-alive downloads a
  saturated MPLS produces. `NewHTTPSource` therefore builds its `http.Client` with **no**
  `Timeout`. Every request bounds itself instead: `getJSON` (30 s) and `FetchConfig` with a
  context deadline, and `FetchSetup` with a watchdog that cancels after `SetupIdleTimeout`
  (2 min) without a single byte (connecting, awaiting the headers, or between body reads),
  reported as `ErrSetupStalled`. Do not put a `Timeout` back on the shared client, and give
  any new request on it its own deadline. A download the server cuts short (its own limit, or
  an admin freeing the slot) arrives as a `200` with a short body: `FetchSetup` fails it on
  `Content-Length` and `Ensure` on SHA-256, so it is discarded and fetched again next cycle.
- **No update source reachable at all → toast + event, once per outage** - when `resolveTarget`
  still fails (primary exhausted, fallback also failed or unconfigured), `Cycle`
  (`internal/service/service.go`) logs event 101 (`EventSourcesUnreachable`, every cycle) and
  calls `notifySourcesUnreachable`, which shows a localized "contact your IT" toast
  (`notify.SourcesUnreachableMessage`) via the same `notify.LaunchToast` SYSTEM → user-session hop
  as the update-complete toast. `Updater.sourcesUnreachableNotified` gates the *toast* (not the
  log line) to once per outage: set only when `LaunchToast` actually shows it, cleared by `Cycle`
  the next time `resolveTarget` succeeds. If nobody was logged in to see it, the flag stays false
  and the next cycle tries again - so a long outage nags once, but only once someone is actually
  there to read it.
- **Singleton guard** - a named kernel mutex `Global\EMLyUpdaterSingleton` prevents `run` (foreground debug) from racing the installed service.
- **`apps.list_upgradable` asks winget twice: as SYSTEM and as the logged-on user** -
  LocalSystem sees what is installed for every user (HKLM, provisioned MSIX) but nothing a
  user installed for themselves alone (HKCU, per-user MSIX), so `listUpgradable`
  (`internal/service/clientcmd.go`) runs the same script in parallel inside the
  interactive user's session too, through `notify.RunInSession` (the `LaunchToast` token
  hop, waited on, stdout/stderr piped back). The session is
  `machineinfo.InteractiveSession`, the same `pickSession` ranking `X-EMLy-LoggedUser`
  uses: the active console user first, an RDP user only when nobody is at the console,
  a disconnected session last. The two lists merge on id (case-insensitive) + installed
  version - the user's session sees machine-wide packages too - and each package carries
  `scope` (`machine`/`user`, a wire contract with the API). The machine half decides the
  outcome, as before; the user half is best-effort and reports itself in `user_scope`,
  `user` and `user_scope_error`, so a missing user list is never read as "nothing to
  update". `RunInSession` restricts inheritance to the child's three standard handles
  with `PROC_THREAD_ATTRIBUTE_HANDLE_LIST`, like `os/exec`: plain `bInheritHandles`
  would leak into the child whatever pipes the SYSTEM-side `os/exec` run has made
  inheritable at that instant.
- **`state.json` holds three independent lifecycles** - EMLy's pending update, the updater's own
  self-update record, and the client channel's pending destructive commands (see above). Every
  setter goes through `Store.update` (read-modify-write); building a fresh `State` and saving it,
  which is what `SetPending` used to do, would silently drop whichever entries the rest of the
  cycle had just written. `Store` writes are serialised by its own mutex, held across Load/Save/
  update: the welcome-burst goroutine (`TakePendingCommands`), the command goroutines (`Add`/
  `RemovePendingCommand`) and the poll goroutine (`SetPending`/`SetSelfUpdate`/...) can all be in
  flight at once, and without it two read-modify-write calls racing on the same `Load` could each
  start from the same snapshot and have one write silently lost. A missing file loads as empty.
  A file that does not parse (`state.ErrCorrupt`) is moved aside to `state.json.corrupt`
  (`Store.OnCorrupt` logs it) and the write rebuilds from an empty `State`: failing closed there
  would block self-update and `service.restart`/`machine.reboot` forever, since all of them
  refuse to act without first recording state. Any other `Load` error (sharing violation, access
  denied, disk error) may be transient, so `update` returns it with nothing written.

## Self-update

The service updates itself by running its own InnoSetup installer, which already knows how to stop
the service, replace the binary and start it again. `internal/selfupdate` holds the rules (pure,
tested) and the launch; `internal/service/selfupdate.go` orchestrates. Design notes worth keeping:

- **It runs first in `Cycle`**, ahead of even a pending EMLy install: a build with a bug in the EMLy
  path has to be able to replace itself before exercising that bug again. A pending entry is
  persisted and resumes under the new binary. `Cycle` returns immediately when `selfUpdate` reports
  a launch - the setup is already stopping the service.
- **The launch must never be waited on.** `selfupdate.Launch` uses `DETACHED_PROCESS` and
  `Process.Release()`, never `Wait`. The setup's first act is `EMLyUpdater.exe stop`; the stop
  handler cancels the loop and waits for it to return, so a blocking launch would deadlock the two
  until the 60-second stop timeout expired and the install failed. This is why the installer drivers
  (`installer.For(...).Install`, which wait) are deliberately not reused here.
- **The outcome is only knowable at the next start.** The launching process does not survive, so
  `state.json`'s `selfUpdate` record is written *before* the launch and reconciled after the restart
  by comparing `version.Version` against it. If the record cannot be persisted, the setup is not
  launched at all - without it the attempt could not be counted.
- **The remote-config cache is moved aside right before the launch**
  (`retireCache`, `internal/service/remoteconfig.go`): `remote-config.json` →
  `remote-config.prev.json` (one copy, overwritten, never read back), so the new
  build starts from the default policy and takes its configuration from the API
  instead of from what the old build accepted. This is a deliberate exception to
  "the cache is the policy when the endpoint is unreachable": if the API is down
  when the new build comes up, it runs on the policy derived from the freshly reset
  `config.ini` until the first successful fetch, and that first document is
  accepted whatever its revision. A failed move is logged and does not hold the
  update back; a failed launch puts the file back (`restoreCache`) unless a cycle
  has already written a newer one.
- **Attempts are bounded** (`selfupdate.MaxAttempts`, 3) with a 10-minute cooldown between launches.
  A release that installs but never results in the new binary running would otherwise stop and
  restart the service on every poll cycle, fleet-wide, forever. A *different* version in the
  manifest resets the count - that is how an operator recovers a stuck fleet.
- **Two checks gate execution: SHA256 and Authenticode.** The internal source is plain HTTP, so
  whoever can serve a tampered setup can serve a matching checksum with it. `internal/authenticode`
  verifies with `WinVerifyTrust` *and* pins the signer's SHA-1 thumbprint to the embedded 3gIT
  certificate - `WinVerifyTrust` alone accepts anything chaining to any trusted root, which on a
  domain PC is every public CA. `CERT_E_UNTRUSTEDROOT` is tolerated only when the pin matches, so
  the check still works with `certificate.enabled = false`.
- **Config reset lives in `config.Reset`, not in `installer.iss`.** The reset to defaults on every
  install (self-update included) is deliberate; keep it in Go where it can back the old file up to
  `config.prev.ini` first, rather than in an `[InstallDelete]` entry that would silently drop it.
- **The updater manifest URL is derived, not configured** (`config.UpdaterManifestURL`): the manifest
  URL in use plus an `updater` path segment, so a site's mirror serves both documents and there is no
  second URL to keep in sync. `selfUpdate.manifestURL` overrides it for every source.
- **A 404 is an answer, not a failure.** `source.ErrNotFound` skips the retries (backing off and
  asking again cannot change it) but still tries the fallback; `manifest.ErrNoRelease` (a manifest
  with nothing published) does the same, EMLy's primary server included - there it is still an
  error, it just is not asked again within the cycle; when nothing serves the endpoint the
  cycle logs it and moves on, which is what lets a mirror that has not been updated yet coexist.
  Nothing in the self-update path ever fails a cycle - keeping EMLy updated is the job, updating
  itself is only how it stays good at it.
- **Every cycle that does not self-update says why, at Info**, under the single message
  `no updater self-update this cycle`, with `manifestURL` naming the endpoint that answered.
  Debug is not enough: the installed service logs at Info and nobody is going to stop it and
  re-run it in the foreground to find out whether it is even looking. `Resolver.Document`
  distinguishes the two manifest fetches a cycle makes, which would otherwise produce two
  identical `... served by primary source ...` lines. `ResolveUpdater` returns the URL that
  actually answered because a `Source`'s `Name()` only carries the *EMLy* manifest URL it was
  built from - never the `/updater` endpoint the fetch really went to.

## Client channel (presence + protocol v2)

The presence channel and protocol v2 (commands, events, notifies) are the same connection -
see `internal/wsclient`'s package doc and the "Protocol v2 is negotiated..." conventions
above. The normative wire reference is `emly-go-api/CLIENT_WS_PROTOCOL.md`.

### Client channel manual verification (admin required)

Nothing here is covered by `go test` for the same reason `internal/power` isn't: a reboot or
a service restart on the test machine is not something CI can be allowed to do. This
checklist is their verification.

1. With a document enabling `clientWs` and `commands` including `machine.reboot`, over
   `wss://`: issue `machine.info` via `POST /v2/client/{id}/commands` and confirm a `done`
   result carrying the payload.
2. `apps.list_upgradable` on a machine without Microsoft.WinGet.Client installed →
   `failed` with `winget_module_missing`; with the module present → the package list.
   The script first imports the module by hand from the highest version folder under
   `%ProgramFiles%\WindowsPowerShell\Modules\Microsoft.WinGet.Client\` when autoloading
   does not find `Get-WinGetPackage` (e.g. the service's PSModulePath misses it); a
   failing `Import-Module` comes back as `internal` with PowerShell's own error.
   Without PowerShell 7 → `failed` with `powershell_not_found`: as SYSTEM the module
   refuses Windows PowerShell 5.1 (`WindowsPowerShellNotSupported`).
   With a user logged on, the list must also carry that user's own packages
   (`scope: "user"`, e.g. something installed per-user under `HKCU` or a per-user MSIX
   app), `user_scope: true` and `user` naming them; log them off (or lock the machine
   with nobody logged on) and it must come back with `user_scope: false` and
   `user_scope_error: "no user is logged on"`. Repeat with the console user and an RDP
   user both logged on: the console user's packages win. `notify.RunInSession` is the
   part of this that no test covers - it needs SYSTEM's `SeTcbPrivilege`.
3. `service.restart` → the connection closes with 1001, the service actually restarts, and
   the new process's `service.started` has `reason: "command"` listing the command's id;
   the command itself shows `done`.
4. `machine.reboot {"delay_seconds":120}` with a user logged on → Windows' own countdown is
   shown, the machine reboots, the next `service.started` has `reason: "boot"`, and the
   command shows `done`. Repeat with `"when_user_active":"skip"` → refused `user_active`,
   nothing happens.
5. The same `machine.reboot` issued through a `ws://` (not `wss://`) mirror → refused
   `insecure_transport`, nothing happens.
6. Disconnect and reconnect over RDP → a `session.changed` event carrying `active-rdp`
   appears in `GET /v2/client/{id}/events`, and `updater_clients.logged_user_state` updates
   without `last_seen_at` moving.
7. `POST /v2/client/notify` a `release.published` for the current updater version + 1 →
   the loop wakes within the jitter window, `update.available`/`update.started` events
   appear, then `update.applied` once the new build comes back up.

## Tray (`EMLyUpdater.exe tray`)

`internal/tray`. A notification-area icon started at every logon (HKLM `Run`,
written by the installer) with a settings window over `config.ini`, a "check
now", and a products window. It is a **client of the service, never a second
agent**: it installs nothing, and the service gains no new inbound surface for it.

- **Reading**: `config.Load` plus a `service.Updater` of its own (what
  `products` builds too), on which it calls `Prepare(ctx, false)` and then
  `EffectivePolicy`/`ProductRows`. `Prepare` makes the Updater **read-only**: it
  must not even move an invalid cache aside (`quarantineCache`), because an
  administrator running the tray (or `products`) would otherwise rename the
  service's `remote-config.json`. `bootRetry=false` believes a failed DC lookup
  at once: the boot retry window is up to 30 s of "Caricamento…" for nothing on
  a machine that booted long ago. Everything runs off the UI thread, serialised
  by `backend.mu`.
- **The settings shown are the effective ones**: when a remote document is in
  force (`PolicyView.Governed`, i.e. the policy source is not `default`), the keys
  it overrides (`governedKeys` in `settings.go`: poll, channel, primary, DC retry,
  self-update, critical warning, certificate) show the document's value and are
  disabled. Unticking "Configurazione remota" re-enables them with config.ini's
  values, since that is what the service will use after the save. `remoteConfig`,
  `ipc` and `progressWindow` have no counterpart in the document and are always
  editable. The list mirrors `policy.DefaultsFromConfig`: a key added there
  belongs in `governedKeys` too.
- **Writing** goes through `EMLyUpdater.exe apply-settings --result <tmp>
  section.key=value...`, launched with `ShellExecuteEx("runas")`, because
  `config.ini` is `Users:(RX)`. Only `config.EditableKeys` are accepted: URLs, the
  API key and the pipe name are not something a settings window should re-point,
  and the elevated process trusts its command line. `config.ApplyEdits` changes
  only the value part of each line, so comments and alignment survive;
  `config.WriteEdits` validates the result with `config.Parse` (the service's own
  loader) before the atomic replace, and the tray runs the same check unelevated
  first so a bad value never costs a UAC prompt. The edit is logged with the
  account (`apply-settings: config.ini edited from the tray`), then the service is
  restarted, since it reads `config.ini` only at start. **The edit lasts until the
  next install or self-update**, which resets the file (`config.Reset`); the
  window says so.
- **"Controlla ora" is a service control code, not IPC**: `service.CheckNowControl`
  (200, in the user-defined range). The default service DACL already grants
  interactive users `SERVICE_USER_DEFINED_CONTROL` (`sc sdshow EMLyUpdater` →
  `(A;;CCLCSWLOCRRC;;;IU)`, where `CR` is that right), so a non-elevated tray can
  send it with no change to the pipe (which admits only `EMLy.exe`), its proto, or
  the service's DACL. It carries no data, so there is nothing to validate.
  `RequestCheck` wakes `RunLoop` with reason `check-now` (reported as trigger
  `cycle`, like `product-exit`) and forces the next config fetch, at most once a
  minute (`checkNowThrottle`). It runs a full ordinary cycle, downloads and
  installs included, paced by the server's 429s like any other. An older service
  ignores the code.
- **"Verifica" in the products window is a dry run**: `CheckProduct` /
  `CheckUpdater` (`notePreferred=false`, no download, no `state.json` write). It
  does reach the server, with the usual `X-EMLy-*` identity headers, from the
  user's session: one small manifest GET per row checked.
- **The tray locks `EMLyUpdater.exe`**, which every install replaces. Inno Setup's
  Restart Manager (`CloseApplications`/`RestartApplications`, both spelled out in
  `installer.iss`) closes it: the tray quits on `WM_ENDSESSION` (hiding on
  `WM_CLOSE` is only for the user's close button), and it calls
  `RegisterApplicationRestart("tray")` at start so Windows brings it back
  afterwards. If it is not restarted in some session, it is back at the next logon.
- **It is a console binary**: the `Run` value starts it as `conhost.exe --headless
  "...\EMLyUpdater.exe" tray`, otherwise a console window stays open beside the
  icon. One tray per session (`Local\AryxDAgentTray` mutex).
- **`ListViewItem.SetData` is not used** in the products window: in testing the
  agent's row read back EMLy's slug, so the window keeps its own index → slug
  slice instead.
- **Theme follows Windows' app mode, live** (`theme.go`): light or dark from
  `AppsUseLightTheme`, re-applied on `WM_SETTINGCHANGE("ImmersiveColorSet")`;
  DWM dark title bar, rounded corners and Mica. Three things no visual style
  does on Windows 11 26100, so they are drawn by hand in dark mode only:
  checkbox captions (a themed checkbox ignores `WM_CTLCOLORBTN`, so each row is a
  bare glyph + a Static, `checkRow`; a disabled row's label is painted dim rather
  than disabled, because a disabled Static is drawn etched), push buttons
  (`darkButton`, NM_CUSTOMDRAW), and list view column titles (`darkListHeader`,
  NM_CUSTOMDRAW on the header via a list view subclass - windigo dispatches
  WM_NOTIFY only by (ID, code), so `fixHeaderID` pins the header's ID first). The
  undocumented uxtheme ordinals (104, 133, 135, 136) are looked up by ordinal and
  skipped when missing. All of it depends on the Common Controls 6 manifest (see
  Common Pitfalls → `versioninfo.json`).
- **Waiting on the backend shows a loading bar** (`loading.go`): an owner-drawn,
  indeterminate bar in the style of the Explorer copy dialog, while the settings
  load or save, and while products load or a manifest check runs. Not
  `msctls_progress32` + `PBS_MARQUEE`, which stays light in the dark palette.
  `Start`/`Stop` nest, so several checks in flight keep one animation. One per
  window: windigo keeps a single WM_DRAWITEM handler per window.

### Tray manual verification

`go test` covers only `config` (edits) and `service` (`RequestCheck`, `Prepare`):
the window, UAC and the Restart Manager need a desktop.

1. Install, log on as a **standard user**: the icon appears, with no console
   window. The menu's first line shows the service state.
2. Settings with a cached document: the governed fields show the document's
   values, greyed out; untick "Configurazione remota" and they become editable
   with config.ini's values.
3. Change the poll interval and save: UAC prompt (admin credentials), then the
   balloon, the `apply-settings: config.ini edited from the tray` line in the
   service log, a service restart, and `config.ini` with only that value changed.
   Cancel the UAC prompt: nothing changes and the edit stays on screen. Enter `0`:
   the error is shown before any UAC prompt.
4. "Controlla ora": `update check requested from the tray, waking the update
   loop`, then a cycle, in the service log; a second click within a minute logs
   `throttled`. With the service stopped, the balloon says so and nothing is sent.
5. Prodotti → "Verifica tutti": the agent's row shows the updater manifest's
   version, each product its own. The service log shows nothing (it was not
   involved); the tray's own log is `%TEMP%\aryxd-agent-tray\updater.log`.
6. Self-update with the tray open, from the service (silent, as SYSTEM): the setup
   does not fail on a locked `EMLyUpdater.exe`, and the tray comes back, in the
   console session and in an RDP session. **Not verified yet.**

## Configuration Reference

`%ProgramData%\EMLyUpdater\config.ini` - full annotated defaults in [internal/config/config.default.ini](internal/config/config.default.ini).

| Key | Section | Default | Notes |
|-----|---------|---------|-------|
| `emlyInstallDir` | `[updater]` | `C:\3gIT\EMLy` | EMLy executable location |
| `emlyConfigFile` | `[updater]` | `C:\3gIT\EMLy\config.ini` | Read for `GUI_SEMVER`, `GUI_RELEASE_CHANNEL`, `LANGUAGE` |
| `pollIntervalMinutes` | `[updater]` | `30` | |
| `channelOverride` | `[updater]` | _(empty)_ | Force `stable` or `beta` fleet-wide |
| `primary` | `[source]` | `external` | `external` or `internal` |
| `externalManifestURL` | `[source]` | (API URL) | Required when `primary=external` |
| `internalManifestURL` | `[source]` | _(empty)_ | Required when `primary=internal` |
| `bkInternManifestURL` | `[source]` | _(empty)_ | Backup internal manifest URL: when `primary=internal` (DC and subnets matched) and `internalManifestURL` is unreachable, tried before falling back to `externalManifestURL` (empty disables) |
| `userAgent` | `[source]` | `EMLy-Updater/{{VERSION}} (...)` | Sent as `User-Agent` on HTTP requests; `{{VERSION}}` is resolved at runtime by `config.BuildUserAgent` |
| `xApiKey` | `[source]` | _(empty)_ | Sent as `X-Api-Key` on HTTP requests |
| `defaultMappingDCSubnets` | `[source]` | `DC-RM2:172.16.96.0/24` | Startup source policy: `dc:cidr[,cidr...][\|dc:cidr[,cidr...]...]` map of DC name to that site's internal subnets (legacy `;` delimiter still accepted; empty disables) |
| `dcLookupRetryAttempts` | `[source]` | `6` | Retries of a failed startup DC lookup (0 disables; skipped when not domain-joined) |
| `dcLookupRetryDelaySeconds` | `[source]` | `5` | Wait between those retries (0 also disables) |
| `criticalWarningEnabled` | `[criticalUpdate]` | `true` | Show countdown WTS dialog before force-kill |
| `criticalWarningSeconds` | `[criticalUpdate]` | `30` | |
| `enabled` | `[ipc]` | `true` | Enable the named-pipe IPC server (see IPC below) |
| `pipeName` | `[ipc]` | `EMLyUpdater` | Exposed as `\\.\pipe\<pipeName>`; must not contain `\` or `/` |
| `enabled` | `[certificate]` | `true` | Install the 3gIT code-signing certificate into `Root` + `TrustedPublisher` (machine + console user) |
| `enabled` | `[selfUpdate]` | `true` | Keep the updater itself up to date |
| `manifestURL` | `[selfUpdate]` | _(empty)_ | Empty = derived from the manifest URL in use (`.../manifest` → `.../manifest/updater`); set only to point at a different host, in which case it applies to every source with no fallback |
| `enabled` | `[progressWindow]` | `true` | Progress window for the user at the machine (console or RDP) while EMLy or the agent is downloaded/installed. **Local only**: deliberately absent from the remote document - see "Progress window" below |

## IPC (EMLyUpdater ⇄ EMLy)

`internal/ipc` serves `SystemInfo`/`ADStatus` (protobuf, `proto/updateripc.proto`) to the EMLy
desktop app over a named pipe, request/response per connection. The service runs as LocalSystem;
tampering with this channel is meant to require Administrator, not just a logged-in user:

- Pipe DACL (`internal/ipc/sddl.go`) grants Authenticated Users connect/read/write only — never
  `GENERIC_WRITE`, which on a pipe object implicitly includes `FILE_CREATE_PIPE_INSTANCE` and would
  let any user squat the pipe name.
- Every connection is authenticated (`internal/ipc/auth.go`): the connecting process's image path
  must match `assoc.ExePath(cfg.EMLyInstallDir, cfg.EMLyExeName)`. Any failure rejects the
  connection — never fails open. (There is deliberately no Authenticode signature/thumbprint
  check on top of the path check — it was removed as too much operational friction for the
  security it added on top of an already-admin-gated install path.)
- **`proto/updateripc.proto` is manually synced with the same file in the `emly` repo** — there is
  no shared Go module between the two repos. Copy changes verbatim both ways and regenerate both
  sides' `ipcpb` packages (`go generate ./internal/ipc/ipcpb`, requires `protoc`+`protoc-gen-go`,
  not required for `go build`/CI since generated code is committed).
- **Versioning**: every `Envelope` also carries `sender_version` — the sending binary's own semver
  (`internal/version/version_generated.go`'s `Version` const here, generated from `versioninfo.json` —
  see below — EMLy's `GUI_SEMVER` on the other side), distinct
  from `protocol_version` which only tracks wire/schema compatibility. Each side enforces the *min*
  half of its own compatibility consts (`MinCompatibleEMLyVersion` here, `MinCompatibleUpdaterVersion`
  in `emly`) and rejects an older peer with `UNSUPPORTED_VERSION` even when `protocol_version`
  matches — not every required fix changes the wire format. The *max* half is informational only
  (logged, never enforced): a newer peer is assumed forward-compatible unless proven otherwise. See
  the compatibility matrix atop `proto/updateripc.proto`, and bump `Version`/`MaxCompatible*Version`
  on every EMLyUpdater or EMLy release — see Common Pitfalls below.

### IPC manual verification (admin required)

1. As a **standard, non-admin** user, confirm a real EMLy.exe can query SystemInfo/ADStatus.
2. Squat test: pre-create a pipe named `\\.\pipe\EMLyUpdater` with a permissive SDDL before
   starting the service; confirm the service logs event 601 instead of silently becoming a second
   pipe instance.
3. Tamper test: copy EMLy.exe elsewhere and try to dial the pipe from there; confirm the client
   gets `UNAUTHORIZED` and the service logs event 600 with the offending PID/path.
4. Confirm `\\<hostname>\pipe\EMLyUpdater` is unreachable from a second machine.
5. `icacls`/AccessChk the live pipe to confirm the `0x120083` mask actually reaches Authenticated
   Users and nothing more — this is reasoned from documented access-right bit values, not
   otherwise verified.

## Code-signing certificate

`internal/cert` embeds the 3gIT code-signing certificate (`CN=3G IT Innovation`,
self-signed, DER) and installs it into `Root` **and** `TrustedPublisher`, for the
machine and for the console user, so EMLy's setup elevates as a verified
publisher instead of "Unknown publisher".

- **Both stores are required.** It is an end-entity certificate, not a CA, so its
  chain is one element long and terminates at itself: `Root` lets that chain
  validate, `TrustedPublisher` makes the publisher trusted. Neither alone works.
- **Per-user stores are reached by SID**, not by a session hop: `CertOpenStore`
  with `CERT_SYSTEM_STORE_USERS` and the store name `<SID>\Root`, the SID coming
  from `notify.ConsoleUserSID`. Those targets must also set
  `CERT_SYSTEM_STORE_UNPROTECTED_FLAG` — the user `Root` store is a *protected
  root* whose ordinary add path raises an interactive confirmation dialog, and
  session 0 has no desktop to draw one on.
- **The per-user targets are normally silent no-ops**, and that is expected, not
  a bug. A per-user system store is a *collection* that includes the machine
  store of the same name: once `LocalMachine\Root` holds the certificate,
  `<SID>\Root` already reports it and the add returns `CRYPT_E_EXISTS`. So a
  healthy run writes 2 stores, not 4. They matter only when a machine store
  could not be written. Related gotcha: the per-user `Root` store is a protected
  root and *denies* the add outright when the certificate is not already
  inherited from the machine store, even with `CERT_SYSTEM_STORE_UNPROTECTED_FLAG`
  — per-user `TrustedPublisher` accepts writes normally. Verified on Windows 11
  26200 as administrator and as SYSTEM.
- **Idempotency comes from `CERT_STORE_ADD_NEW`**, which returns `CRYPT_E_EXISTS`
  on a duplicate. That is the already-installed signal — there is deliberately no
  separate `CertFindCertificateInStore` lookup, which would only add a race.
- **It runs every cycle, not once at startup**, so a user who logs on after boot
  is covered and manual removal self-heals. The already-present path logs at
  Debug; only a real write logs Info + event 702.
- **SmartScreen is explicitly not addressed** — it is a cloud reputation service
  and does not consult local trust stores. Only a publicly-issued OV/EV
  certificate changes its behaviour.

### Progress window

`internal/progresswin` + `service/progress.go`. While an update of EMLy or of
the agent itself is downloaded and installed, the user at the machine sees a
fixed-size, non-closable window (no close/minimize, Alt+F4 ignored) with
EMLy's icon, a heading, a detail line and a progress bar. Its text is
**Italian only**, by request - unlike the toasts and the critical-update
warning, it does not follow EMLy's `LANGUAGE`.

- **Only `config.ini` decides** (`[progressWindow] enabled`). There is no
  counterpart in the remote document and there must not be one: the user asked
  for it to be a local decision. Remember that `config.ini` is rewritten from
  the embedded defaults on every install, so the default in
  `config.default.ini` is what the fleet actually runs.
- **Opened lazily**, on the first thing worth showing: the first byte of a
  download the server accepted (`source.WithProgress` carries the callback in
  the context, so neither `download.Manager` nor the `Source` interface knows
  about it), or the start of a setup. A cached setup, a `429`-queued download
  and an update waiting for EMLy to close never flash an empty window.
- **Closed before any wait on the user**: `apply` closes it as soon as EMLy is
  found running (countdown, or waiting for exit - possibly hours), and
  `install` reopens it. It is also closed before the update-complete toast.
- **Install phase is a marquee**: Inno Setup `/VERYSILENT` reports nothing
  until it exits, so there is no percentage to show.
- **Self-update**: the setup stops this service, so the service's end of the
  pipe closes mid-install. The service *detaches* instead of closing, and the
  helper (`--wait-service EMLyUpdater --wait-pid <old pid>`) stays up until the
  SCM reports the service running under a new process, 3 minutes at most. EOF
  without a `close` message is what tells the two cases apart.
- **It never blocks an update**: nil-safe everywhere, writes go through a
  goroutine that keeps only the latest message, and a window that cannot be
  opened (nobody logged on, token errors) is a log line.
- **Console or RDP**: see "UI goes to the viewer session" in Common Pitfalls.
- **Z-order**: a process started by a service cannot take the foreground, and
  its window would land behind the user's. It is shown with
  `SW_SHOWNOACTIVATE` and passed through `HWND_TOPMOST` → `HWND_NOTOPMOST`:
  on top, without stealing the keyboard focus.
- **Needs the manifest** (`app.manifest`, embedded via `versioninfo.json`
  `ManifestPath`): Common Controls 6 for the modern bar and `PBS_MARQUEE`,
  `dpiAware` for crisp scaling. Without it the bar is the classic one and the
  install phase shows a full bar.

### Progress window manual verification

1. `$env:EMLY_PROGRESS_WINDOW_TEST=1; go test ./internal/progresswin/ -run Live -v`
   shows the window for ~10s (download to 100%, then install, then close).
   A test binary has no manifest: expect the classic bar.
2. For the real look, feed the built exe by hand:
   `'{"heading":"Download di EMLy 1.8.0 in corso","detail":"...","percent":40}' | build\EMLyUpdater.exe show-progress --title "EMLy - Aggiornamento" --icon C:\3gIT\EMLy\EMLy.exe`
   (the window closes when stdin ends).
3. End to end: the local E2E recipe in README.md with a new version in
   `version.json`, logged on at the console - then again over RDP.

### Certificate manual verification (admin required)

Nothing in `internal/cert/store.go` or `internal/notify/console_user.go` is
covered by `go test` — they are pure Windows API calls and CI has no admin
rights. This checklist is their verification.

0. Quickest check of the crypt32 path, no install needed — from an **elevated**
   shell: `$env:EMLY_CERT_STORE_TEST=1; go test ./internal/cert/ -run Live -v`.
   It exercises Ensure against the real stores with a throwaway certificate and
   cleans up after itself. Skipped by default so CI never runs it.
1. On a clean machine, run `emly-updater install` and confirm in `certmgr.msc`
   (Local Computer) that `CN=3G IT Innovation` is in both **Trusted Root
   Certification Authorities** and **Trusted Publishers**.
2. Log on as a standard user, wait one poll cycle, and confirm in `certmgr.msc`
   (Current User) that it is in the same two stores for that user.
3. Run the EMLy setup and confirm the UAC prompt reads *"Verified publisher:
   3G IT Innovation"*.
4. Delete the certificate from `LocalMachine\Root`, wait one cycle, and confirm it
   is restored and event 702 is logged.
5. With nobody logged on at the console, confirm the machine stores are still
   maintained and only a Debug line notes the skipped per-user half.
6. Confirm a second cycle after a successful install logs nothing at Info — i.e.
   the `CRYPT_E_EXISTS` path really is Debug-level and 96 cycles a day do not
   flood the log.
7. Set `enabled = false` under `[certificate]`, restart the service, and confirm
   nothing is written and nothing is logged.

## Deployment

### Installer (recommended)

1. Generate version resources: `go generate`
2. Build: `go build -ldflags "-s -w" -o build\bin\emly-updater.exe .`
3. Compile `installer\installer.iss` with Inno Setup 6 → `installer\Output\EMLyUpdater_Installer_<ver>.exe`
4. Deploy the setup via GPO / Intune / SCCM (requires admin; runs silently).

The setup:
- Installs the binary to `%ProgramFiles%\EMLyUpdater\`
- Calls `emly-updater.exe install` (seeds config, registers service + Event Log source)
- Calls `emly-updater.exe start`
- Writes the HKLM `Run` value that starts the tray at every logon (`conhost.exe
  --headless ... tray`), and on an interactive install starts it right away
- On upgrade: stops the service first (60 s wait), then replaces the binary
- Optional component `wingetmodule` (**off by default**): downloads
  `Microsoft.WinGet.Client` from PowerShell Gallery at install time and puts it in
  `%ProgramFiles%\WindowsPowerShell\Modules` (= `Install-Module -Scope AllUsers`),
  and installs PowerShell 7 from its GitHub MSI unless a `pwsh.exe` is already in
  `%ProgramFiles%\PowerShell\7`. There is no separate PowerShell component on purpose:
  the service runs as SYSTEM, where the module refuses Windows PowerShell 5.1, so the
  module is useless to it without PowerShell 7 (`internal/winget` prefers `pwsh.exe`).
  Each half is skipped (not downloaded, not installed) when already present:
  `pwsh.exe` in `%ProgramFiles%\PowerShell\7` (any version), and the module at the
  pinned version **or newer** under `%ProgramFiles%\WindowsPowerShell\Modules` or
  `%ProgramFiles%\PowerShell\Modules` (the AllUsers paths SYSTEM can load; a
  CurrentUser install does not count). With both present, an interactive run greys
  out the component ("already installed", `CurPageChanged(wpSelectComponents)`) without
  changing its checked state; a silent run with `/COMPONENTS=...,wingetmodule` just
  skips both downloads.
  Both downloads run on a download page with a progress bar right after "Ready to
  Install" (`NextButtonClick(wpReady)`, also reached in a silent install), before the
  service is stopped; only the extract/msiexec steps run in `ssPostInstall`.
  Opt in with `/COMPONENTS="updater,wingetmodule"` (`/COMPONENTS` replaces the
  selection, so list both); without `/COMPONENTS` an upgrade keeps the previous
  choice. Version and SHA256 are pinned by `#define`s in `installer.iss` - change
  them together. The download runs from `[Code]`, not a `[Files]` `download` entry,
  on purpose: a failure (Gallery unreachable, hash mismatch) is only logged and the
  updater still installs, where a failed `[Files]` download would abort a silent
  setup - self-update included. Uninstall leaves the module in place.

### Manual (admin shell)

```powershell
Copy-Item .\build\bin\emly-updater.exe "C:\Program Files\EMLyUpdater\"
& "C:\Program Files\EMLyUpdater\emly-updater.exe" install
& "C:\Program Files\EMLyUpdater\emly-updater.exe" start
```

### Post-deployment config tweaks

Edit `%ProgramData%\EMLyUpdater\config.ini` as an administrator - by hand, or from
the tray's settings window, which validates the edit and restarts the service for
you. The service reads the file only at start, so a hand edit needs a restart.
Edits do **not** survive an upgrade: every install, self-update included, rewrites
the file from the new build's defaults (`config.Reset`, previous copy in
`config.prev.ini`). Anything that must hold fleet-wide belongs in the remote
configuration document instead.

## Logs & Diagnostics

| File | Content |
|------|---------|
| `%ProgramData%\EMLyUpdater\logs\updater.log` | Rolling 5 MB × 5 - all events |
| `<ExeDir>\updater.log` | Same events, kept next to exe for on-site access |
| `%ProgramData%\EMLyUpdater\logs\emly-install-<ver>.log` | InnoSetup silent install log |
| `%ProgramData%\EMLyUpdater\logs\updater-selfinstall-<ver>.log` | InnoSetup log of the updater installing itself |
| `%ProgramData%\EMLyUpdater\logs\updater-final.log` | Exe-dir log preserved on uninstall |
| `%ProgramData%\EMLyUpdater\config.prev.ini` | The config as it was before the last reset |
| Windows Event Log → `EMLyUpdater` source | Update found (100), install ok (200)/failed (201), forced kill (300), assoc repair (400), IPC client rejected (600), IPC unavailable (601), source policy decision (700)/failure (701), cert installed (702), cert install failed (703), self-update started (800)/completed (801)/refused or abandoned (802), presence channel connected (920)/lost (921)/endpoint not implemented (922)/switched off by the document (923), client channel command accepted (924, Event Log only for `service.restart`/`machine.reboot`)/refused (925)/pending restart-or-reboot auto-expired (926) |

Event 801 is written by the build that came up *after* the restart, so a self-update reads
`800` → (service stops and restarts) → `801`. An `800` with no `801` after it is one that did not
land; the `selfUpdate` record left in `state.json` says which version was attempted and how often.

## Branching

- **Feature grande che tocca un nuovo evento IPC e/o richiede bump di
  versione dell'Updater**: lavora su un branch dedicato, non su `master`.
  Motivo: `proto/updateripc.proto` è sincronizzato a mano col repo `emly` e
  un bump di `ProtocolVersion`/`MaxCompatibleEMLyVersion` tocca la matrice di
  compatibilità wire — cambi che vuoi poter revisionare/rollback come unità
  prima che finiscano su `master`.

## Common Pitfalls

- **Never patch `products` from an override while agents <= 1.7.x are in the field**: a global `products` section is harmless (`policy.complete` copies only the sections it knows), but an override whose patch touches `products` makes those agents reject the **whole** document ("not a patchable section"), and they then stay on their cached revision for everything - servers, kill switch, all of it. Same trap as `clientWs` in September. Wait until the fleet is all >= 1.8.0.
- **Removing a product from the document drops it from the inventory, so from the dashboard**: `installedProducts` walks the document's products, so a product that is no longer there is no longer reported. To stop updating a product use `enabled: false`, which keeps detection and the inventory intact.
- **NSIS command lines are raw**: `/D=<dir>` must be the last argument and unquoted (even with spaces), and the uninstaller needs `_?=<dir>` last and unquoted too. Go's argument quoting would add quotes, so the nsis driver sets `SysProcAttr.CmdLine` itself. Without `_?=` the NSIS uninstaller copies itself to `%TEMP%`, relaunches and returns at once, and the agent would start the reinstall while the uninstall is still running.
- **An uninstaller in a directory writable by Users is never run as SYSTEM** (`installer.CheckNotUserWritable`): it is stricter than "no write for Users". The owner of `InstallDir` and of the uninstaller must be SYSTEM, Administrators or TrustedInstaller, and any non-inherit-only allow ACE granting a write right (including `FILE_DELETE_CHILD`) to any other SID, an unknown allow-ACE type, or a NULL DACL refuses. A refusal only skips the clean-reinstall uninstall (Warn), the reinstall itself proceeds. Consequence: an install directory created by a user, or one inheriting "Authenticated Users: Modify" from `C:\` (e.g. `C:\3gIT\EMLy` unless its installer sets permissions), makes the uninstall step always skipped. That is by design, not a bug to relax.
- **3g-RocketChat's `config.ini` `[app] version` is not rewritten by its setup today**: its detection chain therefore reads `version.txt` first, then the ini, then the exe's VERSIONINFO. Reordering it puts a stale version first and the agent will reinstall forever (until the 3-attempt cap stops it).
- **Building a `download.Manager` without the shared `Pacer`**: a nil `Pacer` turns off 429 pacing entirely (the 429 comes straight back as an error, retried only next cycle). `service.New` gives `Downloads` and `SelfDownloads` the *same* `Pacer` because the server's slots are one pool; giving each its own lets the EMLy download hit the server right after the self-update download was told to wait.
- **Adding a new config key**: update `Config` struct, `Load()`, and `config.default.ini` (all three, otherwise the key is invisible to callers and missing from freshly seeded configs). Upgrades pick it up for free — `config.Reset` rewrites the file from the embedded defaults, so a new key arrives with its default and its comment (and any per-machine edit is discarded).
- **Rotating the code-signing certificate now also gates self-update**: `internal/authenticode` pins the signer to whatever `cert.Embedded()` holds, so a release signed with the *new* certificate cannot be self-installed by machines still running a build that embeds only the old one. Ship the new certificate in a release signed with the old one first, let the fleet take it, and only then start signing with the new one.
- **Editing `proto/updateripc.proto`**: copy the change verbatim to `emly/proto/updateripc.proto` and regenerate both repos' `ipcpb` packages. The two repos share no Go module, so nothing enforces this automatically — a one-sided edit silently desyncs the wire protocol.
- **Cutting an EMLyUpdater release**: bump `versioninfo.json`'s `StringFileInfo.FileVersion`/`ProductVersion` (the single source of truth for the version string — see `tools/genversion`) and run `go generate ./...`. That regenerates `internal/version/version_generated.go` and rewrites the version token in `installer/installer.iss` (`ApplicationVersion`) — no other file should ever hardcode the version string by hand again. (`config.default.ini` is deliberately *not* patched any more: its `userAgent` carries a `{{VERSION}}` placeholder resolved at runtime.) Then publish the release to the updater manifest — the signed installer plus its SHA256 on `/v2/updates/manifest/updater`, on the server (one machine; every address in the policy reaches it, see *Deployment topology*) — or no machine will pick it up by itself. Then update the **EMLyUpdater max** column of the compatibility matrix atop `proto/updateripc.proto` to the version being shipped, even if the release doesn't touch `internal/ipc` at all — otherwise the matrix silently goes stale. (That file is manually synced with the `emly` repo, so copy the edit there too.) Do **not** touch `MaxCompatibleEMLyVersion` here: despite living in this repo it tracks *EMLy's* releases, not this one's, and bumping it for an EMLyUpdater release would claim compatibility with an EMLy build that may not exist. Bump it — and the matrix's EMLy max column — when *EMLy* cuts a release. Bump `MinCompatibleEMLyVersion` only when this release genuinely requires a newer EMLy build. Mirror `MaxCompatibleUpdaterVersion` on the `emly` side the same way when *that* repo cuts a release.
- **Rotating the code-signing certificate**: replace **both**
  `certs/3GITInnovation.cer` (the source of record) and
  `internal/cert/3GITInnovation.cer` (the embedded copy — `//go:embed` cannot
  reach above its own package), update `wantThumbprint` in
  `internal/cert/cert_test.go` and section 4 of the design doc, then cut a
  release. The old certificate stays installed on existing machines and keeps
  validating signatures made with it. `internal/cert/cert_test.go` fails 60 days
  before expiry, so this should never be a surprise. Two standing
  recommendations for whoever issues the next one: give it a 10-year validity and
  a SHA-256 signature (it is self-signed — the lifetime is a free choice, and an
  annual one creates yearly release pressure for nothing), and timestamp the
  signatures themselves (`signtool /tr <rfc3161-url> /td sha256`) so they survive
  the certificate's expiry.
- **`versioninfo.json` must keep `IconPath` and `ManifestPath`** (PascalCase, as
  goversioninfo reads them): commit `f62bbf7` replaced both with an `icon_path` key
  goversioninfo ignores, and every build from 1.7.2 to 1.8.0 shipped with no icon
  and no manifest. That means no Common Controls 6 (classic progress bar, no
  `PBS_MARQUEE`, classic tray controls), no `dpiAware`, and the generic icon on the
  exe itself (Explorer, the tray, the window title bars). Quick check after `go generate`: `resource.syso`
  is ~575 KB with them, ~1 KB without.
- **HTTP headers**: set them in `HTTPSource` only - the `Resolver` itself is header-agnostic.
- **`logging.New` signature**: `(logDir, exeLogPath, console)` - passing an empty string for `exeLogPath` disables the exe-side sink.
- **InnoSetup version lock**: `installer.iss` uses `{autopf}` and `ArchitecturesInstallIn64BitMode` which require IS 6. IS 5 will refuse to compile it.
- **UI goes to the viewer session, not the console session**: every notifier in
  `internal/notify` (toast, progress window, critical warning, pending box) picks
  its session with `viewerSession()` (`notify/session.go`), which reuses
  `machineinfo.InteractiveSession`: the active console session when someone is
  logged on there, otherwise an active RDP session; a disconnected session is
  skipped (nobody would see it). Never go back to `WTSGetActiveConsoleSessionId`
  for UI: it only names the physical console, so a user on RDP - whose console
  sits at the login screen - saw none of it. `ConsoleUserSID` (per-user
  certificate stores) still uses the console session.
- **Update-complete toast**: shown via `internal/toast.Show`, which must run inside the user's desktop session (session 0, where the SYSTEM service lives, has none). `internal/notify.LaunchToast` does the SYSTEM -> user-session hop with `WTSQueryUserToken` + `CreateProcessAsUser`, re-launching the updater's own exe with the hidden `show-toast` subcommand. The icon shown is extracted at runtime from the installed `EMLy.exe` (`ExtractIconEx`) - there is nothing to keep in sync when EMLy's icon changes. Toast failures (no active user session, token/privilege errors, missing icon) are always best-effort/logged, never fail the (already-successful) update.
