# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Read AGENTS.md first

[AGENTS.md](AGENTS.md) is the authoritative guide for this repo: the package map, every
design convention (remote-config document, identity headers, presence channel, self-update,
IPC, code-signing certificate) and the release checklist. This file only covers what it
doesn't: the day-to-day commands and where the rest of the documentation lives.

When you change behaviour that AGENTS.md describes, update AGENTS.md in the same change —
its "Key Conventions" and "Common Pitfalls" sections are the only record of *why* several
non-obvious choices were made.

## Commands

Windows-only. The binary links `golang.org/x/sys/windows`; `go build` will not work on
Linux/macOS. Go 1.26.1 (`go.mod`).

```powershell
go generate                       # goversioninfo -64 + tools/genversion (see Releases below)
go build -ldflags "-s -w" -o build\bin\emly-updater.exe .
go test ./...                     # all tests, pure Go, no admin rights needed
go vet ./...
iscc installer\installer.iss      # Inno Setup 6 only; after the Go build
```

Single package / single test:

```powershell
go test ./internal/policy/
go test ./internal/service/ -run TestBeginCycle -v
go test ./internal/policy/ -run TestValidate/invalid -v
```

Two `Live` tests are gated behind environment variables and skipped by default, because they
exercise real Windows API paths that CI cannot provide:

```powershell
# Real certificate stores, from an ELEVATED shell. Uses a throwaway cert and cleans up after itself.
$env:EMLY_CERT_STORE_TEST=1; go test ./internal/cert/ -run Live -v

# Real WinVerifyTrust against a signed file — no elevation needed. Use it on a freshly built
# installer to prove it is signed the way the self-update path expects.
$env:EMLY_AUTHENTICODE_TEST_FILE="installer\Output\EMLyUpdater_Installer_1.5.0.exe"
$env:EMLY_AUTHENTICODE_TEST_THUMBPRINT="<hex>"   # optional: also assert the signer
go test ./internal/authenticode/ -run Live -v
```

Regenerating protobuf (`go generate ./internal/ipc/ipcpb`) needs `protoc` + `protoc-gen-go`.
The generated file is committed, so neither is needed for a build, `go test ./...`, or CI.

## Running it locally

`emly-updater run` is the foreground debug mode (same loop as the service, logs to console).
A named kernel mutex (`Global\EMLyUpdaterSingleton`) stops it racing an installed service.

To exercise the real code paths without any server infrastructure, see
[README.md](README.md) § *Testing end-to-end without infrastructure*: serve `version.json`,
a setup binary, a `/v2/config` document and (optionally) an updater manifest from
`python -m http.server`, point `ProgramData` at a scratch directory so the machine's real
config/state/logs are untouched, and set `[ipc] enabled = false` so the run doesn't collide
with an installed service over the pipe. The fixtures under `testdata/remoteconfig/` are
ready-made documents for this.

## The cycle, at a glance

`service.RunLoop` (`internal/service/service.go`) is where the whole design meets. Reading it
top to bottom is the fastest way to understand the system, and the order of its steps is
load-bearing — each one is commented with why it sits where it does:

1. `initPolicy` → cached document, or `policy.FromLegacy(config.ini)` if none was ever accepted.
2. `refreshConfig` — one non-retried fetch, so cycle one starts on the current policy.
3. `beginCycle` — resolve the nearest DC, match the machine to a site, build the server chain.
   Called from `RunLoop` and deliberately **not** from `service.New` (SCM start timeout).
4. `runClientWS` — the presence channel, started beside the loop, not inside it.
5. Then each `Cycle`: certificate self-heal → control gate → **self-update first** →
   pending install → poll → download+verify → install.

Everything downstream reads from the effective policy snapshot, never from `config.ini`
directly. `config.ini` is bootstrap only and is never written at runtime.

## Documentation map

| File | What it holds |
|------|---------------|
| [AGENTS.md](AGENTS.md) | Package map, conventions, pitfalls, release checklist, manual verification checklists |
| [README.md](README.md) | Update state machine, source resolution, full config reference, local E2E recipe |
| [ADDING_IPC_EVENTS.md](ADDING_IPC_EVENTS.md) | Step-by-step for a new IPC request/field/error code across both repos (Italian) |
| `docs/superpowers/specs/` | Design docs per feature (remote config, presence WS, cert install, VNC relay) |
| `docs/superpowers/plans/` | The implementation plans those specs were executed from |
| `docs/remote-config.example.json` | A worked remote-configuration document |
| `internal/config/config.default.ini` | Annotated defaults; the embedded bootstrap config |

## Two things that silently desync

Neither has any automatic enforcement — nothing fails if you only change one side.

- **`proto/updateripc.proto`** is copied verbatim between this repo and `emly`. Any edit goes
  to both, and both `ipcpb` packages get regenerated. Same for the compatibility matrix at the
  top of that file.
- **`testdata/remoteconfig/`** is copied verbatim between this repo and `emly-go-api`. Both
  validators run against the same fixtures; that is the only thing keeping the two
  implementations of the document's validation equal. A rule added without its fixture is a
  rule the other side doesn't have.

## Releases

`versioninfo.json` is the single source of truth for the version string. Bump
`StringFileInfo.FileVersion`/`ProductVersion`, run `go generate ./...`, and
`tools/genversion` propagates it into `internal/version/version_generated.go` and
`installer/installer.iss`. Never hardcode the version anywhere else — `config.default.ini`
carries a `{{VERSION}}` placeholder resolved at runtime by `config.BuildUserAgent`.

See AGENTS.md § *Common Pitfalls* → "Cutting an EMLyUpdater release" for the rest: publishing
to the manifest endpoints, the compatibility-matrix column to update, and which
`MaxCompatible*Version` constant belongs to which repo's releases (they are easy to get
backwards).

CI (`.github/workflows/build.yml`, windows-latest) builds, code-signs the exe with
`secrets.CODE_SIGN_PFX`, compiles the installer and signs it too, then uploads it as an
artifact. It does **not** run `go test`.

## Branching

A feature that touches a new IPC event and/or needs an updater version bump goes on a
dedicated branch, never straight to `master` — a `ProtocolVersion` /
`MaxCompatibleEMLyVersion` bump changes the wire compatibility matrix shared with the `emly`
repo, and that wants to be reviewable and revertable as one unit.
