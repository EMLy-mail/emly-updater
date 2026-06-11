# EMLyUpdater

Standalone Windows update service for **EMLy**. Runs as a `LocalSystem`
auto-start service on domain-joined PCs and keeps EMLy current without any
user interaction: it polls an update manifest, downloads and SHA256-verifies
the InnoSetup installer, and applies it silently.

The service is fully independent of EMLy: binary in `C:\Program Files\EMLyUpdater`,
everything else (config, state, logs, download cache) under
`C:\ProgramData\EMLyUpdater`, which survives EMLy uninstall/reinstall and is
never touched by EMLy's own installer.

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

## Configuration

`C:\ProgramData\EMLyUpdater\config.ini` — created from embedded defaults on
first `install`/start if absent (see `internal/config/config.default.ini`).
Update channel follows each machine's `[EMLy].GUI_RELEASE_CHANNEL` unless
`channelOverride` forces `stable`/`beta` fleet-wide.

## Build

```
go build -ldflags "-s -w" -o build\EMLyUpdater.exe .
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
