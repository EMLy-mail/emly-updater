# EMLyUpdater

Standalone Windows update service for **EMLy**. Runs as a `LocalSystem`
auto-start service on domain-joined PCs and keeps EMLy current without any
user interaction: it polls an update manifest, downloads and SHA256-verifies
the InnoSetup installer, and applies it silently.

The service is fully independent of EMLy: binary in `C:\Program Files\EMLyUpdater`,
everything else (config, state, logs, download cache) under
`C:\ProgramData\EMLyUpdater`, which survives EMLy uninstall/reinstall and is
never touched by EMLy's own installer.

## Requirements

| Requirement | Detail |
|---|---|
| **OS** | Windows 10 / Windows Server 2016 or later (64-bit) |
| **Privileges** | `install` / `uninstall` / `start` / `stop` require administrator rights; the service itself runs as `LocalSystem` |
| **EMLy** | `C:\3gIT\EMLy\config.ini` must exist and contain `GUI_SEMVER` for version detection (configurable via `emlyConfigFile`) |
| **Network** | HTTPS access to the external manifest URL, **or** LAN access to an internal HTTP manifest |
| **Build-time** | Go 1.22+ and [Inno Setup 6](https://jrsoftware.org/isdl.php) (only to compile the installer; not needed for the service itself) |

## How an update is applied

| EMLy state | Behavior |
|---|---|
| Not running | Install immediately |
| Running, normal update | Queue to `state.json`, wait for EMLy to exit (kernel wait, no polling), then install |
| Running, **forced** update (`isCritical` or installed < `minRequiredVersion`) | Optional WTS warning box in the user's session (countdown, localized it/en), then `TerminateProcess` and install |
| EMLy not installed (no `config.ini`) | Fresh-install mode: treat installed version as `0.0.0` and install the channel target (channel = `channelOverride` or `stable`) |

After every successful install the service re-reads `GUI_SEMVER` from EMLy's
`config.ini` to confirm the version, self-heals the machine-wide `.eml`/`.msg`
HKLM file associations (+ `SHChangeNotify`), and shows a Windows notification
in the active user's session announcing the new version and channel (e.g.
"EMLy has been updated to version 1.4.2 (beta)."), carrying EMLy's own icon.
This is a courtesy notification only: it is skipped silently when nobody is
logged in at the console, and never affects update success/failure.

A queued update survives reboots: the pending entry lives in
`C:\ProgramData\EMLyUpdater\state.json` (written atomically) and its setup is
checksum-re-verified before any resumed install. A setup whose SHA256 does not
match the manifest is **never** executed.

## Remote configuration

Everything operational - which servers exist, which subnets belong to which
site, the poll interval, the log level, whether the updater may install at
all - comes from one JSON document the service fetches from `GET /v2/config`
on the same hosts that serve the manifest. `config.ini` is reduced to a
bootstrap: how to reach *a* server, where EMLy lives, the pipe name, the API
key. It is read once at start and **never written at runtime**.

The document is validated **all or nothing** against the same rules the API
enforces before publishing it (schema version, server references, CIDRs,
version bounds, override selectors and patches). Anything wrong and the whole
document is refused with the offending field paths in the log (event 902),
and the machine keeps what it had.

Three layers, in order of precedence:

| Layer | Where | When it applies |
|---|---|---|
| Remote document | `GET /v2/config` → `remote-config.json` | whenever a valid one has been received |
| Last-known-good | `C:\ProgramData\EMLyUpdater\remote-config.json` | the endpoint is unreachable, or has nothing newer |
| Default policy | derived from `config.ini`'s legacy `[source]` keys | no document has ever been accepted on this machine |

The default policy reproduces the pre-existing behaviour exactly, chain
included (`internal` → `bkInternManifestURL` → `external`), so a machine that
never reaches the endpoint behaves as it did before this existed. Set
`enabled = false` under `[remoteConfig]` to pin a machine to that
permanently.

Failure is always towards *keep working*:

- **Unreachable** (every candidate server failed): the cached document stays
  in force, event 901 once per outage. Never a pause.
- **Nothing published yet** (`204`) or **unchanged** (`304`): not an error, no
  outage logged.
- **A mirror serving an older copy**: ignored. A document is only accepted
  when its `revision` is greater than or equal to the cached one, so a lagging
  server cannot roll a machine back.
- **Cache older than `refresh.staleAfterDays`**: event 903 once a day, and the
  policy stays in force. Age is measured from the local `fetchedAt`, never
  from the document's `generatedAt` - PC clocks drift.
- **Disk full / unwritable cache**: the new policy is kept in memory for the
  run and the write is retried at the next fetch.
- **Corrupt or no-longer-valid cache**: moved aside to `remote-config.bad.json`
  and the default policy carries the run.

### Kill switch

`control.updater.enabled = false` pauses the update machinery: no manifest
fetch, no download, no install, no self-update. The configuration fetch, the
IPC server and the trust-store/file-association self-heal keep running - which
is what lets a paused fleet be un-paused remotely. Each flip is event 904 with
the reason. A `control.updater.until` in the past reads as enabled, so a
forgotten freeze expires on its own.

`control.app` is not enforced here: it is handed to EMLy over IPC.

### Per-host exceptions

`overrides` are evaluated on the machine, in list order, as JSON Merge
Patches over `control`, `updater`, `logging` and `defaultServer`. A selector's
keys are ANDed and the values inside one list are ORed; `match: {"all": true}`
selects the whole fleet and `except` carves a group back out of it. Because
they are evaluated locally, a laptop that moves between sites changes side at
its next cycle without a new document.

## Update sources

The manifest is fetched from the server chain the policy assigns to this
machine: the site's `baseServer` first, retried with exponential backoff,
then each `backupServer` once, in order. A machine at no site uses
`defaultServer` alone. The setup binary is always fetched from the same
source that served the manifest. HTTP manifests key checksums by version and
carry full download URLs.

Which site a machine is at is decided **every cycle**, not once at start: the
service resolves the nearest domain controller and matches it against
`dcLookupMap`, requiring one of the machine’s own IPs to be inside one of that
site’s subnets. The local addresses are re-read every cycle (no network
involved) and the domain controller is re-queried whenever they change or the
cached answer is an hour old, so a laptop that moves follows its network
instead of waiting for a service restart.

At boot that lookup can run before the network, DNS and netlogon are ready,
and Windows then answers *"the specified domain either does not exist or could
not be contacted"* - indistinguishable from a machine that is genuinely
off-site, which would pin it to the public API for the whole run. Two things
guard against it: the service is registered as **delayed** auto-start with a
dependency on `Dnscache` and `LanmanWorkstation`, and the **first** lookup is
retried (`updater.dcLookupRetry`, 6 × 5s by default) before the decision is
taken. Later cycles get a single attempt - a machine that has moved must not
stall a cycle on a doomed retry. A machine that is not domain-joined skips the
retry entirely: there the failure is the answer.

When a poll cycle still cannot reach any source (primary exhausted, fallback
also failed or not configured), a Windows notification tells the active
console user to contact their IT department ("EMLy Updater non riesce a
contattare il server degli aggiornamenti. Contattare il proprio IT." / the
English equivalent), carrying EMLy's own icon like every other toast this
service shows. It fires at most once per outage - shown once, then suppressed
until a cycle succeeds again - so a prolonged outage doesn't nag on every
poll; if nobody was logged in to see it, the next cycle tries again.

## Self-update

The service also keeps **itself** current. Every cycle, before anything else,
it asks the same server that serves EMLy's manifest for its own
(`.../v2/updates/manifest/updater`, derived by appending `updater` to the
manifest URL). When a newer release is published it downloads the signed
installer, verifies it, and hands it off — the setup then stops the service,
replaces the binary, and starts it again. The service dies mid-cycle by
design; a queued EMLy update is persisted and resumes under the new binary.

A setup is executed only when **both** checks pass:

- its SHA256 matches the manifest, and
- it carries a valid Authenticode signature made by the embedded 3gIT
  certificate (pinned by thumbprint, not merely "some trusted publisher").

The signature is what makes this safe over the internal LAN source, which is
plain HTTP: whoever could serve a tampered setup there could serve a matching
checksum with it, but not a signature.

**Your `config.ini` is reset.** The `install` step rewrites it from the new
release's embedded defaults; the previous file is kept as `config.prev.ini`,
so a setting you still need can be recovered and re-applied from there.

A release that installs but does not result in the new binary running is
retried at most 3 times and then abandoned (event 802) until a *different*
version is published — so a bad build cannot put the fleet in a restart loop.
Publishing `{"version": ""}` stops the rollout fleet-wide immediately.

Set `updater.selfUpdate.enabled` to `false` in the remote configuration - or
`enabled = false` under `[selfUpdate]` on a machine with no document - to opt
out entirely. A paused updater (`control.updater.enabled = false`) does not
self-update either.

## Installation

### Via Installer (recommended)

1. Download `EMLyUpdater_Installer_<version>.exe` from the release or build it (see [Build](#build)).
2. Run as administrator (or deploy silently via GPO/Intune/SCCM):

   ```
   EMLyUpdater_Installer_<version>.exe /VERYSILENT /SUPPRESSMSGBOXES /NORESTART
   ```

The installer:
- Places the binary in `C:\Program Files\EMLyUpdater\`
- Calls `install` (seeds `config.ini`, registers the Windows service and Event Log source).
  The service is registered as **delayed auto-start**, depending on `Dnscache` and
  `LanmanWorkstation`, so it does not run its domain controller lookup before the
  network is usable
- Starts the service immediately
- On upgrade: gracefully stops the running service first, replaces the binary, restarts

**Uninstall** via Add/Remove Programs or:
```
EMLyUpdater_Installer_<version>.exe /VERYSILENT /UNINSTALL
```
`C:\ProgramData\EMLyUpdater` (config, state, logs) is **kept** intentionally.

### Manual (admin PowerShell)

```powershell
Copy-Item .\build\bin\emly-updater.exe "C:\Program Files\EMLyUpdater\" -Force
& "C:\Program Files\EMLyUpdater\emly-updater.exe" install
& "C:\Program Files\EMLyUpdater\emly-updater.exe" start
```

## Configuration

`C:\ProgramData\EMLyUpdater\config.ini` is created from embedded defaults on
the first `install` or service start (see [`internal/config/config.default.ini`](internal/config/config.default.ini)).
Changes take effect on the next poll cycle.

**Most of it is now a fallback.** With `[remoteConfig] enabled = true` (the
default), the keys marked *(legacy)* below only apply until this machine
first receives a valid document from `GET /v2/config`; from then on the
document decides. The keys that are *not* marked stay in force always - they
are what makes the document reachable in the first place. See
[Remote configuration](#remote-configuration).

Edits do **not** survive upgrades: on every `install` (including a
self-update) the file is rewritten from the new release's embedded defaults,
and the previous file is kept as `config.prev.ini`. A setting that must
persist across upgrades has to be re-applied after the upgrade (or pushed by
GPO/script). The default `userAgent` carries a `{{VERSION}}` placeholder
resolved at runtime, so a machine always reports the version it is actually
running. The file does survive uninstall (`%ProgramData%` is kept).

### `[updater]`

| Key | Default | Description |
|---|---|---|
| `emlyInstallDir` | `C:\3gIT\EMLy` | Directory containing EMLy's executable |
| `emlyExeName` | `EMLy.exe` | EMLy executable filename |
| `emlyConfigFile` | `C:\3gIT\EMLy\config.ini` | EMLy config read for version, channel, language |
| `pollIntervalMinutes` | `15` | *(legacy)* How often to check for updates → `updater.pollIntervalMinutes` |
| `channelOverride` | _(empty)_ | *(legacy)* Leave empty to follow each machine's `GUI_RELEASE_CHANNEL`; set `stable` or `beta` to force fleet-wide → `updater.channelOverride` |

### `[remoteConfig]`

| Key | Default | Description |
|---|---|---|
| `enabled` | `true` | Fetch the policy document from `GET /v2/config`. `false` pins the machine to the keys in this file |
| `endpoints` | (API URL) | Base URLs to ask, `\|`-separated, tried **after** the servers the cached policy assigns to this machine |
| `configPath` | `/v2/config` | Route appended to every base URL |
| `timeoutSeconds` | `10` | Per-attempt timeout (1-120). The fetch is best-effort: a failure never fails a cycle |

### `[source]`

| Key | Default | Description |
|---|---|---|
| `primary` | `internal` | *(legacy)* `external` (public HTTPS) or `internal` (LAN HTTP) → `dcLookupMap` / `defaultServer` |
| `externalManifestURL` | (API URL) | *(legacy)* Required when `primary = external` → `servers` |
| `internalManifestURL` | _(empty)_ | *(legacy)* Required when `primary = internal` → `servers` |
| `bkInternManifestURL` | _(empty)_ | *(legacy)* Backup internal manifest URL → a site's `backupServer` |
| `userAgent` | `EMLy-Updater/{{VERSION}} (...)` | `User-Agent` header sent on every HTTP request, the config fetch included; `{{VERSION}}` is replaced at runtime with the running version |
| `xApiKey` | _(empty)_ | `X-Api-Key` header sent on every HTTP request, the config fetch included |
| `defaultMappingDCSubnets` | `DC-RM2:...` | *(legacy)* Per-site map of domain controller name to its internal CIDR subnets: `dc:cidr[,cidr...][\|dc:cidr[,cidr...]...]` (legacy `;` delimiter still accepted); empty disables the site check → `dcLookupMap` |
| `dcLookupRetryAttempts` | `6` | *(legacy)* Extra domain controller lookups attempted when the first one fails at startup; `0` disables retrying. Skipped on a machine that is not domain-joined → `updater.dcLookupRetry.attempts` |
| `dcLookupRetryDelaySeconds` | `5` | *(legacy)* Wait between those attempts (`0` also disables retrying) → `updater.dcLookupRetry.delaySeconds` |

### `[criticalUpdate]`

| Key | Default | Description |
|---|---|---|
| `criticalWarningEnabled` | `true` | *(legacy)* Show a countdown dialog in the user's session before a forced close → `updater.criticalWarning.enabled` |
| `criticalWarningSeconds` | `30` | *(legacy)* Countdown duration; warning language follows EMLy's `LANGUAGE` key (fallback `en`) → `updater.criticalWarning.seconds` |

### `[selfUpdate]`

| Key | Default | Description |
|---|---|---|
| `enabled` | `true` | *(legacy)* Keep the updater itself up to date (see [Self-update](#self-update)) → `updater.selfUpdate.enabled` |
| `manifestURL` | _(empty)_ | Leave empty to derive it from the manifest URL in use (`.../manifest` → `.../manifest/updater`), so each site self-updates from its own mirror. Set it only to point at a different host; it then applies to every source, with no fallback |

### `[certificate]`

| Key | Default | Description |
|---|---|---|
| `enabled` | `true` | *(legacy)* Install the 3gIT code-signing certificate into `Root` + `TrustedPublisher`, for the machine and for the console user. Makes EMLy's setup elevate as a verified publisher instead of "Unknown publisher". Re-checked every cycle; idempotent → `updater.installCertificate.enabled` |

### `[fileAssociations]`

| Key | Default | Description |
|---|---|---|
| `progIdEml` | `EMLy.EML` | ProgID for `.eml` files; self-healed after every install |
| `progIdMsg` | `EMLy.MSG` | ProgID for `.msg` files |

## Deployment

### GPO / Intune / SCCM

Deploy the installer silently to domain-joined machines. The service registers
itself as `EMLyUpdater`, auto-start, `LocalSystem`. No user session is required
for the service to function. Post-deployment, push a customised `config.ini` to
`C:\ProgramData\EMLyUpdater\` via a File-based GPO preference or Intune
deployment script (the file is never overwritten by upgrades).

## Build

```
go build -ldflags "-s -w" -o build\bin\emly-updater.exe .
go test ./...
```

Installer (requires Inno Setup 6): compile `installer\installer.iss` after the
build; the setup installs the binary, registers + starts the service, and on
uninstall removes the service but keeps ProgramData.

## CLI

```
EMLyUpdater.exe install     # register delayed auto-start service + Event Log source (admin)
EMLyUpdater.exe uninstall   # stop + remove the service, keep ProgramData
EMLyUpdater.exe start|stop  # control the service
EMLyUpdater.exe run         # foreground debug mode (console logging)
```

## Logs

- Rolling file: `C:\ProgramData\EMLyUpdater\logs\updater.log` (2 MB × 5, gzip-compressed once rotated; level and limits are `logging.*` in the remote configuration and change without a restart)
- InnoSetup install logs: `C:\ProgramData\EMLyUpdater\logs\emly-install-<version>.log`
- Self-update install logs: `C:\ProgramData\EMLyUpdater\logs\updater-selfinstall-<version>.log`
- Windows Event Log (source `EMLyUpdater`): update found (100), sources
  unreachable (101), install ok (200) / failed (201), forced kill (300),
  associations repaired (400), source decided (700) / undecidable (701),
  certificate installed (702) / failed (703), self-update started (800) /
  completed (801) / refused or abandoned (802), remote configuration applied
  (900) / unreachable (901) / rejected (902) / stale (903), kill switch
  flipped (904)

Events 900-904 are written to the Event Log even when the remote
configuration turns the mirror off (`logging.eventLog = false`), so a bad
push is always diagnosable from Event Viewer alone.

A self-update reads as `800` → *(service stops and restarts)* → `801`, the two
records written by different builds. An `800` never followed by an `801` is a
self-update that did not land.

To confirm the service is actually looking, without stopping it: every cycle
that does not self-update logs one line saying why, naming the endpoint it
reached.

```
msg="no updater self-update this cycle" installed=1.5.0 manifestURL=http://172.16.33.72:8080/v2/updates/manifest/updater reason="already running 1.5.0, offered 1.5.0"
msg="no updater self-update this cycle" installed=1.4.2 manifestURL=https://api.emly.ffois.it/v2/updates/manifest/updater reason="no updater release is published"
```

```powershell
Get-Content C:\ProgramData\EMLyUpdater\logs\updater.log -Tail 50 |
  Select-String "no updater self-update|updater update available|updater manifest served"
```

## Testing end-to-end without infrastructure

Serve a `version.json` and a setup binary from a local HTTP server (e.g.
`python -m http.server` in a folder containing both), with `version.json`'s
`stableDownload`/`betaDownload` set to absolute URLs pointing back at that
server and `sha256Checksums` keyed by version. Point `externalManifestURL` (or
`internalManifestURL`) at it, then `EMLyUpdater.exe run`. Tamper with the
checksum in `version.json` to watch the refusal path; set `"isCritical": true`
while EMLy is open to exercise the warn-and-kill path.

For the remote configuration, serve a document at `/v2/config` from the same
local server and point `[remoteConfig] endpoints` at it. Flip
`logging.level`, `control.updater.enabled` and `updater.pollIntervalMinutes`
and watch them take effect without restarting the service; break a field on
purpose to watch the document being refused whole (event 902) while the
previous one stays in force. The conformance fixtures under
`testdata/remoteconfig/` are ready-made documents for this.

Point `ProgramData` at a scratch directory for the run (`config.DataDir()`
reads it) so the machine's real config, state and logs are untouched, and set
`[ipc] enabled = false` so the run does not collide with an installed service
over the named pipe.

For the **self-update** path, serve a second file next to it holding the
updater manifest and set `selfUpdate.manifestURL` to it:

```json
{ "version": "9.9.9",
  "download": "http://127.0.0.1:8000/EMLyUpdater_Installer_9.9.9.exe",
  "sha256": "<sha256 of that file>" }
```

With an unsigned stand-in for the installer the run should log event 800, fetch
and checksum it, then refuse it with event 802 — the signature check doing its
job. Exercising the launch itself needs the real, CI-signed installer.
