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
main.go                  Subcommands: install | uninstall | start | stop | run (foreground debug) | show-toast (internal, see notify/)
proto/                   updateripc.proto - IPC wire schema, manually synced with the emly repo
tools/genversion/        go generate helper: propagates versioninfo.json's version everywhere else
internal/
  config/                INI loader; reset.go rewrites config.ini from this build's defaults on
                         upgrade (previous kept as config.prev.ini); paths.go owns all %ProgramData%\EMLyUpdater\* paths + ExeDir helpers
  source/                Source interface + HTTPSource (with User-Agent / X-Api-Key headers) + Resolver (retry/backoff)
  manifest/              JSON manifest parse/compare (go-version for semver); updater.go is the updater's own release manifest
  download/              Download manager: Ensure = fetch+SHA256 verify; atomic writes. Prefix keeps
                         EMLy's cache and the updater's own from sweeping each other away
  authenticode/          WinVerifyTrust + signer-thumbprint pinning, for the updater's own setup
  selfupdate/            The self-update rules (Reconcile/Decide, pure) + the detached setup launch
  installer/             Runs InnoSetup /VERYSILENT and verifies via EMLy's config.ini
  service/               Windows service handler + RunLoop / Cycle state machine + IPC server lifecycle;
                         sourcepolicy.go picks the manifest source from the detected DC at startup;
                         selfupdate.go orchestrates the updater updating itself
  state/                 state.json: pending update entry, written atomically, survives reboots
  logging/               Two sinks: lumberjack rolling file + Windows Event Log; exe-side log
  notify/                WTS warning dialog + update-complete toast launcher (SYSTEM -> user-session hop) in the active user session
  toast/                 Notification-area balloon (Shell_NotifyIcon) with EMLy's icon; runs inside the user session, launched via `show-toast`
  process/               Kernel wait on EMLy process handle + TerminateProcess for forced updates
  assoc/                 HKLM file-association self-heal after install
  cert/                  Embedded 3gIT code-signing certificate + install into Root/TrustedPublisher (machine + console user)
  ipc/                   Named-pipe server exposing SystemInfo/ADStatus to the EMLy client (protobuf)
```

See [README.md](README.md) for the full update-state-machine table and update-sources description.

## Key Conventions

- **Config is never shipped** - `config.default.ini` is embedded via `//go:embed` and written to `%ProgramData%\EMLyUpdater\config.ini` only when the file is absent. Per-machine edits do **not** survive upgrades: `cmdInstall` calls `config.Reset`, which backs the existing file up to `config.prev.ini` and rewrites `config.ini` from this build's embedded defaults. A setting that must persist has to be re-applied after the upgrade.
- **ProgramData survives uninstall** - `cmdUninstall` deletes the service but never removes `%ProgramData%\EMLyUpdater`. The InnoSetup `[UninstallRun]` block does the same.
- **Exe-dir log is preserved on uninstall** - `cmdUninstall` copies `<ExeDir>\updater.log` to `%ProgramData%\EMLyUpdater\logs\updater-final.log` before the InnoSetup uninstaller can delete the exe directory.
- **SHA256 is mandatory** - a setup whose checksum is missing or wrong is never executed. This applies to resumed pending installs too (re-verified before use).
- **The Updater's target version always wins over what's already installed** - `Updater.install` (`internal/service/service.go`) trusts `installer.VerifyInstalled` (config.ini's `GUI_SEMVER`), not the setup's own exit code. If a run doesn't leave config.ini reporting the target version - setup failure or a clean exit that still doesn't match (e.g. EMLy's installer treating a stale/inconsistent prior install as already current) - `installer.Uninstall` wipes the existing install via EMLy's own `unins*.exe` (best-effort; a missing uninstaller is not an error) and the setup is retried once against a clean slate. Still mismatched after that → the pending entry stays and the whole thing (including the uninstall/reinstall) is retried on the next poll cycle.
- **Atomic state writes** - `state.Store` writes to a temp file then renames, so a crash mid-write cannot corrupt the pending entry.
- **The update source is decided at startup, not just configured** - `applySourcePolicy`
  (`internal/service/sourcepolicy.go`, called from `service.New`) resolves the nearest domain
  controller and forces `primary` to `internal` only when that DC's name is a key in
  `defaultMappingDCSubnets` *and* at least one of this machine's own local IPs falls inside one
  of that key's CIDR subnets; anything else (DC not in the map, no local IP in its subnets,
  machine off the domain) forces `external`. Each site's `internalManifestURL` lives in that
  site's office, so a machine whose own IP is not on that site's subnet cannot reach it. The
  decision is applied in memory *and* written back to `config.ini` via `config.SetPrimary`, and
  is logged every start (event 700; 701 on failure) even when nothing changes. It never switches
  to a source whose manifest URL is empty - that config would fail `Load` on the next start and
  take the service down. Leaving `defaultMappingDCSubnets` empty disables the whole check. It
  runs **once at startup**: a laptop that boots off-site stays `external` until the service
  restarts on a mapped LAN.
- **`config.Load` reads the ini file with `IgnoreInlineComment: true`** - without it, ini.v1
  treats a bare `;` anywhere in a value as the start of an inline comment and silently truncates
  the rest of the line, no error. `defaultMappingDCSubnets` now uses `|` between DC entries, but
  the legacy `;` delimiter written by previous releases is still accepted, so this isn't
  hypothetical: a carried-over two-site value like `DC-RM2:...;DC-CB:...` would load as just
  `DC-RM2:...`, and every site after the first would look "not configured" with nothing in the
  log to explain why - `Load`'s comments live on their own line, never after a value on the same
  line, so this is safe for the whole file, not just this one key.
- **A dead internal manifest endpoint doesn't fail the cycle** - `source.Resolver`
  (`internal/source/resolver.go`) takes an optional `Fallback` source, tried once (no retries)
  after `Primary` exhausts its attempts. `Updater.resolveTarget` wires `externalManifestURL` in
  as that fallback whenever `primary = internal`: the startup DC/subnet check can be correct
  while the internal manifest host itself is down, misconfigured, or firewalled. The fallback is
  used for that fetch only - it is never persisted to `cfg.Primary` or `config.ini`, so the next
  cycle still tries `internal` first.
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
- **`state.json` holds two independent lifecycles** - EMLy's pending update and the updater's own
  self-update record. All four setters go through `Store.update` (read-modify-write); building a
  fresh `State` and saving it, which is what `SetPending` used to do, would silently drop whichever
  entry the other half of the cycle had just written.

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
  until the 60-second stop timeout expired and the install failed. This is why `installer.Run`
  (which does wait) is deliberately not reused here.
- **The outcome is only knowable at the next start.** The launching process does not survive, so
  `state.json`'s `selfUpdate` record is written *before* the launch and reconciled after the restart
  by comparing `version.Version` against it. If the record cannot be persisted, the setup is not
  launched at all - without it the attempt could not be counted.
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
  asking again cannot change it) but still tries the fallback; when nothing serves the endpoint the
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
| `criticalWarningEnabled` | `[criticalUpdate]` | `true` | Show countdown WTS dialog before force-kill |
| `criticalWarningSeconds` | `[criticalUpdate]` | `30` | |
| `enabled` | `[ipc]` | `true` | Enable the named-pipe IPC server (see IPC below) |
| `pipeName` | `[ipc]` | `EMLyUpdater` | Exposed as `\\.\pipe\<pipeName>`; must not contain `\` or `/` |
| `enabled` | `[certificate]` | `true` | Install the 3gIT code-signing certificate into `Root` + `TrustedPublisher` (machine + console user) |
| `enabled` | `[selfUpdate]` | `true` | Keep the updater itself up to date |
| `manifestURL` | `[selfUpdate]` | _(empty)_ | Empty = derived from the manifest URL in use (`.../manifest` → `.../manifest/updater`); set only to point at a different host, in which case it applies to every source with no fallback |

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
| `%ProgramData%\EMLyUpdater\logs\updater-selfinstall-<ver>.log` | InnoSetup log of the updater installing itself |
| `%ProgramData%\EMLyUpdater\logs\updater-final.log` | Exe-dir log preserved on uninstall |
| `%ProgramData%\EMLyUpdater\config.prev.ini` | The config as it was before the last reset |
| Windows Event Log → `EMLyUpdater` source | Update found (100), install ok (200)/failed (201), forced kill (300), assoc repair (400), IPC client rejected (600), IPC unavailable (601), source policy decision (700)/failure (701), cert installed (702), cert install failed (703), self-update started (800)/completed (801)/refused or abandoned (802) |

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

- **Adding a new config key**: update `Config` struct, `Load()`, and `config.default.ini` (all three, otherwise the key is invisible to callers and missing from freshly seeded configs). Upgrades pick it up for free — `config.Reset` rewrites the file from the embedded defaults, so a new key arrives with its default and its comment (and any per-machine edit is discarded).
- **Rotating the code-signing certificate now also gates self-update**: `internal/authenticode` pins the signer to whatever `cert.Embedded()` holds, so a release signed with the *new* certificate cannot be self-installed by machines still running a build that embeds only the old one. Ship the new certificate in a release signed with the old one first, let the fleet take it, and only then start signing with the new one.
- **Editing `proto/updateripc.proto`**: copy the change verbatim to `emly/proto/updateripc.proto` and regenerate both repos' `ipcpb` packages. The two repos share no Go module, so nothing enforces this automatically — a one-sided edit silently desyncs the wire protocol.
- **Cutting an EMLyUpdater release**: bump `versioninfo.json`'s `StringFileInfo.FileVersion`/`ProductVersion` (the single source of truth for the version string — see `tools/genversion`) and run `go generate ./...`. That regenerates `internal/version/version_generated.go` and rewrites the version token in `installer/installer.iss` (`ApplicationVersion`) — no other file should ever hardcode the version string by hand again. (`config.default.ini` is deliberately *not* patched any more: its `userAgent` carries a `{{VERSION}}` placeholder resolved at runtime.) Then publish the release to the updater manifest — the signed installer plus its SHA256 on `/v2/updates/manifest/updater`, on the public API **and** on every site's internal mirror — or no machine will pick it up by itself. Then update the **EMLyUpdater max** column of the compatibility matrix atop `proto/updateripc.proto` to the version being shipped, even if the release doesn't touch `internal/ipc` at all — otherwise the matrix silently goes stale. (That file is manually synced with the `emly` repo, so copy the edit there too.) Do **not** touch `MaxCompatibleEMLyVersion` here: despite living in this repo it tracks *EMLy's* releases, not this one's, and bumping it for an EMLyUpdater release would claim compatibility with an EMLy build that may not exist. Bump it — and the matrix's EMLy max column — when *EMLy* cuts a release. Bump `MinCompatibleEMLyVersion` only when this release genuinely requires a newer EMLy build. Mirror `MaxCompatibleUpdaterVersion` on the `emly` side the same way when *that* repo cuts a release.
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
- **HTTP headers**: set them in `HTTPSource` only - the `Resolver` itself is header-agnostic.
- **`logging.New` signature**: `(logDir, exeLogPath, console)` - passing an empty string for `exeLogPath` disables the exe-side sink.
- **InnoSetup version lock**: `installer.iss` uses `{autopf}` and `ArchitecturesInstallIn64BitMode` which require IS 6. IS 5 will refuse to compile it.
- **Update-complete toast**: shown via `internal/toast.Show`, which must run inside the console user's desktop session (session 0, where the SYSTEM service lives, has none). `internal/notify.LaunchToast` does the SYSTEM -> user-session hop with `WTSQueryUserToken` + `CreateProcessAsUser`, re-launching the updater's own exe with the hidden `show-toast` subcommand. The icon shown is extracted at runtime from the installed `EMLy.exe` (`ExtractIconEx`) - there is nothing to keep in sync when EMLy's icon changes. Toast failures (no console session, token/privilege errors, missing icon) are always best-effort/logged, never fail the (already-successful) update.
