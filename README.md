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
| **Network** | HTTPS access to the external manifest URL, **or** LAN access to an internal HTTP manifest, **or** access to a UNC share - at least one source must be reachable |
| **Build-time** | Go 1.22+ and [Inno Setup 6](https://jrsoftware.org/isdl.php) (only to compile the installer; not needed for the service itself) |

## How an update is applied

| EMLy state | Behavior |
|---|---|
| Not running | Install immediately |
| Running, normal update | Queue to `state.json`, wait for EMLy to exit (kernel wait, no polling), then install |
| Running, **forced** update (`isCritical` or installed < `minRequiredVersion`) | Optional WTS warning box in the user's session (countdown, localized it/en), then `TerminateProcess` and install |
| EMLy not installed (no `config.ini`) | Fresh-install mode: treat installed version as `0.0.0` and install the channel target (channel = `channelOverride` or `stable`) |

After every successful install the service re-reads `GUI_SEMVER` from EMLy's
`config.ini` to confirm the version, and self-heals the machine-wide
`.eml`/`.msg` HKLM file associations (+ `SHChangeNotify`).

A queued update survives reboots: the pending entry lives in
`C:\ProgramData\EMLyUpdater\state.json` (written atomically) and its setup is
checksum-re-verified before any resumed install. A setup whose SHA256 does not
match the manifest is **never** executed.

## Update sources

The manifest source order is: configured primary (`external` HTTPS or
`internal` LAN HTTP, 3 attempts with backoff) → UNC share fallback. The setup
binary is always fetched from the same source that served the manifest.

The UNC share keeps the conventions EMLy's in-app updater already uses:
manifest at `<uncRoot>\version.json`, `stableDownload`/`betaDownload` are
filenames relative to the share root, and `sha256Checksums` is keyed by
filename. HTTP manifests key checksums by version and carry full download URLs.

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
Edits survive upgrades and uninstall. Changes take effect on the next poll cycle.

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
| `uncRoot` | `\\dc-rm2\logo\update` | UNC fallback share; `version.json` lives here |
| `userAgent` | _(empty)_ | Optional `User-Agent` header sent on HTTP requests |
| `xApiKey` | _(empty)_ | Optional `X-Api-Key` header sent on HTTP requests |

### `[criticalUpdate]`

| Key | Default | Description |
|---|---|---|
| `criticalWarningEnabled` | `true` | Show a countdown dialog in the user's session before a forced close |
| `criticalWarningSeconds` | `30` | Countdown duration; warning language follows EMLy's `LANGUAGE` key (fallback `en`) |

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
- Windows Event Log (source `EMLyUpdater`): update found (100), install ok
  (200) / failed (201), forced kill (300), associations repaired (400), source
  fallback (500)

## Testing end-to-end without infrastructure

Point `uncRoot` at a local folder containing a `version.json` and a setup, set
an unreachable `externalManifestURL`, then `EMLyUpdater.exe run`. Tamper with
the checksum in `version.json` to watch the refusal path; set
`"isCritical": true` while EMLy is open to exercise the warn-and-kill path.
