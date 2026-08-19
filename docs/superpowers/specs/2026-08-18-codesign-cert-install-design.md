# Automatic installation of the 3gIT code-signing certificate

Design document — 2026-08-18

## 1. Problem

EMLy and its InnoSetup installer are signed with a self-signed code-signing
certificate (`CN=3G IT Innovation`). Because the certificate is not present in
any Windows trust store, the UAC elevation prompt shown when the setup runs
reads *"Unknown publisher"* instead of naming 3gIT.

The updater already runs on every machine as a LocalSystem service and already
owns comparable per-machine self-heal work (`internal/assoc` repairs HKLM file
associations). Installing the certificate is the same class of task and belongs
in the same place.

## 2. Goals

- On every machine running EMLyUpdater, the 3gIT code-signing certificate is
  present in `LocalMachine\Root` and `LocalMachine\TrustedPublisher`.
- It is also present in `Root` and `TrustedPublisher` of the interactive
  console user, when one is logged on.
- The operation is idempotent, self-healing, and never fails an update cycle.

## 3. Non-goals

- **SmartScreen is not addressed.** SmartScreen is a cloud reputation service;
  it does not consult local trust stores. A self-signed certificate has no
  reputation, and installing it locally does not create any. Only a
  publicly-issued OV/EV certificate (or an enterprise policy exclusion) changes
  SmartScreen behaviour. Note also that setups downloaded by the updater's own
  Go HTTP client receive no Mark-of-the-Web, so SmartScreen does not gate that
  path today regardless.
- **Users other than the active console user** are out of scope. Manipulating
  the registry hives of logged-off profiles is invasive and can corrupt them;
  the machine store already covers UAC for every user.
- **Certificate rotation automation** is out of scope. Rotation is a release
  activity (see §12).

## 4. The certificate

Facts established from `certs/3GITInnovation.cer` on 2026-08-18:

| Property | Value |
|---|---|
| Subject / Issuer | `CN=3G IT Innovation` (self-signed) |
| Validity | 2026-06-16 → **2027-06-16** (1 year) |
| Public key | RSA 2048 |
| Key Usage | Digital Signature (critical) |
| Extended Key Usage | Code Signing |
| Basic Constraints | absent → **not a CA** |
| Signature algorithm | `sha1WithRSAEncryption` |
| SHA-256 thumbprint | `BFD4D8090131E81E11E3EC216839EB709389D97ACC2A5B53C46FA710BE7268EB` |
| Encoding | DER, 778 bytes |

Two consequences drive the design:

**It is an end-entity certificate, not a CA.** For a self-signed code-signing
certificate the chain is one element long and terminates at itself, so it must
be in `Root` for the chain to validate at all, and in `TrustedPublisher` for
the publisher to be trusted. Both stores are required; neither alone suffices.

**It expires 2027-06-16**, roughly ten months after this document. Rotation is
therefore annual in practice, not the 1–3 years assumed when the embedding
approach was chosen. §12 covers the mitigation.

## 5. Approach: embed, do not download

The certificate is embedded in the updater binary with `//go:embed`, mirroring
how `internal/config/config.default.ini` is already handled.

The alternative — downloading it from the EMLy API — was rejected. A downloaded
trust anchor must be pinned to an expected thumbprint, or anyone able to
intercept the (plain-HTTP, by default `http://172.16.96.73:8080`) internal
source could have their own certificate installed fleet-wide. Once the
thumbprint has to be compiled into the binary anyway, embedding the 778-byte
certificate itself removes the endpoint, the network failure mode, and the
attack surface at no cost.

The trade-off accepted: a new certificate requires a new updater release.

## 6. Architecture

### 6.1 `internal/cert` — new package

Owns the certificate bytes and all crypt32 interaction. Knows nothing about
sessions, tokens, or configuration.

```go
package cert

//go:embed 3GITInnovation.cer
var embedded []byte

// Embedded returns the parsed embedded 3gIT code-signing certificate
// alongside its raw DER encoding.
func Embedded() (*x509.Certificate, []byte, error)

// Target identifies one Windows certificate store.
type Target struct {
    Location uint32 // CERT_SYSTEM_STORE_LOCAL_MACHINE or CERT_SYSTEM_STORE_USERS
    SID      string // required when Location is ..._USERS; empty otherwise
    Name     string // "Root" or "TrustedPublisher"
}

// MachineTargets returns the two LocalMachine stores.
func MachineTargets() []Target

// UserTargets returns the two per-user stores for sid.
func UserTargets(sid string) []Target

// Ensure adds der to every target store that does not already hold it.
// It is idempotent. Returns the stores actually written, and the first
// error encountered; a failure on one target does not skip the others.
func Ensure(der []byte, targets []Target, logf func(string, ...any)) ([]string, error)
```

The `.cer` is copied from `certs/` into `internal/cert/` because `//go:embed`
cannot reference paths above the package directory. `certs/3GITInnovation.cer`
remains the source of record; the copy is a build input.

### 6.2 Store access

Verified present in `golang.org/x/sys/windows` v0.46.0 (already a dependency —
no new modules, no `certutil.exe` subprocess):

```go
store, err := windows.CertOpenStore(
    windows.CERT_STORE_PROV_SYSTEM,       // 10, the W variant
    0,                                    // encoding: unused for system stores
    0,                                    // hCryptProv: none
    target.Location,                      // ..._LOCAL_MACHINE or ..._USERS
    uintptr(unsafe.Pointer(nameUTF16)),   // "Root" | "<SID>\\Root"
)
defer windows.CertCloseStore(store, 0)

ctx, err := windows.CertCreateCertificateContext(
    windows.X509_ASN_ENCODING|windows.PKCS_7_ASN_ENCODING,
    &der[0], uint32(len(der)))
defer windows.CertFreeCertificateContext(ctx)

err = windows.CertAddCertificateContextToStore(
    store, ctx, windows.CERT_STORE_ADD_NEW, nil)
```

`CERT_STORE_READONLY_FLAG` must **not** be set. The store name for a
`CERT_SYSTEM_STORE_USERS` target is the SID string, a backslash, and the store
name: `S-1-5-21-…-1001\Root`.

**Per-user targets must additionally set `CERT_SYSTEM_STORE_UNPROTECTED_FLAG`**
(`0x40000000`). The user `Root` store is a *protected root*: adding to it
through the ordinary path is designed to raise an interactive confirmation
dialog, and session 0 — where this service lives — has no desktop to draw one
on. The flag writes the underlying store directly and bypasses that machinery.
The machine `Root` store is gated by administrator rights rather than by a
prompt and needs no such flag. The location bits (`0x00060000`) and this flag
occupy different parts of the flags word, so they simply OR together.

*(This requirement was identified while writing the implementation plan, after
the first draft of this document. The Task 0 probe tests the flagged path.)*

### 6.3 Idempotency via `CERT_STORE_ADD_NEW`

No separate presence check is performed. `CERT_STORE_ADD_NEW` fails with
`CRYPT_E_EXISTS` (`0x80092005`) when the identical certificate is already in
the store and succeeds when it adds it. That return value *is* the
already-present / just-installed signal, obtained atomically. A preceding
`CertFindCertificateInStore` would add code and a race window between check and
add, for no benefit.

`CRYPT_E_EXISTS` arrives as a `syscall.Errno` carrying the HRESULT; match on
the numeric value rather than on `windows.CRYPT_E_EXISTS`, which is typed
`windows.Handle`.

`CERT_STORE_ADD_NEW` compares the whole certificate, not the subject. During
rotation the old and new 3gIT certificates therefore coexist in the store,
which is the desired behaviour: signatures made with the old certificate keep
validating.

### 6.4 Resolving the console user's SID

`internal/notify` already owns the WTS machinery (`WTSGetActiveConsoleSessionId`,
`WTSQueryUserToken`, and the `enablePrivilege("SeTcbPrivilege")` helper that
`WTSQueryUserToken` needs from a service token). It gains one exported
function:

```go
// ConsoleUserSID returns the SID of the interactive console user.
// Returns ok == false when no user is logged on at the console.
func ConsoleUserSID() (sid string, ok bool)
```

Implementation: active console session id → `WTSQueryUserToken` →
`Token.GetTokenUser()` → `.User.Sid.String()`.

This keeps the boundary clean: WTS/token code stays in `notify`, crypt32 stays
in `cert`, and `service` composes the two. It also avoids the alternative
design — a hidden `install-cert --user` subcommand relaunched into the user's
session via `CreateProcessAsUser`, the way `notify.LaunchToast` works. That
alternative is the documented fallback if §14's probe fails.

Reaching another user's store through `CERT_SYSTEM_STORE_USERS` requires that
user's registry hive to be loaded under `HKU`, which holds for a logged-on
user — precisely the scope of this design.

### 6.5 Wiring

A new method on `*Updater`:

```go
// ensureCertificate installs the 3gIT code-signing certificate into the
// machine and console-user trust stores. Best-effort: every failure is
// logged and swallowed, never returned.
func (u *Updater) ensureCertificate()
```

Called from:

- the top of `Updater.Cycle`, before the pending-update check;
- `cmdInstall` in `main.go`, so the certificate is in place before the service
  first starts.

## 7. Frequency: every cycle, not only at startup

`ensureCertificate` runs at the start of every poll cycle (default: every 15
minutes), not once at service start.

Running only at startup would never cover a user who logs on *after* the
service starts — which, since the service starts at boot and users log on
afterwards, is the normal case rather than the exception. Running every cycle
also self-heals removal of the certificate, and costs two syscalls per store
when it is already present. This mirrors `assoc.Repair`, which is likewise a
periodically-applied backstop rather than a one-shot.

To keep the log readable at 96 cycles a day, `CRYPT_E_EXISTS` logs at `Debug`
level. Only an actual store write logs at `Info` with an Event Log entry.

## 8. Error handling

Every failure path is best-effort and non-fatal, consistent with `assoc` and
the update-complete toast:

| Condition | Behaviour |
|---|---|
| Embedded certificate fails to parse | Log Warn once per cycle; skip entirely. Cannot happen if tests pass. |
| `CertOpenStore` fails on a machine store | Log Warn (event 701); continue to the remaining targets. |
| `CertAddCertificateContextToStore` returns `CRYPT_E_EXISTS` | Log Debug; not an error. |
| No console user, or the user token cannot be queried | Log Debug; machine targets still processed. `ConsoleUserSID` returns the same `false` for both — the first is routine, the second rare, and neither changes the action — so the log line names both possibilities rather than claiming to know which occurred. |
| `certificate.enabled = false` | Skip the whole step silently. |

A failure on one target never short-circuits the others: partial success (for
example machine stores written, user store unreachable) is a valid and useful
outcome.

## 9. Configuration

One new section and one new key:

```ini
[certificate]
; Installa il certificato di code-signing 3gIT negli store Root e
; TrustedPublisher (macchina + utente della sessione console attiva), così
; che EMLy e il suo setup risultino firmati da un editore verificato.
enabled = true
```

`Config.CertificateEnabled bool`, default `true`.

A second key to disable only the per-user half was considered and dropped
(YAGNI): if the per-user path misbehaves the whole step can be turned off, and
the machine store retains what earlier runs already installed.

Per the standing pitfall in AGENTS.md, all three of `Config`, `Load()` and
`config.default.ini` must be updated together.

## 10. Logging

Two new Event Log ids in `internal/logging`, continuing the existing scheme:

| Id | Constant | Level | Meaning |
|---|---|---|---|
| 700 | `EventCertInstalled` | Info | Certificate written to a store. Fields: store, thumbprint, subject. |
| 701 | `EventCertFailed` | Warn | A store could not be opened or written. Never Error — the step is best-effort. |

## 11. Testing

AGENTS.md requires `go test ./...` to pass in CI without administrator rights
and without Windows API calls. The crypt32 paths therefore cannot be unit
tested; the embedded certificate can be, and that is where the failure modes
that actually bite live.

`internal/cert/cert_test.go`, all pure Go:

1. **Parses** — `Embedded()` returns a valid `*x509.Certificate`.
2. **Is a code-signing certificate** — `ExtKeyUsage` contains
   `x509.ExtKeyUsageCodeSigning`. Catches embedding the wrong `.cer`.
3. **Is not near expiry** — fails when fewer than **60 days** remain before
   `NotAfter`. With annual rotation this converts "the certificate expired and
   nobody noticed" into a red build two months ahead of the deadline. This is
   the highest-value test in the file.
4. **Is not yet expired / already valid** — `NotBefore` is in the past.
5. **Thumbprint matches** — SHA-256 equals the constant recorded in §4.
   Replacing the `.cer` without updating the documentation fails the build.
6. **Is self-signed** — `Subject` equals `Issuer`.

Table-driven where it reads naturally, following the style of
`internal/manifest/manifest_test.go`.

### Manual verification (administrator required)

Modelled on the existing "IPC manual verification" section of AGENTS.md:

1. On a clean machine, install the updater and confirm via `certmgr.msc`
   (Local Computer) that the certificate is in both `Root` and
   `Trusted Publishers`.
2. Log on as a standard user, wait one poll cycle, and confirm via
   `certmgr.msc` (Current User) that it is in that user's two stores.
3. Run the EMLy setup and confirm the UAC prompt now reads
   *"Verified publisher: 3G IT Innovation"*.
4. Delete the certificate from `LocalMachine\Root`, wait one cycle, confirm it
   is restored and that event 700 is logged.
5. With no user logged on at the console, confirm the machine stores are still
   maintained and only a Debug line notes the skipped user half.
6. Confirm a second cycle after a successful install logs nothing at Info —
   i.e. the Debug-level `CRYPT_E_EXISTS` path works and the log is not spammed.

## 12. Certificate rotation

The certificate expires **2027-06-16**. Two recommendations, both outside this
repository but recorded here because they determine whether the embedding
approach stays sound:

- **Timestamp the signature** at signing time
  (`signtool /tr <rfc3161-url> /td sha256`). A timestamped signature remains
  valid after the signing certificate expires, so machines still running an
  older updater keep validating binaries signed with the older certificate.
  This removes most of the urgency from propagating a rotation.
- **Issue the next certificate with a 10-year validity and a SHA-256
  signature.** The certificate is self-signed, so its lifetime is a free
  choice; an annual lifetime creates yearly release pressure for no benefit.
  The current `sha1WithRSAEncryption` self-signature is in practice tolerated
  for a root (a root is trusted by presence, its self-signature is not
  validated as part of chain building), but there is no reason to carry it
  forward. This is a recommendation, not a blocking finding — it has not been
  empirically verified on a target machine.

Rotation procedure when it happens: replace `certs/3GITInnovation.cer` and the
copy in `internal/cert/`, update the thumbprint constant and §4 of this
document, let the tests confirm, and cut a release. The old certificate stays
installed on existing machines and continues to validate old signatures.

## 13. Security considerations

Installing this certificate into `Root` + `TrustedPublisher` makes the 3gIT
signing key a full code-trust anchor on every machine in the fleet: anything
signed with it runs without friction. If the private key leaks, the holder can
sign software that every EMLy machine treats as legitimate 3gIT software.

Two concrete mitigations, one already applied:

- **Applied 2026-08-18:** `certs/3GITInnovation.pfx` — which contains the
  private key — was present in the working tree, untracked but *not* ignored,
  while `origin` is the public `github.com/EMLy-mail/emly-updater`. History was
  verified clean (`git log --all --diff-filter=A -- '*.pfx' '*.cer' '*.pem'
  '*.key'` returned nothing), but a single `git add -A` would have published
  it. `.gitignore` now excludes `*.pfx`, `*.key` and `certs/*.pfx`.
- **Recommended:** keep the `.pfx` off general-purpose developer machines and
  protect it with a strong password at minimum; a hardware token is the proper
  answer. Alternatively, a publicly-issued OV certificate removes the need for
  this feature altogether *and* addresses SmartScreen — at roughly €200–400 a
  year.

## 14. Implementation order

The first step de-risks the whole approach and must complete before anything
else is written.

0. **Probe `CERT_SYSTEM_STORE_USERS`.** A throwaway program, run as SYSTEM
   (e.g. via `psexec -s`), that resolves the console user's SID and writes a
   test certificate into `<SID>\Root`. Confirm with `certmgr.msc` in that
   user's context. This validates the one assumption the whole design rests on.
   **If it fails**, fall back to §6.4's alternative — a hidden
   `install-cert --user` subcommand relaunched into the user session with
   `CreateProcessAsUser`, reusing the proven `notify.LaunchToast` pattern —
   and revise this document before continuing. The probe code is discarded
   either way.
1. Track `certs/3GITInnovation.cer` in git — it is currently untracked, and it
   is the public half, safe and necessary to version as the source of record.
2. `internal/cert`: embed, `Embedded()`, `Target`, `MachineTargets`,
   `UserTargets`, `Ensure`, plus `cert_test.go` (§11).
3. `notify.ConsoleUserSID`.
4. `Config.CertificateEnabled` + `config.default.ini` + `Load()`;
   `EventCertInstalled` / `EventCertFailed` in `internal/logging`.
5. `Updater.ensureCertificate`, wired into `Cycle` and `cmdInstall`.
6. Documentation: AGENTS.md (architecture tree, config table, event-id table,
   manual-verification section, rotation pitfall) and README.md.
7. Manual verification per §11 on a real machine.

## 15. Release

Work happens on a dedicated branch, `feat/codesign-cert-install`, not on
`master` — per the branching rule in AGENTS.md this is a feature that needs a
version bump to ship.

It does not touch `proto/updateripc.proto` or `internal/ipc`, so no
synchronisation with the `emly` repository is required. The standard release
checklist still applies at ship time: bump `versioninfo.json`, run
`go generate ./...`, and bump `MaxCompatibleEMLyVersion`.

## 16. Open risks

| Risk | Mitigation |
|---|---|
| `CERT_SYSTEM_STORE_USERS` does not behave as documented from a SYSTEM service | Step 0 probe, before any production code; documented fallback in §6.4 |
| Certificate expires 2027-06-16 and rotation is forgotten | 60-day CI failure margin (§11, test 3); rotation procedure in §12 |
| Private key leak | `.gitignore` applied; key handling recommendations in §13 |
| SHA-1 self-signature rejected on some Windows configuration | Not observed, not verified; §12 recommends SHA-256 at next rotation |
| Users expect SmartScreen to be fixed | Documented as an explicit non-goal (§3) |
