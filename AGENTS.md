# EMLyUpdater - Agent Instructions

## Build & Test

```powershell
# Generate version resources (requires goversioninfo)
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
main.go                  Subcommands: install | uninstall | start | stop | run (foreground debug)
proto/                   updateripc.proto - IPC wire schema, manually synced with the emly repo
internal/
  config/                INI loader; paths.go owns all %ProgramData%\EMLyUpdater\* paths + ExeDir helpers
  source/                Source interface + HTTPSource (with User-Agent / X-Api-Key headers) + UNCSource + Resolver
  manifest/              JSON manifest parse/compare (go-version for semver)
  download/              Download manager: Ensure = fetch+SHA256 verify; atomic writes
  installer/             Runs InnoSetup /VERYSILENT and verifies via EMLy's config.ini
  service/               Windows service handler + RunLoop / Cycle state machine + IPC server lifecycle
  state/                 state.json: pending update entry, written atomically, survives reboots
  logging/               Two sinks: lumberjack rolling file + Windows Event Log; exe-side log
  notify/                WTS warning dialog in the active user session
  process/               Kernel wait on EMLy process handle + TerminateProcess for forced updates
  assoc/                 HKLM file-association self-heal after install
  ipc/                   Named-pipe server exposing SystemInfo/ADStatus to the EMLy client (protobuf)
```

See [README.md](README.md) for the full update-state-machine table and source-fallback description.

## Key Conventions

- **Config is never shipped** - `config.default.ini` is embedded via `//go:embed` and written to `%ProgramData%\EMLyUpdater\config.ini` only when the file is absent. Per-machine edits survive upgrades.
- **ProgramData survives uninstall** - `cmdUninstall` deletes the service but never removes `%ProgramData%\EMLyUpdater`. The InnoSetup `[UninstallRun]` block does the same.
- **Exe-dir log is preserved on uninstall** - `cmdUninstall` copies `<ExeDir>\updater.log` to `%ProgramData%\EMLyUpdater\logs\updater-final.log` before the InnoSetup uninstaller can delete the exe directory.
- **SHA256 is mandatory** - a setup whose checksum is missing or wrong is never executed. This applies to resumed pending installs too (re-verified before use).
- **Atomic state writes** - `state.Store` writes to a temp file then renames, so a crash mid-write cannot corrupt the pending entry.
- **Singleton guard** - a named kernel mutex `Global\EMLyUpdaterSingleton` prevents `run` (foreground debug) from racing the installed service.

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
| `uncRoot` | `[source]` | `\\dc-rm2\logo\update` | UNC fallback share; `version.json` lives here |
| `userAgent` | `[source]` | _(empty)_ | Sent as `User-Agent` on HTTP requests |
| `xApiKey` | `[source]` | _(empty)_ | Sent as `X-Api-Key` on HTTP requests |
| `criticalWarningEnabled` | `[criticalUpdate]` | `true` | Show countdown WTS dialog before force-kill |
| `criticalWarningSeconds` | `[criticalUpdate]` | `30` | |
| `enabled` | `[ipc]` | `true` | Enable the named-pipe IPC server (see IPC below) |
| `pipeName` | `[ipc]` | `EMLyUpdater` | Exposed as `\\.\pipe\<pipeName>`; must not contain `\` or `/` |

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
  (`internal/ipc/version.go`'s `Version` const here, EMLy's `GUI_SEMVER` on the other side), distinct
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
- On upgrade: stops the service first (60 s wait), then replaces the binary

### Manual (admin shell)

```powershell
Copy-Item .\build\bin\emly-updater.exe "C:\Program Files\EMLyUpdater\"
& "C:\Program Files\EMLyUpdater\emly-updater.exe" install
& "C:\Program Files\EMLyUpdater\emly-updater.exe" start
```

### Post-deployment config tweaks

Edit `%ProgramData%\EMLyUpdater\config.ini` (survives upgrades). Changes take effect on the next poll cycle; no service restart needed for most keys.

## Logs & Diagnostics

| File | Content |
|------|---------|
| `%ProgramData%\EMLyUpdater\logs\updater.log` | Rolling 5 MB × 5 - all events |
| `<ExeDir>\updater.log` | Same events, kept next to exe for on-site access |
| `%ProgramData%\EMLyUpdater\logs\emly-install-<ver>.log` | InnoSetup silent install log |
| `%ProgramData%\EMLyUpdater\logs\updater-final.log` | Exe-dir log preserved on uninstall |
| Windows Event Log → `EMLyUpdater` source | Update found (100), install ok (200)/failed (201), forced kill (300), assoc repair (400), source fallback (500), IPC client rejected (600), IPC unavailable (601) |

## Common Pitfalls

- **Adding a new config key**: update `Config` struct, `Load()`, and `config.default.ini` (all three, otherwise the key is invisible to callers and missing from freshly seeded configs).
- **Editing `proto/updateripc.proto`**: copy the change verbatim to `emly/proto/updateripc.proto` and regenerate both repos' `ipcpb` packages. The two repos share no Go module, so nothing enforces this automatically — a one-sided edit silently desyncs the wire protocol.
- **Cutting an EMLyUpdater release**: bump `internal/ipc/version.go`'s `Version` const and `MaxCompatibleEMLyVersion` to the release being shipped, even if the release doesn't touch `internal/ipc` at all — otherwise the compatibility matrix and the (informational) forward-compat log silently go stale. Bump `MinCompatibleEMLyVersion` only when this release genuinely requires a newer EMLy build. Mirror `MaxCompatibleUpdaterVersion` on the `emly` side the same way when *that* repo cuts a release.
- **HTTP headers**: set them in `HTTPSource` only - `UNCSource` and the `Resolver` are header-agnostic.
- **`logging.New` signature**: `(logDir, exeLogPath, console)` - passing an empty string for `exeLogPath` disables the exe-side sink.
- **InnoSetup version lock**: `installer.iss` uses `{autopf}` and `ArchitecturesInstallIn64BitMode` which require IS 6. IS 5 will refuse to compile it.
