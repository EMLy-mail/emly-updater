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

## Update sources

The manifest is fetched from the configured primary source (`external` HTTPS
or `internal` LAN HTTP), retried up to 3 times with exponential backoff. When
`primary = internal`, a reachable `externalManifestURL` is wired in as a
one-shot fallback: if the internal endpoint is still unreachable after every
retry, the cycle falls back to the public API rather than failing outright.
The fallback is only for that fetch - it is never written to `config.ini`, so
the next cycle tries `internal` again first. The setup binary is always
fetched from the same source that served the manifest. HTTP manifests key
checksums by version and carry full download URLs.

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
it asks the same source that serves EMLy's manifest for its own
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

**Your `config.ini` survives.** The `install` step reconciles it with the new
release's defaults rather than overwriting it: every value already on the
machine wins, keys the release added appear with their documented defaults,
keys it removed disappear, and the previous file is kept as `config.prev.ini`.

A release that installs but does not result in the new binary running is
retried at most 3 times and then abandoned (event 802) until a *different*
version is published — so a bad build cannot put the fleet in a restart loop.
Publishing `{"version": ""}` stops the rollout fleet-wide immediately.

Set `enabled = false` under `[selfUpdate]` to opt a machine out entirely.

## Installation

### Via Installer (recommended)

1. Download `EMLyUpdater_Installer_<version>.exe` from the release or build it (see [Build](#build)).
2. Run as administrator (or deploy silently via GPO/Intune/SCCM):

   ```
   EMLyUpdater_Installer_<version>.exe /VERYSILENT /SUPPRESSMSGBOXES /NORESTART
   ```

The installer:
- Places the binary in `C:\Program Files\EMLyUpdater\`
- Calls `install` (seeds `config.ini`, registers the Windows service and Event Log source)
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

Edits survive upgrades and uninstall: on every `install` the file is merged
with the new release's defaults — the values already on the machine win, keys
the release added arrive with their defaults and their documentation, and the
pre-merge file is kept as `config.prev.ini`. The one value not carried over is
`userAgent` when it still looks like a shipped default (`EMLy-Updater/<x.y.z>
...`): it is replaced with the current default, whose `{{VERSION}}` placeholder
resolves at runtime, so a machine always reports the version it is actually
running. A `userAgent` customised by hand is preserved like everything else.

### `[updater]`

| Key | Default | Description |
|---|---|---|
| `emlyInstallDir` | `C:\3gIT\EMLy` | Directory containing EMLy's executable |
| `emlyExeName` | `EMLy.exe` | EMLy executable filename |
| `emlyConfigFile` | `C:\3gIT\EMLy\config.ini` | EMLy config read for version, channel, language |
| `pollIntervalMinutes` | `30` | How often to check for updates |
| `channelOverride` | _(empty)_ | Leave empty to follow each machine's `GUI_RELEASE_CHANNEL`; set `stable` or `beta` to force fleet-wide |

### `[source]`

| Key | Default | Description |
|---|---|---|
| `primary` | `external` | `external` (public HTTPS) or `internal` (LAN HTTP) |
| `externalManifestURL` | (API URL) | Required when `primary = external` |
| `internalManifestURL` | _(empty)_ | Required when `primary = internal` |
| `userAgent` | `EMLy-Updater/{{VERSION}} (...)` | Optional `User-Agent` header sent on HTTP requests; `{{VERSION}}` is replaced at runtime with the running version |
| `xApiKey` | _(empty)_ | Optional `X-Api-Key` header sent on HTTP requests |
| `defaultMappingDCSubnets` | `DC-RM2:172.16.96.0/24` | Per-site map of domain controller name to its internal CIDR subnets: `dc:cidr[,cidr...][;dc:cidr[,cidr...]...]`; empty disables the startup source check |

### `[criticalUpdate]`

| Key | Default | Description |
|---|---|---|
| `criticalWarningEnabled` | `true` | Show a countdown dialog in the user's session before a forced close |
| `criticalWarningSeconds` | `30` | Countdown duration; warning language follows EMLy's `LANGUAGE` key (fallback `en`) |

### `[selfUpdate]`

| Key | Default | Description |
|---|---|---|
| `enabled` | `true` | Keep the updater itself up to date (see [Self-update](#self-update)) |
| `manifestURL` | _(empty)_ | Leave empty to derive it from the manifest URL in use (`.../manifest` → `.../manifest/updater`), so each site self-updates from its own mirror. Set it only to point at a different host; it then applies to every source, with no fallback |

### `[certificate]`

| Key | Default | Description |
|---|---|---|
| `enabled` | `true` | Install the 3gIT code-signing certificate into `Root` + `TrustedPublisher`, for the machine and for the console user. Makes EMLy's setup elevate as a verified publisher instead of "Unknown publisher". Re-checked every cycle; idempotent. |

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
EMLyUpdater.exe install     # register auto-start service + Event Log source (admin)
EMLyUpdater.exe uninstall   # stop + remove the service, keep ProgramData
EMLyUpdater.exe start|stop  # control the service
EMLyUpdater.exe run         # foreground debug mode (console logging)
```

## Logs

- Rolling file: `C:\ProgramData\EMLyUpdater\logs\updater.log` (5 MB × 5)
- InnoSetup install logs: `C:\ProgramData\EMLyUpdater\logs\emly-install-<version>.log`
- Self-update install logs: `C:\ProgramData\EMLyUpdater\logs\updater-selfinstall-<version>.log`
- Windows Event Log (source `EMLyUpdater`): update found (100), install ok
  (200) / failed (201), forced kill (300), associations repaired (400),
  self-update started (800) / completed (801) / refused or abandoned (802)

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
