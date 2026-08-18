# 3gIT Code-Signing Certificate Install — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make EMLyUpdater install the self-signed 3gIT code-signing certificate into the Windows `Root` and `TrustedPublisher` stores — for the machine and for the logged-on console user — so EMLy's setup elevates as a verified publisher instead of "Unknown publisher".

**Architecture:** A new `internal/cert` package owns the embedded certificate (`//go:embed`) and all crypt32 interaction. `internal/notify` gains one function to resolve the console user's SID. `internal/service` composes the two and calls them at the top of every poll cycle, best-effort, exactly the way `internal/assoc` self-heals file associations. No network, no subprocess, no new module dependency.

**Tech Stack:** Go 1.26.1, `golang.org/x/sys/windows` v0.46.0 (already a dependency — supplies every crypt32 call and constant needed), `gopkg.in/ini.v1` for config, standard `crypto/x509` for the tests.

**Spec:** `docs/superpowers/specs/2026-08-18-codesign-cert-install-design.md`

## Global Constraints

- **Windows-only.** The binary imports `golang.org/x/sys/windows`. Never attempt to build or test on Linux/macOS. CI runs on `windows-latest`.
- **`go test ./...` must pass without administrator rights.** Tests may *import* `golang.org/x/sys/windows`, but must never *call* a privileged Windows API. Everything crypt32 is verified manually (Task 8).
- **Best-effort, always.** No certificate failure may ever fail an update cycle or block service installation. Log and continue — the same rule `internal/assoc` and the update-complete toast already follow.
- **Adding a config key touches exactly three places:** the `Config` struct, `Load()`, and `internal/config/config.default.ini`. Miss one and the key is either invisible to callers or absent from freshly seeded configs.
- **Comments in `config.default.ini` are Italian**; Go doc comments and log messages are English. Match the surrounding file.
- **Branch:** `feat/codesign-cert-install` (already created, already holds the spec commit). Do not commit to `master`.
- **Certificate facts** (verify against these, never re-derive): subject and issuer `CN=3G IT Innovation`, self-signed, RSA 2048, EKU Code Signing, valid 2026-06-16 → 2027-06-16, DER, 778 bytes, SHA-256 `bfd4d8090131e81e11e3ec216839eb709389d97acc2a5b53c46fa710be7268eb`.

## File Structure

| File | Responsibility |
|---|---|
| `internal/cert/3GITInnovation.cer` | Create. The embedded DER bytes. A build input copied from `certs/`. |
| `internal/cert/cert.go` | Create. Embeds and parses the certificate. Pure Go, no Windows calls. |
| `internal/cert/target.go` | Create. `Target` — which store, addressed how. Pure Go string logic, fully unit-testable. |
| `internal/cert/store.go` | Create. The crypt32 calls. The only file that talks to Windows. |
| `internal/cert/cert_test.go` | Create. Tests the certificate itself. |
| `internal/cert/target_test.go` | Create. Tests store addressing. |
| `internal/notify/console_user.go` | Create. `ConsoleUserSID()`. Sits in `notify` because that package already owns the WTS/token machinery. |
| `internal/config/config.go` | Modify. `CertificateEnabled`. |
| `internal/config/config.default.ini` | Modify. `[certificate]` section. |
| `internal/config/config_test.go` | Modify. Two tests for the new key. |
| `internal/logging/logging.go` | Modify. Event ids 700/701. |
| `internal/service/service.go` | Modify. `ensureCertificate()` + call from `Cycle`. |
| `main.go` | Modify. `installCertificate()` + call from `cmdInstall`. |
| `AGENTS.md`, `README.md` | Modify. Architecture, config table, event ids, manual verification, rotation pitfall. |

Splitting `cert.go` / `target.go` / `store.go` is deliberate: it keeps every untestable line (crypt32) in one small file and leaves the rest under test.

---

## Task 0: Probe `CERT_SYSTEM_STORE_USERS` — design gate ✅ PASSED

> **Result, 2026-08-18 — PASSED. Approach A is confirmed; no fallback needed.**
>
> Run as `NT AUTHORITY\SYSTEM` in session 0 via a scheduled task (`schtasks
> /ru SYSTEM`), which is the production context exactly, on Windows 11 Pro
> 10.0.26200. All three assumptions held:
>
> - A LocalSystem process opened the console user's store for writing through
>   `CERT_SYSTEM_STORE_USERS` with the store name `<SID>\Root`.
> - `CERT_SYSTEM_STORE_UNPROTECTED_FLAG` bypassed the protected-root
>   confirmation dialog — no prompt, no block, from a desktop-less session.
> - `CERT_STORE_ADD_NEW` returned `CRYPT_E_EXISTS` on the duplicate add, so
>   the idempotency signal Task 3 depends on behaves as designed.
> - The `LocalMachine\Root` control also passed, confirming the probe itself
>   was sound.
>
> Probe artifacts were removed: binary, output file, and the scratch
> `EMLyProbe` registry key (verified absent with `reg query`).

**This task gated the plan. It has passed — Task 1 onwards may proceed as written.**

The design rests on one unverified assumption: that a LocalSystem process can write into another user's certificate store by opening `CERT_SYSTEM_STORE_USERS` with the store name `"<SID>\Root"`. This is documented behaviour, but it has not been observed on a real machine. Thirty throwaway lines settle it before any production code is written.

**A second thing this probe must settle:** the per-user `Root` store is a *protected root* store. Adding to it through the ordinary path is designed to raise an interactive confirmation dialog — and session 0, where the service lives, has no desktop to draw one on. `CERT_SYSTEM_STORE_UNPROTECTED_FLAG` (`0x40000000`) is the documented way to write the underlying store directly and skip that machinery. The probe tests **with** the flag, because that is what the production code will use.

**Files:**
- Create: `<scratchpad>/certprobe/main.go` — throwaway, never committed

**Interfaces:**
- Consumes: nothing
- Produces: a yes/no answer. On "no", stop and revise §6.4 of the spec to the `CreateProcessAsUser` fallback before continuing.

- [x] **Step 1: Write the probe**

Create `main.go` in a scratch directory (not in the repo):

```go
// Throwaway probe: can a LocalSystem process write into the console user's
// certificate store via CERT_SYSTEM_STORE_USERS? Discard after answering.
package main

import (
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

func main() {
	sid, err := consoleUserSID()
	if err != nil {
		fmt.Println("FAIL: could not resolve console user SID:", err)
		os.Exit(1)
	}
	fmt.Println("console user SID:", sid)

	// A scratch store name, so a failed probe cannot leave junk in Root.
	// Permissions on HKU\<SID>\...\SystemCertificates\<name> are identical.
	for _, storeName := range []string{sid + `\EMLyProbe`, sid + `\Root`} {
		if err := probeStore(storeName); err != nil {
			fmt.Printf("FAIL %s: %v\n", storeName, err)
			os.Exit(1)
		}
		fmt.Printf("OK   %s: opened for write\n", storeName)
	}
	fmt.Println("PASS: CERT_SYSTEM_STORE_USERS is writable from this context")
}

func consoleUserSID() (string, error) {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	proc := kernel32.NewProc("WTSGetActiveConsoleSessionId")
	session, _, _ := proc.Call()
	if uint32(session) == 0xFFFFFFFF {
		return "", fmt.Errorf("no active console session - log a user on first")
	}
	var token windows.Token
	if err := windows.WTSQueryUserToken(uint32(session), &token); err != nil {
		return "", fmt.Errorf("WTSQueryUserToken: %w (are you running as SYSTEM?)", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String(), nil
}

func probeStore(storeName string) error {
	name, err := windows.UTF16PtrFromString(storeName)
	if err != nil {
		return err
	}
	flags := uint32(windows.CERT_SYSTEM_STORE_USERS | windows.CERT_SYSTEM_STORE_UNPROTECTED_FLAG)
	store, err := windows.CertOpenStore(
		windows.CERT_STORE_PROV_SYSTEM, 0, 0, flags, uintptr(unsafe.Pointer(name)))
	runtime.KeepAlive(name)
	if err != nil {
		return fmt.Errorf("CertOpenStore: %w", err)
	}
	defer windows.CertCloseStore(store, 0)
	return nil
}
```

- [x] **Step 2: Build it**

```bash
cd <scratchpad>/certprobe
go mod init certprobe && go get golang.org/x/sys/windows@v0.46.0
go build -o certprobe.exe .
```

- [x] **Step 3: Run it as SYSTEM, with a user logged on at the console**

Requires [PsExec](https://learn.microsoft.com/sysinternals/downloads/psexec) and an elevated shell:

```powershell
psexec -s -accepteula .\certprobe.exe
```

Expected: `PASS: CERT_SYSTEM_STORE_USERS is writable from this context`.

- [x] **Step 4: Decide**

- **PASS** → continue to Task 1. Note in the branch's commit message or a scratch note that the probe passed, and on which Windows build.
- **FAIL on `WTSQueryUserToken`** → you are not running as SYSTEM, or no user is logged on. Fix the setup and re-run; this is not a design failure.
- **FAIL on `CertOpenStore`** → the design assumption is wrong. **Stop.** Revise spec §6.4 to the documented fallback (a hidden `install-cert --user` subcommand relaunched into the user's session with `CreateProcessAsUser`, reusing the `notify.LaunchToast` pattern), then rewrite Tasks 2–4 and 6 against it.

- [x] **Step 5: Clean up**

Delete the scratch directory. Nothing from this task is committed. If the probe created `HKU\<SID>\Software\Microsoft\SystemCertificates\EMLyProbe`, remove that key.

---

## Task 1: Embed the certificate

**Files:**
- Create: `internal/cert/3GITInnovation.cer` (copy of `certs/3GITInnovation.cer`)
- Create: `internal/cert/cert.go`
- Test: `internal/cert/cert_test.go`
- Also track: `certs/3GITInnovation.cer` (currently untracked)

**Interfaces:**
- Consumes: nothing
- Produces: `cert.Embedded() (*x509.Certificate, []byte, error)` — the parsed certificate and its raw DER, in that order. Every later task calls this.

- [ ] **Step 1: Put the certificate files in place**

`//go:embed` cannot reference paths above its own package directory, so the certificate is copied rather than referenced. `certs/3GITInnovation.cer` stays the source of record and gets tracked in git — it is the public half, safe to version.

```bash
mkdir -p internal/cert
cp certs/3GITInnovation.cer internal/cert/3GITInnovation.cer
```

Confirm both are 778 bytes and byte-identical:

```bash
cmp certs/3GITInnovation.cer internal/cert/3GITInnovation.cer && echo identical
```

- [ ] **Step 2: Write the failing test**

Create `internal/cert/cert_test.go`:

```go
package cert

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"testing"
	"time"
)

// wantThumbprint is the SHA-256 of the DER encoding this package is expected
// to embed. Swapping the .cer without updating this constant - and the design
// doc that records it - fails the build on purpose.
const wantThumbprint = "bfd4d8090131e81e11e3ec216839eb709389d97acc2a5b53c46fa710be7268eb"

// expiryMargin is how long before NotAfter these tests start failing. The
// certificate rotates roughly annually, so failing two months early turns a
// silent expiry - which would quietly restore the "Unknown publisher" prompt
// on every machine - into a red build with time left to act.
const expiryMargin = 60 * 24 * time.Hour

func TestEmbeddedParses(t *testing.T) {
	c, der, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded() failed: %v", err)
	}
	if c == nil {
		t.Fatal("Embedded() returned a nil certificate")
	}
	if len(der) == 0 {
		t.Fatal("Embedded() returned empty DER")
	}
}

func TestEmbeddedThumbprint(t *testing.T) {
	_, der, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded() failed: %v", err)
	}
	sum := sha256.Sum256(der)
	if got := hex.EncodeToString(sum[:]); got != wantThumbprint {
		t.Errorf("embedded certificate thumbprint = %s, want %s\n"+
			"If the certificate was deliberately rotated, update wantThumbprint "+
			"and section 4 of the design doc.", got, wantThumbprint)
	}
}

func TestEmbeddedIsCodeSigning(t *testing.T) {
	c, _, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded() failed: %v", err)
	}
	for _, eku := range c.ExtKeyUsage {
		if eku == x509.ExtKeyUsageCodeSigning {
			return
		}
	}
	t.Errorf("embedded certificate has no Code Signing EKU, got %v", c.ExtKeyUsage)
}

func TestEmbeddedIsSelfSigned(t *testing.T) {
	c, _, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded() failed: %v", err)
	}
	if c.Subject.String() != c.Issuer.String() {
		t.Errorf("certificate is not self-signed: subject %q, issuer %q",
			c.Subject, c.Issuer)
	}
}

func TestEmbeddedIsAlreadyValid(t *testing.T) {
	c, _, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded() failed: %v", err)
	}
	if now := time.Now(); now.Before(c.NotBefore) {
		t.Errorf("certificate is not valid yet: NotBefore = %s, now = %s",
			c.NotBefore.Format(time.RFC3339), now.Format(time.RFC3339))
	}
}

func TestEmbeddedNotNearingExpiry(t *testing.T) {
	c, _, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded() failed: %v", err)
	}
	deadline := c.NotAfter.Add(-expiryMargin)
	if time.Now().After(deadline) {
		t.Errorf("embedded code-signing certificate expires %s, which is less "+
			"than %d days away.\nRotate it: issue a new certificate, replace "+
			"certs/3GITInnovation.cer and internal/cert/3GITInnovation.cer, "+
			"update wantThumbprint, and cut a release.",
			c.NotAfter.Format("2006-01-02"), int(expiryMargin.Hours()/24))
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

```bash
go test ./internal/cert/ -v
```

Expected: build failure — `undefined: Embedded`.

- [ ] **Step 4: Write the implementation**

Create `internal/cert/cert.go`:

```go
// Package cert installs the 3gIT code-signing certificate into the Windows
// trust stores, so EMLy and its setup elevate as a verified publisher rather
// than "Unknown publisher".
//
// The certificate is embedded rather than fetched from the update API. A
// trust anchor pulled off the network has to be pinned to a thumbprint
// compiled into the binary anyway - otherwise anyone able to intercept the
// (plain-HTTP by default) internal source could have their own certificate
// trusted fleet-wide. Once the thumbprint has to ship in the binary,
// embedding the 778-byte certificate itself costs nothing and removes the
// endpoint, the network failure mode, and the interception risk outright.
//
// The trade-off accepted: rotating the certificate requires a new release.
package cert

import (
	"crypto/x509"
	_ "embed"
	"fmt"
)

//go:embed 3GITInnovation.cer
var embedded []byte

// Embedded parses and returns the embedded 3gIT code-signing certificate
// together with its raw DER encoding. The DER is what the Windows store APIs
// consume; the parsed certificate is for inspection and logging.
func Embedded() (*x509.Certificate, []byte, error) {
	c, err := x509.ParseCertificate(embedded)
	if err != nil {
		return nil, nil, fmt.Errorf("embedded certificate is not valid DER: %w", err)
	}
	return c, embedded, nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go test ./internal/cert/ -v
```

Expected: all six PASS.

- [ ] **Step 6: Commit**

```bash
git add certs/3GITInnovation.cer internal/cert/
git commit -m "feat(cert): embed the 3gIT code-signing certificate

The public certificate is now versioned and embedded via //go:embed,
alongside tests that assert it is a self-signed code-signing certificate
with the expected thumbprint.

The expiry test fails 60 days before NotAfter rather than on it: the
certificate is annual, and a silent expiry would quietly restore the
\"Unknown publisher\" prompt fleet-wide."
```

---

## Task 2: Store targets

**Files:**
- Create: `internal/cert/target.go`
- Test: `internal/cert/target_test.go`

**Interfaces:**
- Consumes: nothing
- Produces:
  - `cert.Target{Location uint32, SID string, Name string}`
  - `(Target).StoreName() string` — the name `CertOpenStore` expects
  - `(Target).String() string` — a stable log label
  - `(Target).openFlags() uint32` — unexported, used by Task 3
  - `cert.MachineTargets() []Target`
  - `cert.UserTargets(sid string) []Target`

- [ ] **Step 1: Write the failing test**

Create `internal/cert/target_test.go`:

```go
package cert

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestStoreName(t *testing.T) {
	tests := []struct {
		name   string
		target Target
		want   string
	}{
		{
			name:   "machine store uses the bare name",
			target: Target{Location: windows.CERT_SYSTEM_STORE_LOCAL_MACHINE, Name: "Root"},
			want:   "Root",
		},
		{
			name: "user store is prefixed with the SID",
			target: Target{
				Location: windows.CERT_SYSTEM_STORE_USERS,
				SID:      "S-1-5-21-1111111111-2222222222-3333333333-1001",
				Name:     "TrustedPublisher",
			},
			want: `S-1-5-21-1111111111-2222222222-3333333333-1001\TrustedPublisher`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.target.StoreName(); got != tc.want {
				t.Errorf("StoreName() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMachineTargetsCoverBothStores(t *testing.T) {
	targets := MachineTargets()
	if len(targets) != 2 {
		t.Fatalf("MachineTargets() returned %d targets, want 2", len(targets))
	}
	seen := map[string]bool{}
	for _, tg := range targets {
		if tg.Location != windows.CERT_SYSTEM_STORE_LOCAL_MACHINE {
			t.Errorf("target %s has location %#x, want CERT_SYSTEM_STORE_LOCAL_MACHINE",
				tg.Name, tg.Location)
		}
		if tg.SID != "" {
			t.Errorf("machine target %s must not carry a SID, got %q", tg.Name, tg.SID)
		}
		seen[tg.Name] = true
	}
	// Both are required: Root closes the one-element chain of a self-signed
	// certificate, TrustedPublisher makes the publisher trusted. Neither
	// alone produces a verified-publisher UAC prompt.
	for _, want := range []string{"Root", "TrustedPublisher"} {
		if !seen[want] {
			t.Errorf("MachineTargets() is missing the %s store", want)
		}
	}
}

func TestUserTargetsCarryTheSID(t *testing.T) {
	const sid = "S-1-5-21-1111111111-2222222222-3333333333-1001"
	targets := UserTargets(sid)
	if len(targets) != 2 {
		t.Fatalf("UserTargets() returned %d targets, want 2", len(targets))
	}
	for _, tg := range targets {
		if tg.Location != windows.CERT_SYSTEM_STORE_USERS {
			t.Errorf("target %s has location %#x, want CERT_SYSTEM_STORE_USERS",
				tg.Name, tg.Location)
		}
		if tg.SID != sid {
			t.Errorf("target %s has SID %q, want %q", tg.Name, tg.SID, sid)
		}
		if !strings.HasPrefix(tg.StoreName(), sid+`\`) {
			t.Errorf("StoreName() = %q, want it prefixed with %q", tg.StoreName(), sid+`\`)
		}
	}
}

func TestUserTargetsAreUnprotected(t *testing.T) {
	// The per-user Root store is a protected root: adding to it through the
	// ordinary path is meant to raise an interactive confirmation dialog, and
	// session 0 has no desktop to draw one on. The flag is mandatory.
	for _, tg := range UserTargets("S-1-5-21-1-2-3-1001") {
		if tg.openFlags()&windows.CERT_SYSTEM_STORE_UNPROTECTED_FLAG == 0 {
			t.Errorf("user target %s must set CERT_SYSTEM_STORE_UNPROTECTED_FLAG", tg.Name)
		}
		if tg.openFlags()&windows.CERT_SYSTEM_STORE_USERS == 0 {
			t.Errorf("user target %s lost its location bits", tg.Name)
		}
	}
}

func TestMachineTargetsAreNotUnprotected(t *testing.T) {
	// The machine Root store is gated by administrator rights, not by an
	// interactive prompt, so the flag is unnecessary there.
	for _, tg := range MachineTargets() {
		if tg.openFlags() != windows.CERT_SYSTEM_STORE_LOCAL_MACHINE {
			t.Errorf("machine target %s openFlags() = %#x, want %#x",
				tg.Name, tg.openFlags(), uint32(windows.CERT_SYSTEM_STORE_LOCAL_MACHINE))
		}
	}
}

func TestStringIsReadable(t *testing.T) {
	machine := Target{Location: windows.CERT_SYSTEM_STORE_LOCAL_MACHINE, Name: "Root"}
	if got := machine.String(); got != `LocalMachine\Root` {
		t.Errorf("String() = %q, want %q", got, `LocalMachine\Root`)
	}
	user := Target{
		Location: windows.CERT_SYSTEM_STORE_USERS,
		SID:      "S-1-5-21-1-2-3-1001",
		Name:     "Root",
	}
	if got := user.String(); !strings.Contains(got, "S-1-5-21-1-2-3-1001") ||
		!strings.Contains(got, "Root") {
		t.Errorf("String() = %q, want it to name both the SID and the store", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/cert/ -run 'Target|StoreName|String' -v
```

Expected: build failure — `undefined: Target`, `undefined: MachineTargets`, `undefined: UserTargets`.

- [ ] **Step 3: Write the implementation**

Create `internal/cert/target.go`:

```go
package cert

import "golang.org/x/sys/windows"

// storeNames are the two stores a self-signed code-signing certificate has to
// occupy. It is an end-entity certificate, not a CA, so its chain is one
// element long and terminates at itself: Root is what lets that chain
// validate, TrustedPublisher is what makes the publisher trusted. Installing
// into only one of them does not produce a verified-publisher UAC prompt.
var storeNames = []string{"Root", "TrustedPublisher"}

// Target identifies one Windows certificate store.
type Target struct {
	Location uint32 // CERT_SYSTEM_STORE_LOCAL_MACHINE or CERT_SYSTEM_STORE_USERS
	SID      string // required when Location is CERT_SYSTEM_STORE_USERS, empty otherwise
	Name     string // "Root" or "TrustedPublisher"
}

// StoreName is the store name CertOpenStore expects. A store belonging to
// another user is addressed as "<SID>\<store>"; machine stores by bare name.
func (t Target) StoreName() string {
	if t.Location == windows.CERT_SYSTEM_STORE_USERS {
		return t.SID + `\` + t.Name
	}
	return t.Name
}

// String is a stable, human-readable label for logs and Event Log entries.
func (t Target) String() string {
	if t.Location == windows.CERT_SYSTEM_STORE_USERS {
		return `User\` + t.Name + ` (` + t.SID + `)`
	}
	return `LocalMachine\` + t.Name
}

// openFlags is the flags word for CertOpenStore.
//
// Per-user stores add CERT_SYSTEM_STORE_UNPROTECTED_FLAG. The user Root store
// is a "protected root": adding to it through the ordinary path is designed to
// raise an interactive confirmation dialog, and session 0 - where this service
// lives - has no desktop to draw one on. The flag writes the underlying store
// directly and bypasses that machinery. The machine Root store is gated by
// administrator rights instead of by a prompt, so it needs no such flag.
func (t Target) openFlags() uint32 {
	if t.Location == windows.CERT_SYSTEM_STORE_USERS {
		return t.Location | windows.CERT_SYSTEM_STORE_UNPROTECTED_FLAG
	}
	return t.Location
}

// MachineTargets returns the two LocalMachine stores. These cover UAC for
// every user of the machine.
func MachineTargets() []Target {
	out := make([]Target, 0, len(storeNames))
	for _, n := range storeNames {
		out = append(out, Target{Location: windows.CERT_SYSTEM_STORE_LOCAL_MACHINE, Name: n})
	}
	return out
}

// UserTargets returns the two per-user stores belonging to sid. Reaching them
// requires that user's registry hive to be loaded under HKU, which holds while
// they are logged on.
func UserTargets(sid string) []Target {
	out := make([]Target, 0, len(storeNames))
	for _, n := range storeNames {
		out = append(out, Target{Location: windows.CERT_SYSTEM_STORE_USERS, SID: sid, Name: n})
	}
	return out
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/cert/ -v
```

Expected: all PASS (Task 1's six plus Task 2's six).

- [ ] **Step 5: Commit**

```bash
git add internal/cert/target.go internal/cert/target_test.go
git commit -m "feat(cert): address machine and per-user certificate stores

Target names one Windows store and knows how CertOpenStore expects it to
be addressed - a bare name for machine stores, \"<SID>\\<store>\" for
another user's.

Per-user targets set CERT_SYSTEM_STORE_UNPROTECTED_FLAG: the user Root
store is a protected root whose ordinary add path wants to raise an
interactive confirmation dialog, and session 0 has no desktop for one."
```

---

## Task 3: Add the certificate to a store

**Files:**
- Create: `internal/cert/store.go`

**Interfaces:**
- Consumes: `Target`, `(Target).StoreName()`, `(Target).String()`, `(Target).openFlags()` from Task 2
- Produces: `cert.Ensure(der []byte, targets []Target, logf func(string, ...any)) ([]string, error)` — returns the `String()` labels of the stores actually written, plus the first error encountered

There is no unit test for this task: it is nothing but privileged Windows API calls, and the global constraint forbids those in CI. Task 8's manual checklist is its verification. Keep the file small and obvious for exactly that reason.

- [ ] **Step 1: Write the implementation**

Create `internal/cert/store.go`:

```go
package cert

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// cryptEExists is CRYPT_E_EXISTS. Under CERT_STORE_ADD_NEW the store returns
// it when it already holds this exact certificate, and succeeds when it adds
// it - so the return value alone distinguishes "already there" from "just
// installed", atomically. A preceding CertFindCertificateInStore would add a
// lookup and a race window between check and add for no benefit.
//
// Declared here as a plain constant rather than used via
// windows.CRYPT_E_EXISTS, which is typed windows.Handle and does not compare
// against the syscall.Errno the store APIs actually return.
const cryptEExists = 0x80092005

// Ensure adds der to every target store that does not already hold it.
//
// It is idempotent: re-running it once the certificate is installed writes
// nothing and returns an empty slice. logf is called once per store actually
// written; callers that poll should log it below Info.
//
// A failure on one target does not skip the others - installing into the
// machine stores is still worth doing when the user's hive is unreachable, and
// vice versa. The first error is returned once every target has been tried.
func Ensure(der []byte, targets []Target, logf func(string, ...any)) ([]string, error) {
	if len(der) == 0 {
		return nil, errors.New("refusing to install an empty certificate")
	}

	ctx, err := windows.CertCreateCertificateContext(
		windows.X509_ASN_ENCODING|windows.PKCS_7_ASN_ENCODING,
		&der[0], uint32(len(der)))
	if err != nil {
		return nil, fmt.Errorf("certificate rejected by CertCreateCertificateContext: %w", err)
	}
	defer windows.CertFreeCertificateContext(ctx)

	var (
		installed []string
		firstErr  error
	)
	for _, t := range targets {
		added, err := addToStore(ctx, t)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if added {
			installed = append(installed, t.String())
			logf("installed code-signing certificate into %s", t.String())
		}
	}
	return installed, firstErr
}

// addToStore opens t for writing and adds ctx under CERT_STORE_ADD_NEW.
// Returns false with a nil error when the certificate is already present.
func addToStore(ctx *windows.CertContext, t Target) (bool, error) {
	name, err := windows.UTF16PtrFromString(t.StoreName())
	if err != nil {
		return false, fmt.Errorf("invalid store name %q: %w", t.StoreName(), err)
	}

	// CERT_STORE_READONLY_FLAG is deliberately absent: this store is written.
	store, err := windows.CertOpenStore(
		windows.CERT_STORE_PROV_SYSTEM,
		0, // encoding type: unused for system stores
		0, // hCryptProv: none
		t.openFlags(),
		uintptr(unsafe.Pointer(name)),
	)
	// name is passed as a uintptr, which the garbage collector does not treat
	// as a live reference - keep it alive across the call explicitly.
	runtime.KeepAlive(name)
	if err != nil {
		return false, fmt.Errorf("cannot open certificate store %s: %w", t, err)
	}
	defer windows.CertCloseStore(store, 0)

	// CERT_STORE_ADD_NEW compares the whole certificate, not the subject, so
	// during a rotation the old and new 3gIT certificates coexist. That is
	// intended: signatures made with the old one keep validating.
	err = windows.CertAddCertificateContextToStore(store, ctx, windows.CERT_STORE_ADD_NEW, nil)
	if err == nil {
		return true, nil
	}

	var errno syscall.Errno
	if errors.As(err, &errno) && uint32(errno) == cryptEExists {
		return false, nil
	}
	return false, fmt.Errorf("cannot add certificate to %s: %w", t, err)
}
```

- [ ] **Step 2: Verify it builds and nothing regressed**

```bash
go build ./... && go vet ./internal/cert/ && go test ./internal/cert/ -v
```

Expected: builds clean, vet silent, all Task 1 and 2 tests still PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/cert/store.go
git commit -m "feat(cert): install the certificate into Windows trust stores

Ensure() adds the certificate to every target store that does not hold it
yet, using CERT_STORE_ADD_NEW so that CRYPT_E_EXISTS is the atomic
already-present signal - no separate lookup, no check-then-add race.

A failure on one store does not skip the rest: machine stores are worth
writing even when the user's hive is unreachable."
```

---

## Task 4: Resolve the console user's SID

**Files:**
- Create: `internal/notify/console_user.go`

**Interfaces:**
- Consumes: `procActiveConsole`, `noConsoleSession`, `enablePrivilege` — all already unexported members of package `notify` (`notify.go:19`, `notify.go:32`, `toast_launch.go:110`)
- Produces: `notify.ConsoleUserSID() (string, bool)`

This lives in `notify`, not `cert`, because `notify` already owns every piece of WTS and token machinery it needs. The boundary that results is clean: WTS stays in `notify`, crypt32 stays in `cert`, and `service` composes them. Like Task 3 it is untestable in CI — it is entirely Windows API calls.

- [ ] **Step 1: Write the implementation**

Create `internal/notify/console_user.go`:

```go
package notify

import "golang.org/x/sys/windows"

// ConsoleUserSID returns the string SID of the user logged on at the active
// console session, and whether there is one at all.
//
// ok == false is a normal state, not an error: a machine sitting at the login
// screen has no console user. Callers should carry on with whatever part of
// their work is machine-wide.
//
// This exists because a LocalSystem service cannot reach the interactive
// user's per-profile state through its own token - that token resolves to
// SYSTEM's profile. Anything per-user (a certificate store, a registry hive)
// has to be addressed by the interactive user's SID, which is what this
// returns.
func ConsoleUserSID() (string, bool) {
	session, _, _ := procActiveConsole.Call()
	if uint32(session) == noConsoleSession {
		return "", false
	}

	// WTSQueryUserToken requires SeTcbPrivilege. LocalSystem holds it, but it
	// is not necessarily enabled in the service's own token - the same
	// best-effort enable LaunchToast does.
	_ = enablePrivilege("SeTcbPrivilege")

	var token windows.Token
	if err := windows.WTSQueryUserToken(uint32(session), &token); err != nil {
		return "", false
	}
	defer token.Close()

	user, err := token.GetTokenUser()
	if err != nil {
		return "", false
	}
	return user.User.Sid.String(), true
}
```

- [ ] **Step 2: Verify it builds**

```bash
go build ./... && go vet ./internal/notify/
```

Expected: clean.

- [ ] **Step 3: Commit**

```bash
git add internal/notify/console_user.go
git commit -m "feat(notify): expose the console user's SID

A LocalSystem service reaches its own profile through its own token, not
the interactive user's, so per-user work has to be addressed by SID.
Lives in notify because that package already owns the WTS and privilege
machinery this needs."
```

---

## Task 5: Configuration key and Event Log ids

**Files:**
- Modify: `internal/config/config.go` — struct field and `Load()`
- Modify: `internal/config/config.default.ini` — new `[certificate]` section
- Modify: `internal/config/config_test.go` — two tests
- Modify: `internal/logging/logging.go:87-95` — two constants

**Interfaces:**
- Consumes: nothing
- Produces: `config.Config.CertificateEnabled bool` (default `true`), `logging.EventCertInstalled` (700), `logging.EventCertFailed` (701)

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go`:

```go
func TestLoadCertificateDefaultsEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.ini")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !cfg.CertificateEnabled {
		t.Error("certificate install should default to enabled")
	}
}

func TestLoadCertificateDisabled(t *testing.T) {
	path := writeConfig(t, `
[source]
primary = internal
internalManifestURL = http://example.invalid/v2/updates/manifest

[certificate]
enabled = false
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.CertificateEnabled {
		t.Error("certificate.enabled = false was not honoured")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/config/ -run Certificate -v
```

Expected: build failure — `cfg.CertificateEnabled undefined`.

- [ ] **Step 3: Add the struct field**

In `internal/config/config.go`, after the `// [ipc]` block in the `Config` struct:

```go
	// [certificate]
	CertificateEnabled bool
```

- [ ] **Step 4: Read the section in `Load()`**

In `internal/config/config.go`, alongside the other section lookups:

```go
	certSec := f.Section("certificate")
```

and in the `cfg := &Config{...}` literal, after the IPC fields:

```go
		CertificateEnabled: certSec.Key("enabled").MustBool(true),
```

No entry in `validate()`: a bool has no invalid value.

- [ ] **Step 5: Add the section to the shipped defaults**

Append to `internal/config/config.default.ini` (Italian, matching the rest of the file):

```ini

[certificate]
; Installa il certificato di code-signing 3gIT negli store Root e
; TrustedPublisher, sia a livello di macchina sia per l'utente della
; sessione console attiva. Serve a far sì che EMLy e il suo setup
; risultino firmati da un editore verificato invece che da "Autore
; sconosciuto" nel prompt UAC.
; Il controllo viene rieseguito a ogni ciclo di polling ed è idempotente.
enabled = true
```

- [ ] **Step 6: Run the config tests to verify they pass**

```bash
go test ./internal/config/ -v
```

Expected: all PASS, including the pre-existing ones.

- [ ] **Step 7: Add the Event Log ids**

In `internal/logging/logging.go`, extend the block at lines 87-95:

```go
	EventCertInstalled  = 700 // code-signing certificate added to a trust store
	EventCertFailed     = 701 // a trust store could not be opened or written
```

- [ ] **Step 8: Verify the whole tree still builds and tests**

```bash
go build ./... && go test ./...
```

Expected: all PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/config/ internal/logging/logging.go
git commit -m "feat(config): add certificate.enabled and event ids 700/701

The key defaults to true and is read from a new [certificate] section.
Event 700 records a store actually written, 701 a store that could not be
opened - Warn rather than Error, since the step is best-effort."
```

---

## Task 6: Wire it into the service and the installer

**Files:**
- Modify: `internal/service/service.go` — new `ensureCertificate` method, called from `Cycle`
- Modify: `main.go` — new `installCertificate` function, called from `cmdInstall`

**Interfaces:**
- Consumes: `cert.Embedded`, `cert.MachineTargets`, `cert.UserTargets`, `cert.Ensure` (Tasks 1–3); `notify.ConsoleUserSID` (Task 4); `config.Config.CertificateEnabled`, `logging.EventCertInstalled`, `logging.EventCertFailed` (Task 5)
- Produces: nothing consumed by later tasks

- [ ] **Step 1: Add the import**

In `internal/service/service.go`, add to the `emlyupdater/internal/...` import group (they are alphabetical):

```go
	"emlyupdater/internal/cert"
```

- [ ] **Step 2: Write `ensureCertificate`**

Add to `internal/service/service.go`, next to `showUpdateToast` — both are best-effort side quests of the main loop:

```go
// ensureCertificate installs the 3gIT code-signing certificate into the
// machine trust stores and, when somebody is logged on at the console, into
// that user's stores as well.
//
// Best-effort by design, like the file-association self-heal: a certificate
// that cannot be installed costs a friendlier UAC prompt, never an update. No
// failure here is ever returned to the caller.
//
// This runs on every cycle rather than once at service start. A user who logs
// on after boot - the normal case, not the exception - would otherwise never
// be covered, and manual removal of the certificate would never heal. When it
// is already installed everywhere the cost is two syscalls per store and
// nothing above Debug reaches the log.
func (u *Updater) ensureCertificate() {
	if !u.Cfg.CertificateEnabled {
		return
	}

	c, der, err := cert.Embedded()
	if err != nil {
		u.Log.Warn("embedded code-signing certificate unusable, skipping install",
			"error", err.Error())
		return
	}

	targets := cert.MachineTargets()
	if sid, ok := notify.ConsoleUserSID(); ok {
		targets = append(targets, cert.UserTargets(sid)...)
	} else {
		// ConsoleUserSID collapses "nobody is logged on" and "the token could
		// not be queried" into the same false - the first is routine and the
		// second is rare, and neither changes what we do. Say both.
		u.Log.Debug("no console user available (nobody logged on, or the user token could not be queried), installing certificate for the machine only")
	}

	installed, err := cert.Ensure(der, targets, func(format string, args ...any) {
		u.Log.Debug(fmt.Sprintf(format, args...))
	})
	for _, store := range installed {
		u.Log.InfoEvent(logging.EventCertInstalled, "code-signing certificate installed",
			"store", store,
			"subject", c.Subject.CommonName,
			"notAfter", c.NotAfter.Format(time.RFC3339))
	}
	if err != nil {
		u.Log.WarnEvent(logging.EventCertFailed, "code-signing certificate install incomplete",
			"error", err.Error(), "storesWritten", len(installed), "storesTried", len(targets))
		return
	}
	if len(installed) == 0 {
		u.Log.Debug("code-signing certificate already present in all trust stores",
			"stores", len(targets))
	}
}
```

- [ ] **Step 3: Call it from `Cycle`**

In `internal/service/service.go`, make it the first thing `Cycle` does — before the pending-update check, so a resumed install already benefits from it:

```go
func (u *Updater) Cycle(ctx context.Context) error {
	// Trust-store self-heal, before anything that might run EMLy's setup.
	u.ensureCertificate()

	emly := u.Cfg.ResolveEMLy()
```

- [ ] **Step 4: Verify it builds**

```bash
go build ./... && go vet ./internal/service/
```

Expected: clean.

- [ ] **Step 5: Add `installCertificate` to main.go**

Add near `cmdInstall` in `main.go`, and add `"emlyupdater/internal/cert"` to the imports:

```go
// installCertificate puts the 3gIT code-signing certificate into the machine
// trust stores during `install`, so the very first EMLy setup this updater
// runs already elevates as a verified publisher instead of waiting for the
// first poll cycle.
//
// Best-effort and non-fatal: it reports what it did and never blocks service
// registration. Only the machine stores are touched here - `install` runs from
// an installer, where there may be no console user, and the service re-checks
// the per-user stores every cycle anyway.
func installCertificate() {
	cfg, err := config.Load(config.ConfigPath())
	if err != nil {
		fmt.Printf("note: certificate install skipped, config unreadable: %v\n", err)
		return
	}
	if !cfg.CertificateEnabled {
		return
	}

	_, der, err := cert.Embedded()
	if err != nil {
		fmt.Printf("note: certificate install skipped: %v\n", err)
		return
	}

	installed, err := cert.Ensure(der, cert.MachineTargets(), func(format string, args ...any) {
		fmt.Printf("  %s\n", fmt.Sprintf(format, args...))
	})
	if err != nil {
		fmt.Printf("note: certificate install incomplete: %v\n", err)
		return
	}
	if len(installed) == 0 {
		fmt.Println("code-signing certificate already present in machine trust stores")
	}
}
```

- [ ] **Step 6: Call it from `cmdInstall`**

In `main.go`, immediately after the `config.WriteDefault` block (around line 199) and before `os.Executable()` — the config has to exist first, and the service does not:

```go
	installCertificate()
```

- [ ] **Step 7: Verify the whole tree builds and tests**

```bash
go build ./... && go vet ./... && go test ./...
```

Expected: builds clean, vet silent, all tests PASS.

- [ ] **Step 8: Smoke-test in the foreground**

From an elevated shell, with the service stopped:

```powershell
go build -o build\bin\emly-updater.exe .
.\build\bin\emly-updater.exe run
```

Expected: the first cycle logs the certificate install (event 700) for `LocalMachine\Root` and `LocalMachine\TrustedPublisher`. Running as an administrator rather than SYSTEM, `ConsoleUserSID` should still resolve, but the per-user stores may behave differently than under the service — Task 8 is what actually verifies that path. Stop with Ctrl-C.

- [ ] **Step 9: Commit**

```bash
git add internal/service/service.go main.go
git commit -m "feat(service): install the code-signing certificate each cycle

Runs at the top of every poll cycle rather than once at startup, so a user
who logs on after boot is covered and manual removal heals itself. Already
installed is the common case and stays at Debug.

cmdInstall seeds the machine stores too, so the first setup this updater
runs already elevates as a verified publisher."
```

---

## Task 7: Documentation

**Files:**
- Modify: `AGENTS.md` — architecture tree, config table, event id list, manual verification, rotation pitfall
- Modify: `README.md` — config reference

**Interfaces:**
- Consumes: everything above
- Produces: nothing

- [ ] **Step 1: Add the package to the architecture tree**

In `AGENTS.md`, in the `internal/` tree, after the `assoc/` line:

```
  cert/                  Embedded 3gIT code-signing certificate + install into Root/TrustedPublisher (machine + console user)
```

- [ ] **Step 2: Add the config key to the reference table**

In `AGENTS.md`, at the end of the Configuration Reference table:

```
| `enabled` | `[certificate]` | `true` | Install the 3gIT code-signing certificate into `Root` + `TrustedPublisher` (machine + console user) |
```

- [ ] **Step 3: Add the event ids**

In `AGENTS.md`, in the Logs & Diagnostics table, extend the Event Log row's list with:

```
cert installed (700), cert install failed (701)
```

- [ ] **Step 4: Add the manual verification section**

In `AGENTS.md`, after the "IPC manual verification (admin required)" section:

```markdown
### Code-signing certificate manual verification (admin required)

Nothing in `internal/cert/store.go` or `internal/notify/console_user.go` is
covered by `go test` — they are pure Windows API calls, and CI has no admin
rights. This checklist is their verification.

1. On a clean machine, run `emly-updater install` and confirm in `certmgr.msc`
   (Local Computer) that `CN=3G IT Innovation` is in both **Trusted Root
   Certification Authorities** and **Trusted Publishers**.
2. Log on as a standard user, wait one poll cycle, and confirm in `certmgr.msc`
   (Current User) that it is in the same two stores for that user.
3. Run the EMLy setup and confirm the UAC prompt now reads
   *"Verified publisher: 3G IT Innovation"*.
4. Delete the certificate from `LocalMachine\Root`, wait one cycle, and confirm
   it is restored and that event 700 is logged.
5. With nobody logged on at the console, confirm the machine stores are still
   maintained and only a Debug line notes the skipped per-user half.
6. Confirm that a second cycle after a successful install logs nothing at Info
   — i.e. the `CRYPT_E_EXISTS` path really is Debug-level and 96 cycles a day
   do not flood the log.
7. Set `enabled = false` under `[certificate]`, restart the service, and
   confirm nothing is written and nothing is logged.
```

- [ ] **Step 5: Add the rotation pitfall**

In `AGENTS.md`, in Common Pitfalls:

```markdown
- **Rotating the code-signing certificate**: replace **both**
  `certs/3GITInnovation.cer` (the source of record) and
  `internal/cert/3GITInnovation.cer` (the embedded copy — `//go:embed` cannot
  reach above its own package), update `wantThumbprint` in
  `internal/cert/cert_test.go` and §4 of the design doc, then cut a release.
  The old certificate stays installed on existing machines and keeps
  validating signatures made with it. `internal/cert/cert_test.go` fails 60
  days before expiry, so this should never be a surprise. Two standing
  recommendations for whoever issues the next one: give it a 10-year validity
  and a SHA-256 signature (it is self-signed — the lifetime is a free choice,
  and an annual one creates yearly release pressure for nothing), and
  timestamp the signatures themselves (`signtool /tr <rfc3161-url> /td
  sha256`) so they survive the certificate's expiry.
```

- [ ] **Step 6: Update README.md**

The README's Configuration section uses one `### \`[section]\`` heading per INI
section, each followed by a `| Key | Default | Description |` table. Add a new
one after `### \`[criticalUpdate]\``:

```markdown
### `[certificate]`

| Key | Default | Description |
|---|---|---|
| `enabled` | `true` | Install the 3gIT code-signing certificate into `Root` + `TrustedPublisher`, for the machine and for the console user. Makes EMLy's setup elevate as a verified publisher instead of "Unknown publisher". Re-checked every cycle; idempotent. |
```

- [ ] **Step 7: Commit**

```bash
git add AGENTS.md README.md
git commit -m "docs: document the code-signing certificate install

Architecture tree, config reference, event ids 700/701, a manual
verification checklist for the crypt32 paths CI cannot reach, and the
rotation pitfall - which has to name both copies of the .cer, since
//go:embed cannot reach above its own package."
```

---

## Task 8: Manual verification on a real machine

**Files:** none

- [ ] **Step 1: Build and install**

```powershell
go generate
go build -ldflags "-s -w" -o build\bin\emly-updater.exe .
```

Install on a test machine per AGENTS.md's Manual deployment section.

- [ ] **Step 2: Work through the checklist**

Run all seven items from the section added in Task 7 Step 4. The one that matters most is **item 2** — the per-user store — because it is the only production use of the `CERT_SYSTEM_STORE_USERS` path that Task 0 probed in isolation.

- [ ] **Step 3: Record the result**

If everything passes, note the Windows build it was verified on in the PR description. If item 2 fails despite Task 0 having passed, the difference is the production context — capture the event 701 payload before changing anything, then reconsider the `CreateProcessAsUser` fallback from spec §6.4.

---

## Task 9: Release preparation

Only once Task 8 passes.

- [ ] **Step 1: Bump the version**

Edit `versioninfo.json`: bump `StringFileInfo.FileVersion` and `ProductVersion`. It is the single source of truth for the version string.

- [ ] **Step 2: Propagate it**

```bash
go generate ./...
```

This regenerates `internal/version/version_generated.go` and rewrites the version tokens in `installer/installer.iss` and `internal/config/config.default.ini`. Never edit those by hand.

- [ ] **Step 3: Bump the compatibility ceiling**

In `internal/ipc/version.go`, set `MaxCompatibleEMLyVersion` to the release being shipped. Do this even though nothing here touches IPC — otherwise the compatibility matrix and the forward-compat log go stale.

Leave `MinCompatibleEMLyVersion` alone: this release requires nothing new of EMLy.

- [ ] **Step 4: Verify**

```bash
go build ./... && go test ./... && go vet ./...
```

- [ ] **Step 5: Commit and open the PR**

```bash
git add -A
git commit -m "chore: bump version for code-signing certificate install"
git push -u origin feat/codesign-cert-install
```

`proto/updateripc.proto` is untouched, so no synchronisation with the `emly` repository is required for this release.

---

## Notes for the reviewer

Three things worth a second look:

**The `CERT_SYSTEM_STORE_UNPROTECTED_FLAG` in `target.go`.** This was not in the original design and was added while writing this plan. The per-user `Root` store is a protected root whose ordinary add path is meant to raise an interactive confirmation dialog; session 0 has no desktop for one. Without the flag, the per-user half plausibly fails or hangs. Task 0 tests the flagged path specifically.

**`Ensure` returns both results and an error.** Deliberate. Partial success is a real and useful outcome: machine stores written while the user's hive is unreachable is better than all-or-nothing, and the caller logs both halves.

**Nothing in Tasks 3, 4, or 6 has automated coverage.** That is forced by the constraint that CI runs without admin rights, not chosen. It is why those files are kept as small and as obvious as they are, and why Task 8 is a task rather than a footnote.
