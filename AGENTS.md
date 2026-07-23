# EMLyUpdater - Agent Instructions

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
main.go                  Subcommands: install | uninstall | start | stop | run (foreground debug)
proto/                   updateripc.proto - IPC wire schema, manually synced with the emly repo
tools/genversion/        go generate helper: propagates versioninfo.json's version everywhere else
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
desktop app over a named pipe. The service runs as LocalSystem; tampering with this channel is
meant to require Administrator, not just a logged-in user:

- Pipe DACL (`internal/ipc/sddl.go`) grants Authenticated Users connect/read/write only — never
  `GENERIC_WRITE`, which on a pipe object implicitly includes `FILE_CREATE_PIPE_INSTANCE` and would
  let any user squat the pipe name.
- Every connection is authenticated (`internal/ipc/auth.go`): the connecting process's image path
  must match `assoc.ExePath(cfg.EMLyInstallDir, cfg.EMLyExeName)`. Any failure rejects the
  connection — never fails open. (There is deliberately no Authenticode signature/thumbprint
  check on top of the path check — it was removed as too much operational friction for the
  security it added on top of an already-admin-gated install path.) This runs before either
  protocol below ever reads a byte from the wire.

**Dual-protocol server, two wire formats on the same pipe:**

- **v1 (frozen)** — the original one-shot exchange: one `Envelope` request in, one `Envelope`
  response out, then the connection closes (`handleLegacyConn` in `server.go`). Kept forever,
  unmodified, so already-deployed EMLy builds older than 2.1.0 keep working against a 1.3.0+
  EMLyUpdater without ever being touched.
- **v2 (current)** — an explicit handshake of dedicated, tag-prefixed messages (no `Envelope`/
  `oneof` wrapper): `ClientHello → ServerAnswHello`, `ClientSemverSend → ServerSemverOk |
  ServerSemverReject`, `ServerRequestAuthChallenge → ClientAuthResponse` (HMAC-SHA256 over a
  static shared secret — `internal/ipc/handshake_secret.go` — as defense-in-depth on top of the
  ACL/PID-path check above, not a replacement for it), then the payload request/response
  (`ClientSystemInfoRequest`/`ClientADStatusRequest` → the matching `Server*Response`). Driven by
  `handleHandshake` in `handshake.go`.
- **Discriminator**: every wire frame's first byte decides which protocol a connection is
  speaking. `MaxFrameSize` is 64KiB, so a v1 client's first byte (the most-significant byte of its
  4-byte length prefix) is always `0x00`; any other first byte is a v2 `FrameType` tag
  (`FRAME_TYPE_UNSPECIFIED = 0` is reserved and never sent as a real tag). This is deterministic,
  not heuristic — see `TestLegacyLengthPrefixFirstByteAlwaysZero`. `handleConn` reads exactly this
  one byte, then branches to `handleLegacyConn` or `handleHandshake`. Only the server needs this:
  a new EMLy talking to an old, not-yet-upgraded EMLyUpdater is out of scope and expected to fail.
- The pre-handshake `UNAUTHORIZED` rejection (auth.go's `verifyClient` failing) is always sent in
  the **legacy** `Envelope` shape, since no wire read has happened yet at that point and the server
  doesn't know which dialect the peer speaks — every client (v1 and v2) can be made to decode it.
- **`proto/updateripc.proto` is manually synced with the same file in the `emly` repo** — there is
  no shared Go module between the two repos. Copy changes verbatim both ways and regenerate both
  sides' `ipcpb` packages (`go generate ./internal/ipc/ipcpb`, requires `protoc`+`protoc-gen-go`,
  not required for `go build`/CI since generated code is committed).
- **Versioning**: separate compatibility rows for each protocol version (frozen v1 consts vs. live
  v2 consts, both in `internal/ipc/version.go`: `MinCompatibleEMLyVersionV1`/`V2` and their `Max`
  counterparts; mirrored in `emly`'s `backend/utils/updateripc/version.go`). Each side enforces the
  *min* half of its own row and rejects an older peer (`UNSUPPORTED_VERSION` for v1,
  `ServerSemverReject` for v2) even when the wire/schema version matches — not every required fix
  changes the wire format. The *max* half is informational only (logged, never enforced): a newer
  peer is assumed forward-compatible unless proven otherwise. See the compatibility matrix atop
  `proto/updateripc.proto`, and bump `Version`/`MaxCompatible*VersionV2` on every EMLyUpdater or
  EMLy release — see Common Pitfalls below.

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
6. Dual-protocol regression: an **old** EMLy build (pre-handshake, protocol_version 1) against this
   1.3.0+ EMLyUpdater still gets `SystemInfo`/`ADStatus` exactly as before — confirms
   `handleLegacyConn` is unaffected by the v2 addition.
7. New handshake happy path: an EMLy 2.1.0+ client completes the full v2 exchange (`ClientHello`
   through the payload response) against this server.
8. Auth-challenge tamper test: a build with a deliberately wrong `sharedSecret` byte gets rejected
   at `ClientAuthResponse` (`ERROR_CODE_UNAUTHORIZED`, service logs event 602) instead of hanging
   or crashing.

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
| Windows Event Log → `EMLyUpdater` source | Update found (100), install ok (200)/failed (201), forced kill (300), assoc repair (400), source fallback (500), IPC client rejected (600), IPC unavailable (601), IPC v2 handshake failed (602) |

## Common Pitfalls

- **Adding a new config key**: update `Config` struct, `Load()`, and `config.default.ini` (all three, otherwise the key is invisible to callers and missing from freshly seeded configs).
- **Editing `proto/updateripc.proto`**: copy the change verbatim to `emly/proto/updateripc.proto` and regenerate both repos' `ipcpb` packages. The two repos share no Go module, so nothing enforces this automatically — a one-sided edit silently desyncs the wire protocol.
- **Editing `internal/ipc/handshake_secret.go`**: copy the byte slice verbatim to `emly/backend/utils/updateripc/handshake_secret.go`. Same manual-sync posture as the proto file — nothing enforces this automatically, and a one-sided edit makes every v2 `ClientAuthResponse` fail HMAC verification, rejecting every client.
- **Cutting an EMLyUpdater release**: bump `versioninfo.json`'s `StringFileInfo.FileVersion`/`ProductVersion` (the single source of truth for the version string — see `tools/genversion`) and run `go generate ./...`. That regenerates `internal/ipc/version_generated.go` and rewrites the version tokens in `installer/installer.iss` (`ApplicationVersion`) and `internal/config/config.default.ini` (`userAgent`) — no other file should ever hardcode the version string by hand again. Also bump `MaxCompatibleEMLyVersionV2` in `internal/ipc/version.go` to the release being shipped, even if the release doesn't touch `internal/ipc` at all — otherwise the compatibility matrix and the (informational) forward-compat log silently go stale. Never bump the frozen `V1` consts. Bump `MinCompatibleEMLyVersionV2` only when this release genuinely requires a newer EMLy build. Mirror `MaxCompatibleUpdaterVersionV2` on the `emly` side the same way when *that* repo cuts a release.
- **HTTP headers**: set them in `HTTPSource` only - `UNCSource` and the `Resolver` are header-agnostic.
- **`logging.New` signature**: `(logDir, exeLogPath, console)` - passing an empty string for `exeLogPath` disables the exe-side sink.
- **InnoSetup version lock**: `installer.iss` uses `{autopf}` and `ArchitecturesInstallIn64BitMode` which require IS 6. IS 5 will refuse to compile it.
