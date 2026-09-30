# Agent multi-prodotto — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Un unico motore di aggiornamento che serve EMLy (migrato senza cambi di comportamento) e i prodotti definiti nella sezione `products` del documento di remote config, a partire da 3g-RocketChat (setup NSIS).

**Architecture:** Nuovo pacchetto `internal/product` (definizione + rilevamento versione a catena `ini`/`file`/`exe`), `internal/installer` a driver (`inno`, `nsis`), `state.Store` con pending per slug. Il ciclo in `internal/service` diventa un giro sequenziale sui prodotti abilitati con EMLy per ultimo; le specificità di EMLy restano dietro `Product.Legacy`. La sezione `products` entra in `internal/policy` con fixture condivise con `emly-go-api`.

**Tech Stack:** Go 1.26.1, Windows-only (`golang.org/x/sys/windows` v0.46.0), `gopkg.in/ini.v1`, `github.com/hashicorp/go-version`.

**Spec:** `docs/superpowers/specs/2026-09-30-multi-product-agent-design.md`

## Global Constraints

- Build/test solo su Windows: `go build ./...`, `go test ./...`, `go vet ./...` (il warning pre-esistente `internal\machineinfo\sessionchange.go:84: possible misuse of unsafe.Pointer` non va toccato).
- Fase 1 (Task 1–5): **nessun cambio di comportamento per EMLy**. I test esistenti di `internal/service` non si modificano; le firme `install(ctx, cyc, *state.Pending, config.EMLyInfo)`, `resolveTarget(ctx, cyc, channel)`, `newResolver(cyc)`, `newProgressUI(self, target)`, `Store.SetPending(p)`, `Store.ClearPending()` restano.
- Slug: `^[a-z0-9][a-z0-9-]{0,19}$`; riservati `updater`, `all`, `manifest`, `releases`, `download`, `products`; `emly` vietato in `products`.
- EMLy: manifest su `/v2/updates/manifest`; altri: `/v2/updates/{slug}/manifest`.
- `state.json`: EMLy resta nel campo `pending`; gli altri in `products` (mappa per slug).
- Un solo `download.Pacer` condiviso da tutti i manager.
- Testi utente per i prodotti generici: solo italiano.
- NSIS: `/D=` e `_?=` sempre **ultimi** e **senza virgolette** → riga di comando costruita a mano (`SysProcAttr.CmdLine`), mai `exec.Command(path, args...)` con quei parametri.
- Tetto 3 tentativi per versione, solo prodotti generici. `cleanReinstall` default `false`, EMLy sempre `true`.
- Prodotto non disponibile (404 / release vuota): 6 ore, in memoria.
- Commit: messaggi convenzionali in inglese, **nessun trailer `Co-Authored-By`** (preferenza dell'utente).
- Quando si cambia un comportamento descritto in AGENTS.md, AGENTS.md si aggiorna nello stesso cambio (CLAUDE.md).

## Review Focus

1. `version.txt` scritto da un setup Windows con BOM UTF-8, CRLF e newline finale → deve dare la versione pulita (test in Task 6).
2. `installDir` con spazi (`C:\Program Files\3gIT\X`) → `/D=` e `_?=` senza virgolette, altrimenti NSIS installa in una directory sbagliata (test in Task 7).
3. Rollback dell'Agent a 1.7.x con `state.json` che contiene `products` → il vecchio binario deve ancora leggere il `pending` di EMLy (test in Task 2).
4. Utente che tiene RocketChat sempre aperto, update non forzato → una sola notifica per versione, non una a ciclo; EMLy nello stesso ciclo aggiornato comunque (test in Task 10).
5. Manifest che offre una versione **inferiore** a quella installata → nessun install, nessun downgrade (test in Task 10).

---

## File Structure

| File | Responsabilità |
|---|---|
| `internal/product/product.go` (nuovo) | `Product`, `VersionSource`, `InstallerSpec`, costanti, `ManifestPath()` |
| `internal/product/detect.go` (nuovo) | `Detect`, catena, lettori `ini`/`file` |
| `internal/product/exe_windows.go` (nuovo) | lettore `exe` (VERSIONINFO) |
| `internal/product/*_test.go` (nuovi) | test della catena e dei lettori |
| `internal/state/state.go` | `Products`, `Attempts`/`GaveUp`, `PendingFor`/`SetPendingFor`/`ClearPendingFor` |
| `internal/installer/installer.go` | `Driver`, `Spec`, `For`, `runSilent(*exec.Cmd, …)`, `VerifyInstalled` (invariata) |
| `internal/installer/inno.go` (nuovo) | driver Inno Setup |
| `internal/installer/nsis.go` (nuovo) | driver NSIS |
| `internal/installer/acl_windows.go` (nuovo) | `CheckNotUserWritable` |
| `internal/notify/notify.go`, `toast_launch.go` | messaggi per prodotto, fix `SendNotifyBox` |
| `internal/manifest/manifest.go` | `ErrNoRelease` |
| `internal/source/resolver.go` | `ErrNoRelease` come risposta definitiva |
| `internal/policy/document.go`, `parse.go`, `products.go` (nuovo) | sezione `products`, validazione |
| `testdata/remoteconfig/**` | fixture `products` |
| `internal/service/product.go` (nuovo) | motore per prodotto: `productCycle`, `applyProduct`, `installProduct`, … |
| `internal/service/products.go` (nuovo) | giro prodotti, conversione da policy, non-disponibilità, tentativi |
| `internal/service/report.go` (nuovo) | sottocomando `products` |
| `internal/service/service.go` | `Cycle` delega al motore; seam; wrapper EMLy; inventario |
| `internal/service/progress.go` | testo per prodotto |
| `main.go` | sottocomando `products` |
| `AGENTS.md`, `README.md`, `docs/remote-config.example.json`, `versioninfo.json` | documentazione, versione 1.8.0 |

---

## Task 0: Committare l'inventario già implementato

Il working tree contiene l'implementazione di `X-EMLy-InstalledProducts` / `installed_products` (sessione precedente). Va committata prima di iniziare, così ogni task successivo ha un diff pulito.

**Files:** già modificati: `AGENTS.md`, `internal/config/emly.go`, `internal/service/{clientws.go,clientws_test.go,service.go,service_test.go}`, `internal/source/{http.go,headers_test.go}`, `internal/wsclient/client.go`

- [ ] **Step 1: Verificare che la suite passi**

Run: `go test ./...`
Expected: tutti `ok`.

- [ ] **Step 2: Commit**

```bash
git add AGENTS.md internal/config/emly.go internal/service/clientws.go internal/service/clientws_test.go internal/service/service.go internal/service/service_test.go internal/source/http.go internal/source/headers_test.go internal/wsclient/client.go
git commit -m "feat: report installed products inventory over HTTP and WS identity"
```

---

# Fase 1 — motore con il solo EMLy

## Task 1: Pacchetto `product` (tipi, catena, sorgente `ini`)

**Files:**
- Create: `internal/product/product.go`
- Create: `internal/product/detect.go`
- Test: `internal/product/detect_test.go`

**Interfaces:**
- Produces:
  - `const EMLySlug = "emly"`
  - `const SourceINI, SourceFile, SourceExe = "ini", "file", "exe"`
  - `const InstallerInno, InstallerNSIS = "inno", "nsis"`
  - `type VersionSource struct { Type, Path, Section, Key string }`
  - `type InstallerSpec struct { Type string; CleanReinstall bool }`
  - `type Product struct { Slug, Name, InstallDir, ExeName, Channel string; Detect []VersionSource; Installer InstallerSpec; InstallWhenAbsent, Legacy bool }`
  - `func (p *Product) ManifestPath() string`
  - `type Outcome int` con `Absent`, `Installed`, `Unknown`
  - `type Result struct { Outcome Outcome; Version, Source string; Err error }`
  - `func Detect(p *Product) Result`
  - `func (r Result) String() string`

- [ ] **Step 1: Scrivere i test che falliscono**

`internal/product/detect_test.go`:

```go
package product

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func iniSource(path string) VersionSource {
	return VersionSource{Type: SourceINI, Path: path, Section: "app", Key: "version"}
}

func TestDetectINIInstalled(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "config.ini", "[app]\nversion = 1.2.0   ; managed\n")
	r := Detect(&Product{InstallDir: dir, Detect: []VersionSource{iniSource("config.ini")}})
	if r.Outcome != Installed || r.Version != "1.2.0" || r.Source != "ini:config.ini" {
		t.Fatalf("got %+v, want installed 1.2.0 from ini:config.ini", r)
	}
}

// Every source's file missing means the product is not on this machine.
func TestDetectAllMissingIsAbsent(t *testing.T) {
	r := Detect(&Product{InstallDir: t.TempDir(), Detect: []VersionSource{iniSource("config.ini")}})
	if r.Outcome != Absent {
		t.Fatalf("got %+v, want absent", r)
	}
}

// A missing file falls through to the next source.
func TestDetectFallsThroughMissingFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "second.ini", "[app]\nversion=2.0.0\n")
	r := Detect(&Product{InstallDir: dir, Detect: []VersionSource{iniSource("first.ini"), iniSource("second.ini")}})
	if r.Outcome != Installed || r.Version != "2.0.0" {
		t.Fatalf("got %+v, want installed 2.0.0", r)
	}
}

// A source whose file exists but is broken, with nothing after it answering,
// is unknown - never absent: absent would drop the product from the
// inventory and could trigger a fresh install over a working copy.
func TestDetectBrokenSourceIsUnknown(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "config.ini", "[app]\nlanguage = it\n")
	r := Detect(&Product{InstallDir: dir, Detect: []VersionSource{iniSource("config.ini"), iniSource("missing.ini")}})
	if r.Outcome != Unknown || r.Err == nil {
		t.Fatalf("got %+v, want unknown with an error", r)
	}
}

// A broken source followed by a working one: the working one wins.
func TestDetectBrokenThenInstalledIsInstalled(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "broken.ini", "[app]\n")
	write(t, dir, "good.ini", "[app]\nversion=3.1.0\n")
	r := Detect(&Product{InstallDir: dir, Detect: []VersionSource{iniSource("broken.ini"), iniSource("good.ini")}})
	if r.Outcome != Installed || r.Version != "3.1.0" {
		t.Fatalf("got %+v, want installed 3.1.0", r)
	}
}

func TestDetectUnparsableVersionIsUnknown(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "config.ini", "[app]\nversion = latest\n")
	if r := Detect(&Product{InstallDir: dir, Detect: []VersionSource{iniSource("config.ini")}}); r.Outcome != Unknown {
		t.Fatalf("got %+v, want unknown", r)
	}
}

// EMLy's config.ini is configured as an absolute path that need not live
// under its install directory: an absolute Path is used as-is.
func TestDetectAbsolutePathIsUsedAsIs(t *testing.T) {
	path := write(t, t.TempDir(), "config.ini", "[EMLy]\nGUI_SEMVER=2.2.5\n")
	p := &Product{InstallDir: t.TempDir(), Detect: []VersionSource{{Type: SourceINI, Path: path, Section: "EMLy", Key: "GUI_SEMVER"}}}
	if r := Detect(p); r.Outcome != Installed || r.Version != "2.2.5" {
		t.Fatalf("got %+v, want installed 2.2.5", r)
	}
}

func TestManifestPath(t *testing.T) {
	if got := (&Product{Slug: EMLySlug}).ManifestPath(); got != "/v2/updates/manifest" {
		t.Errorf("emly manifest path = %q", got)
	}
	if got := (&Product{Slug: "3g-rocketchat"}).ManifestPath(); got != "/v2/updates/3g-rocketchat/manifest" {
		t.Errorf("product manifest path = %q", got)
	}
}

func TestResultString(t *testing.T) {
	r := Result{Outcome: Installed, Version: "1.0.0", Source: "file:version.txt"}
	if !strings.Contains(r.String(), "1.0.0") || !strings.Contains(r.String(), "file:version.txt") {
		t.Errorf("String() = %q", r.String())
	}
}
```

- [ ] **Step 2: Eseguire i test e verificare che falliscano**

Run: `go test ./internal/product/`
Expected: FAIL, `undefined: Detect` (il pacchetto non esiste).

- [ ] **Step 3: Implementare `product.go`**

```go
// Package product describes what the agent updates: EMLy, which is built in,
// and every product the remote-configuration document lists under
// "products". It knows how to tell which version of a product is installed;
// it does no network I/O and runs nothing.
package product

// EMLySlug is EMLy's product slug, the one the API uses in the inventory and
// in /v2/updates/{slug}/.... EMLy itself keeps the historical, slug-less
// manifest route (see ManifestPath).
const EMLySlug = "emly"

// Version source types. Closed on purpose: the document picks among readers
// implemented here, it cannot name an arbitrary command.
const (
	SourceINI  = "ini"
	SourceFile = "file"
	SourceExe  = "exe"
)

// Installer driver types, see internal/installer.
const (
	InstallerInno = "inno"
	InstallerNSIS = "nsis"
)

// VersionSource is one link of a product's detection chain. Path is relative
// to Product.InstallDir; an absolute Path is used as-is (only the built-in
// EMLy definition uses one - the document's validator forbids it).
type VersionSource struct {
	Type    string
	Path    string
	Section string // ini only
	Key     string // ini only
}

// InstallerSpec selects the setup driver.
type InstallerSpec struct {
	Type string
	// CleanReinstall runs the product's uninstaller before the retry that
	// follows a failed install. Off by default for products: an NSIS
	// uninstaller typically removes the whole install directory, user data
	// and IT-managed configuration included.
	CleanReinstall bool
}

// Product is one updatable product.
type Product struct {
	Slug       string
	Name       string // user-facing text and logs
	InstallDir string
	ExeName    string // process waited for / closed before an install
	Channel    string // "stable" | "beta"
	Detect     []VersionSource
	Installer  InstallerSpec
	// InstallWhenAbsent installs the product on a machine that does not have
	// it. EMLy: always (its historical fresh-install mode). Others: off
	// unless the document turns it on.
	InstallWhenAbsent bool
	// Legacy marks the built-in EMLy definition, which keeps behaviour no
	// other product has: file associations, the localized critical warning,
	// the blocking wait for the app to close, update.* events, unlimited
	// retries. See the spec, section 3.4.
	Legacy bool
}

// ManifestPath is the manifest route for this product, relative to a
// server's base URL.
func (p *Product) ManifestPath() string {
	if p.Slug == EMLySlug {
		return "/v2/updates/manifest"
	}
	return "/v2/updates/" + p.Slug + "/manifest"
}
```

- [ ] **Step 4: Implementare `detect.go`**

```go
package product

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	version "github.com/hashicorp/go-version"
	"gopkg.in/ini.v1"
)

// Outcome is what detection concluded about a product on this machine.
type Outcome int

const (
	// Absent: no source of the chain exists. The product is not installed.
	Absent Outcome = iota
	// Installed: a source answered with a parsable version.
	Installed
	// Unknown: a source exists but could not be read, and no later source
	// answered. Never acted on: not installed over, not reported as absent.
	Unknown
)

// Result is Detect's answer.
type Result struct {
	Outcome Outcome
	Version string // Installed only
	Source  string // "type:path" of the source that decided, "" for Absent
	Err     error  // Unknown only
}

func (r Result) String() string {
	switch r.Outcome {
	case Installed:
		return fmt.Sprintf("installed %s (%s)", r.Version, r.Source)
	case Unknown:
		return fmt.Sprintf("unknown (%s: %v)", r.Source, r.Err)
	default:
		return "absent"
	}
}

// Detect walks p's chain in order. A source whose file does not exist passes
// to the next; the first source that yields a version wins. A source that
// exists but is broken is remembered: if nothing after it answers, the
// outcome is Unknown rather than Absent.
func Detect(p *Product) Result {
	var broken *Result
	for _, src := range p.Detect {
		label := src.Type + ":" + src.Path
		v, err := readSource(src, p.resolve(src.Path))
		switch {
		case err == nil:
			return Result{Outcome: Installed, Version: v, Source: label}
		case errors.Is(err, fs.ErrNotExist):
			continue
		case broken == nil:
			broken = &Result{Outcome: Unknown, Source: label, Err: err}
		}
	}
	if broken != nil {
		return *broken
	}
	return Result{Outcome: Absent}
}

func (p *Product) resolve(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(p.InstallDir, path)
}

func readSource(src VersionSource, path string) (string, error) {
	switch src.Type {
	case SourceINI:
		return readINI(path, src.Section, src.Key)
	}
	return "", fmt.Errorf("unknown version source type %q", src.Type)
}

// readINI returns section/key of the INI file at path. A missing file comes
// back wrapping fs.ErrNotExist, which is what lets the chain fall through.
func readINI(path, section, key string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	f, err := ini.Load(path)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	v := strings.TrimSpace(f.Section(section).Key(key).String())
	if v == "" {
		return "", fmt.Errorf("%s: [%s] %s missing", path, section, key)
	}
	return checkVersion(v)
}

func checkVersion(v string) (string, error) {
	if _, err := version.NewVersion(v); err != nil {
		return "", fmt.Errorf("version %q is not parsable: %w", v, err)
	}
	return v, nil
}
```

- [ ] **Step 5: Eseguire i test**

Run: `go test ./internal/product/ -v`
Expected: PASS su tutti.

- [ ] **Step 6: Commit**

```bash
git add internal/product
git commit -m "feat(product): add product definition and version detection chain"
```

---

## Task 2: `state` — pending per slug

**Files:**
- Modify: `internal/state/state.go`
- Test: `internal/state/state_test.go`

**Interfaces:**
- Consumes: niente.
- Produces:
  - `const LegacySlug = "emly"` (deve coincidere con `product.EMLySlug`; `state` resta un pacchetto foglia)
  - `State.Products map[string]*Pending` (`json:"products,omitempty"`)
  - `Pending.Attempts int` (`json:"attempts,omitempty"`), `Pending.GaveUp bool` (`json:"gaveUp,omitempty"`)
  - `func (s *Store) PendingFor(slug string) (*Pending, error)`
  - `func (s *Store) SetPendingFor(slug string, p *Pending) error`
  - `func (s *Store) ClearPendingFor(slug string) error`
  - `SetPending`/`ClearPending` restano, equivalenti a `…For(LegacySlug, …)`

- [ ] **Step 1: Scrivere i test che falliscono** (in coda a `state_test.go`)

```go
// EMLy's pending entry stays in the historical "pending" field and every
// other product goes under "products": an agent rolled back to a build that
// predates products still finds (and resumes) EMLy's queued install.
func TestPendingForKeepsEMLyInTheLegacyField(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "state.json")}
	emly := &Pending{Version: "2.3.0", SetupPath: `C:\x\EMLy-2.3.0-setup.exe`, SHA256: "aa"}
	rc := &Pending{Version: "1.1.0", SetupPath: `C:\x\3g-rocketchat-1.1.0-setup.exe`, SHA256: "bb", Attempts: 1}
	if err := s.SetPendingFor(LegacySlug, emly); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPendingFor("3g-rocketchat", rc); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	// What a pre-products build decodes: only the fields it knows.
	var old struct {
		Pending *struct {
			Version string `json:"version"`
		} `json:"pending"`
	}
	if err := json.Unmarshal(data, &old); err != nil {
		t.Fatalf("old build cannot decode state.json: %v", err)
	}
	if old.Pending == nil || old.Pending.Version != "2.3.0" {
		t.Fatalf("old build sees pending = %+v, want EMLy 2.3.0", old.Pending)
	}
	if strings.Contains(string(data), `"attempts": 0`) || strings.Contains(string(data), `"gaveUp": false`) {
		t.Errorf("zero attempts/gaveUp must be omitted, got:\n%s", data)
	}

	got, err := s.PendingFor("3g-rocketchat")
	if err != nil || got == nil || got.Version != "1.1.0" || got.Attempts != 1 {
		t.Fatalf("PendingFor(3g-rocketchat) = %+v, %v", got, err)
	}
	if got, _ := s.PendingFor(LegacySlug); got == nil || got.Version != "2.3.0" {
		t.Fatalf("PendingFor(emly) = %+v", got)
	}
}

func TestClearPendingForTouchesOnlyThatSlug(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "state.json")}
	_ = s.SetPending(&Pending{Version: "2.3.0"})
	_ = s.SetPendingFor("3g-rocketchat", &Pending{Version: "1.1.0"})

	if err := s.ClearPendingFor("3g-rocketchat"); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Load()
	if st.Pending == nil || st.Products != nil {
		t.Fatalf("after clearing the product: pending=%+v products=%+v", st.Pending, st.Products)
	}
	if err := s.ClearPendingFor(LegacySlug); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Load(); st.Pending != nil {
		t.Fatalf("EMLy pending not cleared: %+v", st.Pending)
	}
}

func TestPendingForUnknownSlugIsNil(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "state.json")}
	if p, err := s.PendingFor("nothing"); p != nil || err != nil {
		t.Fatalf("PendingFor on empty state = %+v, %v", p, err)
	}
}
```

Aggiungere agli import di `state_test.go` quelli mancanti tra `encoding/json`, `os`, `path/filepath`, `strings`.

- [ ] **Step 2: Eseguire i test e verificare che falliscano**

Run: `go test ./internal/state/ -run "PendingFor|ClearPendingFor"`
Expected: FAIL, `undefined: LegacySlug` / `s.SetPendingFor undefined`.

- [ ] **Step 3: Implementare**

In `state.go`, dopo `Pending.DownloadedAt` aggiungere:

```go
	// Attempts and GaveUp cap how often a product other than EMLy retries a
	// failing target version (spec §6.2). Always zero for EMLy, whose entry
	// is therefore byte-identical to the pre-products format.
	Attempts int  `json:"attempts,omitempty"`
	GaveUp   bool `json:"gaveUp,omitempty"`
```

Sopra `type State`:

```go
// LegacySlug is EMLy's product slug. Its pending entry lives in State.Pending,
// the field every build before multi-product reads; must equal
// product.EMLySlug (this package stays a leaf and does not import it).
const LegacySlug = "emly"
```

In `State`, dopo `Pending`:

```go
	// Products holds the pending entry of every product other than EMLy,
	// keyed by slug. A build that predates it ignores the field.
	Products map[string]*Pending `json:"products,omitempty"`
```

Sostituire `SetPending`/`ClearPending` con:

```go
// SetPending persists p as EMLy's pending update.
func (s *Store) SetPending(p *Pending) error { return s.SetPendingFor(LegacySlug, p) }

// ClearPending removes EMLy's pending update.
func (s *Store) ClearPending() error { return s.ClearPendingFor(LegacySlug) }

// PendingFor returns slug's pending update, nil when there is none.
func (s *Store) PendingFor(slug string) (*Pending, error) {
	st, err := s.Load()
	if err != nil {
		return nil, err
	}
	if slug == LegacySlug {
		return st.Pending, nil
	}
	return st.Products[slug], nil
}

// SetPendingFor persists p as slug's pending update.
func (s *Store) SetPendingFor(slug string, p *Pending) error {
	return s.update(func(st *State) {
		if slug == LegacySlug {
			st.Pending = p
			return
		}
		if st.Products == nil {
			st.Products = map[string]*Pending{}
		}
		st.Products[slug] = p
	})
}

// ClearPendingFor removes slug's pending update.
func (s *Store) ClearPendingFor(slug string) error {
	return s.update(func(st *State) {
		if slug == LegacySlug {
			st.Pending = nil
			return
		}
		delete(st.Products, slug)
		if len(st.Products) == 0 {
			st.Products = nil
		}
	})
}
```

Aggiornare il doc comment di `Store` e di `update`: "three independent lifecycles" diventa "EMLy's and every product's queued update, the updater's own self-update record, and the client channel's pending destructive commands".

- [ ] **Step 4: Eseguire i test**

Run: `go test ./internal/state/ ./internal/service/`
Expected: PASS (i test esistenti di `service` usano `SetPending`/`ClearPending`, invariati).

- [ ] **Step 5: Commit**

```bash
git add internal/state
git commit -m "feat(state): per-product pending entries, EMLy kept in the legacy field"
```

---

## Task 3: `installer` a driver (solo Inno)

**Files:**
- Modify: `internal/installer/installer.go`
- Create: `internal/installer/inno.go`
- Test: `internal/installer/inno_test.go`

**Interfaces:**
- Consumes: niente.
- Produces:
  - `type Driver interface { Install(setupPath, version string) error; Uninstall() error }`
  - `type Spec struct { Slug, Type, InstallDir, LogsDir string; ForceUpgrade bool }`
  - `func For(s Spec) Driver` (tipo sconosciuto → Inno; la validazione della policy impedisce altri valori)
  - `func innoInstallArgs(logPath string, force bool) []string`, `func innoUninstallArgs(logPath string) []string`
  - `func runSilent(cmd *exec.Cmd, logPath string) error`
  - `VerifyInstalled` invariata.
  - `Run` e `Uninstall` (funzioni libere) **rimosse**: unico chiamante è `internal/service`, adattato nel Task 5.

- [ ] **Step 1: Scrivere i test che falliscono**

`internal/installer/inno_test.go`:

```go
package installer

import (
	"slices"
	"testing"
)

// The arguments EMLy's setup has always been run with: /FORCEUPGRADE is
// EMLy-specific (its installer asks Yes/No on upgrade even under /VERYSILENT
// without it), so only the legacy definition passes it.
func TestInnoInstallArgs(t *testing.T) {
	want := []string{"/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/FORCEUPGRADE", `/LOG=C:\logs\emly-install-2.3.0.log`}
	if got := innoInstallArgs(`C:\logs\emly-install-2.3.0.log`, true); !slices.Equal(got, want) {
		t.Errorf("EMLy args = %q, want %q", got, want)
	}
	want = []string{"/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", `/LOG=C:\logs\x-install-1.0.0.log`}
	if got := innoInstallArgs(`C:\logs\x-install-1.0.0.log`, false); !slices.Equal(got, want) {
		t.Errorf("generic args = %q, want %q", got, want)
	}
}

func TestInnoUninstallArgs(t *testing.T) {
	want := []string{"/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", `/LOG=C:\logs\u.log`}
	if got := innoUninstallArgs(`C:\logs\u.log`); !slices.Equal(got, want) {
		t.Errorf("args = %q, want %q", got, want)
	}
}

// EMLy's log names do not change: "emly" is its slug.
func TestInnoLogNamesFollowTheSlug(t *testing.T) {
	d := For(Spec{Slug: "emly", Type: "inno", LogsDir: `C:\logs`}).(inno)
	if got := d.installLog("2.3.0"); got != `C:\logs\emly-install-2.3.0.log` {
		t.Errorf("install log = %q", got)
	}
}

// An install directory with no uninstaller is not an error: there is nothing
// to clean up.
func TestInnoUninstallWithoutUninstallerIsNoop(t *testing.T) {
	if err := For(Spec{Slug: "emly", Type: "inno", InstallDir: t.TempDir(), LogsDir: t.TempDir()}).Uninstall(); err != nil {
		t.Fatalf("Uninstall = %v, want nil", err)
	}
}
```

- [ ] **Step 2: Eseguire i test e verificare che falliscano**

Run: `go test ./internal/installer/`
Expected: FAIL, `undefined: innoInstallArgs`.

- [ ] **Step 3: Implementare**

In `installer.go`: aggiornare il commento del pacchetto ("runs product setups silently through a driver per installer technology, and confirms EMLy's result through its config.ini"); sostituire `runSilent`, `Run`, `Uninstall` con:

```go
// Driver runs one product's setup and uninstaller silently.
type Driver interface {
	// Install runs the setup at setupPath and waits for it.
	Install(setupPath, version string) error
	// Uninstall runs the product's own uninstaller, if one is present in its
	// install directory, and waits for it. No uninstaller is not an error.
	Uninstall() error
}

// Spec selects and configures a driver.
type Spec struct {
	Slug       string // names the log files
	Type       string // "inno" | "nsis"
	InstallDir string
	LogsDir    string
	// ForceUpgrade adds EMLy's /FORCEUPGRADE (Inno only).
	ForceUpgrade bool
}

// For returns the driver for s.Type. The remote-configuration validator only
// lets "inno" and "nsis" through; anything else falls back to Inno, which is
// what EMLy - the only definition not coming from the document - uses.
func For(s Spec) Driver {
	switch s.Type {
	default:
		return inno{s}
	}
}

// runSilent starts cmd hidden, waits for it to exit, and translates the
// result into an error. logPath names where the setup wrote its own log, for
// the error message; "" when it writes none.
func runSilent(cmd *exec.Cmd, logPath string) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	name := filepath.Base(cmd.Path)
	see := ""
	if logPath != "" {
		see = " (see " + logPath + ")"
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start %s: %w", cmd.Path, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				return fmt.Errorf("%s exited with code %d%s", name, exitErr.ExitCode(), see)
			}
			return fmt.Errorf("%s failed: %w", name, err)
		}
		return nil
	case <-time.After(installTimeout):
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("%s did not finish within %s, killed%s", name, installTimeout, see)
	}
}
```

`internal/installer/inno.go`:

```go
package installer

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"time"
)

// inno drives an Inno Setup setup and its unins*.exe.
type inno struct{ s Spec }

func (d inno) installLog(version string) string {
	return filepath.Join(d.s.LogsDir, fmt.Sprintf("%s-install-%s.log", d.s.Slug, version))
}

// Install runs the setup with /VERYSILENT as SYSTEM. /LOG writes Inno's own
// log next to the agent's for post-mortems.
func (d inno) Install(setupPath, version string) error {
	logPath := d.installLog(version)
	return runSilent(exec.Command(setupPath, innoInstallArgs(logPath, d.s.ForceUpgrade)...), logPath)
}

// Uninstall runs unins*.exe from the install directory, if there is one.
func (d inno) Uninstall() error {
	matches, err := filepath.Glob(filepath.Join(d.s.InstallDir, "unins*.exe"))
	if err != nil {
		return fmt.Errorf("failed to look for %s's uninstaller: %w", d.s.Slug, err)
	}
	if len(matches) == 0 {
		return nil
	}
	logPath := filepath.Join(d.s.LogsDir, fmt.Sprintf("%s-uninstall-%d.log", d.s.Slug, time.Now().Unix()))
	return runSilent(exec.Command(matches[0], innoUninstallArgs(logPath)...), logPath)
}

// innoInstallArgs: /FORCEUPGRADE is EMLy-specific - without it EMLy's
// InitializeSetup shows a Yes/No upgrade dialog even under /VERYSILENT (see
// installer.iss in the emly repo).
func innoInstallArgs(logPath string, force bool) []string {
	args := []string{"/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART"}
	if force {
		args = append(args, "/FORCEUPGRADE")
	}
	return append(args, "/LOG="+logPath)
}

func innoUninstallArgs(logPath string) []string {
	return []string{"/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/LOG=" + logPath}
}
```

Rimuovere da `installer.go` gli import non più usati.

- [ ] **Step 4: Eseguire i test del pacchetto**

Run: `go test ./internal/installer/ -v`
Expected: PASS. (`go build ./...` fallisce ancora in `internal/service`: lo sistema il Task 5. Non committare finché il Task 5 non compila — vedi Step 5.)

- [ ] **Step 5: Commit insieme al Task 5**

Questo task non compila da solo il repo (`service.go` chiama `installer.Run`). Lasciare le modifiche nel working tree e committarle con il Task 5.

---

## Task 4: `notify` — messaggi per prodotto

**Files:**
- Modify: `internal/notify/notify.go`
- Modify: `internal/notify/toast_launch.go`
- Test: `internal/notify/messages_test.go` (nuovo)

**Interfaces:**
- Produces:
  - `func CriticalUpdateProductMessage(name string) Message` (Body con un `%d` per i secondi)
  - `func WarnCriticalUpdateProduct(name string, seconds int) bool`
  - `func ProductWaitingMessage(name string) Message`
  - `func ProductUpdatedMessage(name, version string) Message`
  - `SendNotifyBox` non aggiunge più `%!(EXTRA int=…)` a un body senza verbi.

- [ ] **Step 1: Scrivere i test che falliscono**

`internal/notify/messages_test.go`:

```go
package notify

import (
	"fmt"
	"strings"
	"testing"
)

func TestProductMessagesNameTheProduct(t *testing.T) {
	for _, m := range []Message{
		CriticalUpdateProductMessage("3g-RocketChat"),
		ProductWaitingMessage("3g-RocketChat"),
		ProductUpdatedMessage("3g-RocketChat", "1.1.0"),
	} {
		if !strings.Contains(m.Title, "3g-RocketChat") || !strings.Contains(m.Body, "3g-RocketChat") {
			t.Errorf("message does not name the product: %+v", m)
		}
		if strings.Contains(m.Title+m.Body, "EMLy") {
			t.Errorf("product message mentions EMLy: %+v", m)
		}
	}
	if !strings.Contains(ProductUpdatedMessage("X", "1.1.0").Body, "1.1.0") {
		t.Error("updated message must carry the version")
	}
}

func TestCriticalUpdateProductMessageTakesTheCountdown(t *testing.T) {
	body := fmt.Sprintf(CriticalUpdateProductMessage("X").Body, 30)
	if !strings.Contains(body, "30") || strings.Contains(body, "%!") {
		t.Errorf("body = %q", body)
	}
}

// formatBody only formats bodies that have a verb: fmt.Sprintf on a body
// without one appends "%!(EXTRA int=60)" to the text the user reads.
func TestFormatBodyWithoutVerbIsUnchanged(t *testing.T) {
	if got := formatBody("Chiudere l'applicazione.", 60); got != "Chiudere l'applicazione." {
		t.Errorf("formatBody = %q", got)
	}
	if got := formatBody("Chiude tra %d secondi.", 60); got != "Chiude tra 60 secondi." {
		t.Errorf("formatBody = %q", got)
	}
}
```

- [ ] **Step 2: Eseguire i test e verificare che falliscano**

Run: `go test ./internal/notify/`
Expected: FAIL, `undefined: CriticalUpdateProductMessage`.

- [ ] **Step 3: Implementare**

In `notify.go`, estrarre l'invio in un helper e aggiungere le funzioni:

```go
// formatBody applies seconds to body only when body has a verb for it.
func formatBody(body string, seconds int) string {
	if strings.Contains(body, "%") {
		return fmt.Sprintf(body, seconds)
	}
	return body
}

// sendBox shows title/body in the viewer session without waiting, the box
// auto-dismissing after seconds. False when nobody is at the machine.
func sendBox(title, body string, seconds int) bool {
	session, ok := viewerSession()
	if !ok {
		return false
	}
	titleU16, err := windows.UTF16FromString(title)
	if err != nil {
		return false
	}
	bodyU16, err := windows.UTF16FromString(body)
	if err != nil {
		return false
	}
	var response uint32
	// Title/message lengths are in BYTES, excluding the NUL terminator.
	// bWait=FALSE: return immediately; Timeout still auto-dismisses the box.
	ret, _, _ := procWTSSendMessage.Call(
		wtsCurrentServerHandle,
		uintptr(session),
		uintptr(unsafe.Pointer(&titleU16[0])),
		uintptr((len(titleU16)-1)*2),
		uintptr(unsafe.Pointer(&bodyU16[0])),
		uintptr((len(bodyU16)-1)*2),
		uintptr(mbOK|mbIconWarning|mbSetForeground|mbTopMost),
		uintptr(seconds),
		uintptr(unsafe.Pointer(&response)),
		0, // bWait = FALSE
	)
	return ret != 0
}
```

Riscrivere `WarnCriticalUpdate` e `SendNotifyBox` usando `sendBox`:

```go
func WarnCriticalUpdate(lang string, seconds int) bool {
	msg, ok := messages[lang]
	if !ok {
		msg = messages["en"]
	}
	return sendBox(msg.Title, formatBody(msg.Body, seconds), seconds)
}

func SendNotifyBox(msg Message, seconds int) bool {
	return sendBox(msg.Title, formatBody(msg.Body, seconds), seconds)
}

// CriticalUpdateProductMessage is the critical-update countdown warning for a
// product other than EMLy. Italian only, like the progress window. %d is the
// countdown in seconds.
func CriticalUpdateProductMessage(name string) Message {
	return Message{
		Title: name + " - Aggiornamento critico",
		Body:  name + " verrà chiuso tra %d secondi per installare un aggiornamento critico.\n\nSi prega di salvare il proprio lavoro.",
	}
}

// WarnCriticalUpdateProduct is WarnCriticalUpdate for a product other than EMLy.
func WarnCriticalUpdateProduct(name string, seconds int) bool {
	msg := CriticalUpdateProductMessage(name)
	return sendBox(msg.Title, formatBody(msg.Body, seconds), seconds)
}

// ProductWaitingMessage tells the user an update is waiting for them to close
// a product other than EMLy.
func ProductWaitingMessage(name string) Message {
	return Message{
		Title: name + " - Aggiornamento in attesa",
		Body:  "Un aggiornamento di " + name + " è pronto. Chiudere l'applicazione per completarlo.",
	}
}
```

In `toast_launch.go`:

```go
// ProductUpdatedMessage is the update-complete toast for a product other than
// EMLy. Italian only.
func ProductUpdatedMessage(name, version string) Message {
	return Message{
		Title: name + " aggiornato",
		Body:  fmt.Sprintf("%s è stato aggiornato alla versione %s.", name, version),
	}
}
```

- [ ] **Step 4: Eseguire i test**

Run: `go test ./internal/notify/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/notify
git commit -m "feat(notify): product-aware messages; stop appending EXTRA to verb-less bodies"
```

---

## Task 5: Motore per prodotto con EMLy sopra (nessun cambio di comportamento)

**Files:**
- Create: `internal/service/product.go`
- Modify: `internal/service/service.go` (Cycle, apply, install, forceRedownload, runSetupAndVerify, struct `Updater`)
- Modify: `internal/service/progress.go`
- Test: `internal/service/product_test.go` (nuovo)

**Interfaces:**
- Consumes: `product.*` (Task 1), `Store.PendingFor/SetPendingFor/ClearPendingFor` (Task 2), `installer.Driver/Spec/For` (Task 3), `notify.WarnCriticalUpdateProduct/ProductWaitingMessage/ProductUpdatedMessage` (Task 4).
- Produces (usati dai Task 10–12):
  - `type productState struct { Installed string; Fresh bool; Channel, Language string }` e `func (ps productState) from() string`
  - `func emlyState(e config.EMLyInfo) productState`
  - `func (u *Updater) emlyProduct(cyc *cycleState) *product.Product`
  - `func (u *Updater) resolveProductState(p *product.Product) (productState, bool)`
  - `func (u *Updater) productCycle(ctx context.Context, cyc *cycleState, p *product.Product) error`
  - `func (u *Updater) applyProduct(ctx context.Context, cyc *cycleState, p *product.Product, pend *state.Pending, ps productState) error`
  - `func (u *Updater) installProduct(ctx context.Context, cyc *cycleState, p *product.Product, pend *state.Pending, ps productState) error`
  - `func (u *Updater) forceRedownloadProduct(ctx context.Context, cyc *cycleState, p *product.Product, pend *state.Pending, channel string) (*state.Pending, error)`
  - `func (u *Updater) resolveProductTarget(ctx context.Context, cyc *cycleState, p *product.Product, channel string) (source.Source, *manifest.Manifest, manifest.Target, error)`
  - `func (u *Updater) downloadsFor(p *product.Product) *download.Manager`
  - `func (u *Updater) driverFor(p *product.Product) installer.Driver`
  - `func (u *Updater) newProductProgressUI(p *product.Product, target string) *progressUI`
  - seam su `Updater`: `runningFn func(exe string) bool`, `driverFn func(*product.Product) installer.Driver`, `notifyBoxFn func(notify.Message, int) bool`, `toastFn func(icon, title, body string) bool`
  - campi su `Updater` (goroutine di poll, senza lock): `productDownloads map[string]*download.Manager`, `detectWarned map[string]bool`, `waitNotified map[string]string`

- [ ] **Step 1: Scrivere i test che falliscono**

`internal/service/product_test.go`:

```go
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"emlyupdater/internal/config"
	"emlyupdater/internal/product"
	"emlyupdater/internal/state"
)

// fakeDriver stands in for a setup: Install writes version.txt into the
// product's directory (what a real setup does), unless failInstall is set.
type fakeDriver struct {
	dir         string
	failInstall bool
	installs    []string
	uninstalls  int
}

func (d *fakeDriver) Install(_ string, version string) error {
	d.installs = append(d.installs, version)
	if d.failInstall {
		return errors.New("setup exited with code 2")
	}
	return os.WriteFile(filepath.Join(d.dir, "version.txt"), []byte(version+"\r\n"), 0o644)
}

func (d *fakeDriver) Uninstall() error { d.uninstalls++; return nil }

// The built-in EMLy definition reproduces what the cycle has always done.
func TestEMLyProductMatchesTheLegacyConfiguration(t *testing.T) {
	cfg := internalCfg(t, config.SourceInternal)
	u := newTestUpdater(t, cfg, dcNamed("DC-RM2"), ipsOf("172.16.96.50"))
	p := u.emlyProduct(nil)
	if p.Slug != product.EMLySlug || !p.Legacy || !p.InstallWhenAbsent {
		t.Fatalf("emly product = %+v", p)
	}
	if p.InstallDir != cfg.EMLyInstallDir || p.ExeName != cfg.EMLyExeName || p.Name != "EMLy" {
		t.Errorf("emly paths = %+v", p)
	}
	if p.Installer.Type != product.InstallerInno || !p.Installer.CleanReinstall {
		t.Errorf("emly installer = %+v", p.Installer)
	}
	if len(p.Detect) != 1 || p.Detect[0].Path != cfg.EMLyConfigFile || p.Detect[0].Key != "GUI_SEMVER" {
		t.Errorf("emly detect = %+v", p.Detect)
	}
}

func rocketChat(dir string) *product.Product {
	return &product.Product{
		Slug: "3g-rocketchat", Name: "3g-RocketChat", InstallDir: dir, ExeName: "3g-RocketChat.exe",
		Channel: "stable", Installer: product.InstallerSpec{Type: product.InstallerNSIS},
		Detect: []product.VersionSource{{Type: product.SourceINI, Path: "config.ini", Section: "app", Key: "version"}},
	}
}

// A generic product installs through its driver, is verified by re-running
// its detection chain, and its pending entry is cleared afterwards.
func TestInstallProductVerifiesThroughDetection(t *testing.T) {
	dir := t.TempDir()
	u := newClientTestUpdater(t)
	u.Store = &state.Store{Path: filepath.Join(t.TempDir(), "state.json")}
	u.toastFn = func(string, string, string) bool { return true }

	p := rocketChat(dir)
	// Detection reads config.ini; the fake setup writes it.
	drv2 := &iniDriver{dir: dir}
	u.driverFn = func(*product.Product) installer.Driver { return drv2 }

	setup := filepath.Join(t.TempDir(), "setup.exe")
	_ = os.WriteFile(setup, []byte("setup-bytes"), 0o644)
	sum := sha256.Sum256([]byte("setup-bytes"))
	pend := &state.Pending{Version: "1.1.0", SetupPath: setup, SHA256: hex.EncodeToString(sum[:])}
	_ = u.Store.SetPendingFor(p.Slug, pend)

	if err := u.installProduct(context.Background(), nil, p, pend, productState{Installed: "1.0.0", Channel: "stable", Language: "it"}); err != nil {
		t.Fatalf("installProduct: %v", err)
	}
	if len(drv2.installs) != 1 || drv2.installs[0] != "1.1.0" {
		t.Errorf("installs = %v", drv2.installs)
	}
	if got, _ := u.Store.PendingFor(p.Slug); got != nil {
		t.Errorf("pending not cleared: %+v", got)
	}
}

// iniDriver writes [app] version the way RocketChat's future setup will.
type iniDriver struct {
	dir      string
	installs []string
}

func (d *iniDriver) Install(_ string, version string) error {
	d.installs = append(d.installs, version)
	return os.WriteFile(filepath.Join(d.dir, "config.ini"), []byte("[app]\nversion = "+version+"\n"), 0o644)
}
func (d *iniDriver) Uninstall() error { return nil }
```

Aggiungere `"emlyupdater/internal/installer"` agli import. (`fakeDriver` serve ai Task 10; in questo task basta che compili.)

- [ ] **Step 2: Eseguire i test e verificare che falliscano**

Run: `go test ./internal/service/ -run "EMLyProduct|InstallProduct"`
Expected: FAIL di compilazione (`u.emlyProduct undefined`, `installer.Run` inesistente dopo il Task 3).

- [ ] **Step 3: Seam e campi in `Updater`**

In `service.go`, nella struct `Updater` (vicino a `emlyRunningFn`), aggiungere:

```go
	// runningFn/driverFn/notifyBoxFn/toastFn are the product engine's seams
	// (product.go): tests fake the running check, the setup, the user
	// notification and the update-complete toast of products other than
	// EMLy. nil means process.IsRunning, installer.For, notify.SendNotifyBox
	// and notify.LaunchToast.
	runningFn   func(exe string) bool
	driverFn    func(*product.Product) installer.Driver
	notifyBoxFn func(notify.Message, int) bool
	toastFn     func(icon, title, body string) bool

	// productDownloads caches one download.Manager per product other than
	// EMLy (downloadsFor). detectWarned and waitNotified keep the per-product
	// "unknown version" and "close the app" messages to one per session and
	// one per version. Poll goroutine only, like announced: no locking.
	productDownloads map[string]*download.Manager
	detectWarned     map[string]bool
	waitNotified     map[string]string
```

- [ ] **Step 4: Creare `internal/service/product.go`**

Il corpo è il codice di oggi di `Cycle` (da "EMLy config.ini not found" in giù), `apply`, `install`, `forceRedownload`, `runSetupAndVerify`, parametrizzato. Ogni messaggio di log e ogni evento di EMLy resta identico quando `p.Name == "EMLy"`.

```go
package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"emlyupdater/internal/assoc"
	"emlyupdater/internal/config"
	"emlyupdater/internal/download"
	"emlyupdater/internal/installer"
	"emlyupdater/internal/logging"
	"emlyupdater/internal/manifest"
	"emlyupdater/internal/notify"
	"emlyupdater/internal/process"
	"emlyupdater/internal/product"
	"emlyupdater/internal/source"
	"emlyupdater/internal/state"
	"emlyupdater/internal/wsclient"
)

// productState is what the engine needs to know about a product on this
// machine, resolved once per product per cycle.
type productState struct {
	Installed string // version on disk; config.FreshInstallVersion when absent
	Fresh     bool   // absent: Installed is the comparison sentinel, not a release
	Channel   string // "stable" | "beta"
	Language  string // notification language; EMLy's LANGUAGE, "it" otherwise
}

// from is the version to report as "installed before this update": empty on
// a fresh install - the 0.0.0 sentinel is not a release.
func (ps productState) from() string {
	if ps.Fresh {
		return ""
	}
	return ps.Installed
}

func emlyState(e config.EMLyInfo) productState {
	return productState{Installed: e.InstalledVersion, Fresh: e.FreshInstall, Channel: e.Channel, Language: e.Language}
}

// emlyProduct is the built-in EMLy definition. cyc may be nil (tests calling
// install directly); the channel override then stays empty.
func (u *Updater) emlyProduct(cyc *cycleState) *product.Product {
	channel := ""
	if cyc != nil && cyc.eff != nil {
		channel = cyc.eff.Doc.Updater.Channel()
	}
	return &product.Product{
		Slug:       product.EMLySlug,
		Name:       "EMLy",
		InstallDir: u.Cfg.EMLyInstallDir,
		ExeName:    u.Cfg.EMLyExeName,
		Channel:    channel,
		Detect: []product.VersionSource{{Type: product.SourceINI, Path: u.Cfg.EMLyConfigFile,
			Section: "EMLy", Key: "GUI_SEMVER"}},
		Installer:         product.InstallerSpec{Type: product.InstallerInno, CleanReinstall: true},
		InstallWhenAbsent: true,
		Legacy:            true,
	}
}

// resolveProductState reads what is installed. EMLy goes through
// ResolveEMLyWithChannel exactly as before (an unreadable config.ini is its
// fresh-install mode, and p.Channel carries the document's override). Any
// other product goes through its detection chain; ok=false means Unknown and
// the product must be left alone this cycle.
func (u *Updater) resolveProductState(p *product.Product) (productState, bool) {
	if p.Legacy {
		return emlyState(u.Cfg.ResolveEMLyWithChannel(p.Channel)), true
	}
	channel := p.Channel
	if channel != "beta" {
		channel = "stable"
	}
	ps := productState{Channel: channel, Language: "it"}
	r := product.Detect(p)
	switch r.Outcome {
	case product.Installed:
		ps.Installed = r.Version
	case product.Absent:
		ps.Installed, ps.Fresh = config.FreshInstallVersion, true
	default:
		if u.detectWarned == nil {
			u.detectWarned = map[string]bool{}
		}
		if !u.detectWarned[p.Slug] {
			u.detectWarned[p.Slug] = true
			u.Log.Warn("installed version unreadable, product left alone", "product", p.Slug, "detect", r.String())
		}
		return ps, false
	}
	if u.detectWarned[p.Slug] {
		delete(u.detectWarned, p.Slug)
	}
	return ps, true
}

// downloadsFor is the download cache of p: EMLy keeps u.Downloads (prefix
// "EMLy-"), every other product gets its own prefix in the same directory and
// the same Pacer - the server's download slots are one pool.
func (u *Updater) downloadsFor(p *product.Product) *download.Manager {
	if p.Legacy {
		return u.Downloads
	}
	if m, ok := u.productDownloads[p.Slug]; ok {
		return m
	}
	if u.productDownloads == nil {
		u.productDownloads = map[string]*download.Manager{}
	}
	m := &download.Manager{Dir: u.Downloads.Dir, Prefix: p.Slug + "-", Pacer: u.Downloads.Pacer, Log: u.Downloads.Log}
	u.productDownloads[p.Slug] = m
	return m
}

func (u *Updater) driverFor(p *product.Product) installer.Driver {
	if u.driverFn != nil {
		return u.driverFn(p)
	}
	return installer.For(installer.Spec{Slug: p.Slug, Type: p.Installer.Type, InstallDir: p.InstallDir,
		LogsDir: config.LogsDir(), ForceUpgrade: p.Legacy})
}

func (u *Updater) isRunning(exe string) bool {
	if u.runningFn != nil {
		return u.runningFn(exe)
	}
	return process.IsRunning(exe)
}

func (u *Updater) notifyBox(msg notify.Message, seconds int) bool {
	if u.notifyBoxFn != nil {
		return u.notifyBoxFn(msg, seconds)
	}
	return notify.SendNotifyBox(msg, seconds)
}

// resolveProductTarget is resolveTarget for any product. EMLy keeps the
// historical path (resolveTarget, preferred-server bookkeeping included).
// Other products are a read of the same server chain on their own route and
// never move the preferred server.
func (u *Updater) resolveProductTarget(ctx context.Context, cyc *cycleState, p *product.Product, channel string) (source.Source, *manifest.Manifest, manifest.Target, error) {
	if p.Legacy {
		return u.resolveTarget(ctx, cyc, channel)
	}
	resolver := u.newProductResolver(cyc, p)
	src, m, err := resolver.Resolve(ctx)
	if err != nil {
		return nil, nil, manifest.Target{}, err
	}
	target, err := src.ResolveTarget(m, channel)
	if err != nil {
		return nil, nil, manifest.Target{}, err
	}
	return src, m, target, nil
}

// newProductResolver is newResolver on p's manifest route.
func (u *Updater) newProductResolver(cyc *cycleState, p *product.Product) *source.Resolver {
	var urls []string
	for _, name := range u.preferredChain(cyc) {
		if base := cyc.eff.BaseURL(name); base != "" {
			urls = append(urls, base+p.ManifestPath())
		}
	}
	if len(urls) == 0 {
		urls = append(urls, "")
	}
	settings := cyc.eff.Doc.Updater.Resolver
	resolver := &source.Resolver{
		Primary:     u.newHTTPSource(urls[0]),
		Attempts:    settings.Attempts,
		BaseBackoff: settings.BaseBackoff(),
		Document:    p.Slug + " manifest",
		Logf: func(format string, args ...any) {
			u.Log.Info(fmt.Sprintf(format, args...))
		},
	}
	for _, url := range urls[1:] {
		resolver.Fallbacks = append(resolver.Fallbacks, u.newHTTPSource(url))
	}
	return resolver
}

// productCycle is one product's share of a cycle: resume a pending install,
// or poll, download and install. It is what Cycle did for EMLy alone.
func (u *Updater) productCycle(ctx context.Context, cyc *cycleState, p *product.Product) error {
	ps, ok := u.resolveProductState(p)
	if !ok {
		return nil
	}
	if p.Legacy && ps.Fresh {
		u.Log.Info("EMLy config.ini not found - fresh-install mode",
			"assumedVersion", ps.Installed, "channel", ps.Channel)
	}
	if ps.Fresh && !p.InstallWhenAbsent {
		return nil
	}
	dl := u.downloadsFor(p)

	// 1) A persisted pending update takes priority over polling: it may have
	// been queued right before a reboot and must not be lost or re-fetched.
	pend, err := u.Store.PendingFor(p.Slug)
	if err != nil {
		u.Log.Warn("state file unreadable, starting fresh", "error", err.Error())
		pend = nil
	}
	if pend != nil && !pend.GaveUp {
		stillNeeded, err := manifest.Less(ps.Installed, pend.Version)
		if err != nil {
			u.Log.Warn("pending update has invalid version, discarding", "product", p.Slug, "version", pend.Version, "error", err.Error())
			_ = u.Store.ClearPendingFor(p.Slug)
		} else if !stillNeeded {
			// Installed by other means (or the pending entry is stale).
			u.Log.Info("pending update already satisfied, clearing", "product", p.Slug, "version", pend.Version)
			_ = u.Store.ClearPendingFor(p.Slug)
			_ = dl.CleanupExcept("")
		} else if err := download.VerifyFile(pend.SetupPath, pend.SHA256); err != nil {
			u.Log.Warn("pending setup failed re-verification, discarding for re-download", "product", p.Slug, "error", err.Error())
			_ = os.Remove(pend.SetupPath)
			_ = u.Store.ClearPendingFor(p.Slug)
		} else {
			u.Log.Info("resuming pending update", "product", p.Slug, "version", pend.Version, "forced", pend.Forced)
			u.cycleTrigger = "resume"
			u.progress = u.newProductProgressUI(p, pend.Version)
			defer u.endProgress()
			return u.applyProduct(ctx, cyc, p, pend, ps)
		}
	}

	// 2) Normal poll: manifest via this machine's server chain.
	src, m, target, err := u.resolveProductTarget(ctx, cyc, p, ps.Channel)
	if err != nil {
		if p.Legacy {
			u.notifySourcesUnreachable()
		}
		return err
	}
	if p.Legacy {
		u.sourcesUnreachableNotified = false
	}

	needUpdate, err := manifest.Less(ps.Installed, target.Version)
	if err != nil {
		return err
	}
	if !needUpdate {
		u.Log.Debug("already on latest version", "product", p.Slug, "installed", ps.Installed,
			"target", target.Version, "channel", ps.Channel)
		// Nothing pending, nothing needed: superseded setups can go.
		_ = dl.CleanupExcept("")
		return nil
	}

	forced, err := m.Forced(ps.Installed)
	if err != nil {
		return err
	}

	u.Log.InfoEvent(logging.EventUpdateFound, "update available", "product", p.Slug,
		"installed", ps.Installed, "target", target.Version,
		"channel", ps.Channel, "forced", forced, "source", src.Name())

	if p.Legacy {
		enabled, _ := cyc.eff.UpdaterEnabled(cyc.host.Now)
		mc := wsclient.ManifestCheck{Target: "emly", Channel: ps.Channel, AvailableVersion: target.Version,
			UpdateAvailable: true, Critical: forced, MinRequiredVersion: m.MinRequiredVersion,
			Decision: emlyDecision(true, forced, u.emlyRunning(), !enabled),
			Source:   u.serverRef(cyc, sourceURL(src)), CheckedAt: u.clock().UTC().Format(time.RFC3339)}
		if !ps.Fresh {
			mc.InstalledVersion = ps.Installed
		}
		u.announceUpdate(mc)
	}

	u.progress = u.newProductProgressUI(p, target.Version)
	defer u.endProgress()
	setupPath, err := dl.Ensure(u.progress.watch(ctx), src, target)
	if err != nil {
		// A full download queue is the server pacing the fleet, not a
		// failure: Ensure has already waited out what it could and logged
		// each refusal, and the next cycle tries again.
		if download.IsQueueFull(err) {
			u.Log.Info(fmt.Sprintf("server download queue full, %s setup download retried next cycle", p.Name),
				"target", target.Version)
			return nil
		}
		return fmt.Errorf("download/verification failed: %w", err)
	}

	pend = &state.Pending{
		Version:      target.Version,
		SetupPath:    setupPath,
		SHA256:       target.SHA256,
		Forced:       forced,
		DownloadedAt: time.Now().UTC(),
	}
	// Persist before applying so a crash/reboot at any later point resumes
	// from the verified local file instead of re-downloading.
	if err := u.Store.SetPendingFor(p.Slug, pend); err != nil {
		u.Log.Warn("failed to persist pending update, continuing", "product", p.Slug, "error", err.Error())
	}

	return u.applyProduct(ctx, cyc, p, pend, ps)
}
```

`applyProduct` = il vecchio `apply` con queste differenze (tutto il resto — commenti compresi — si sposta tale e quale):

```go
func (u *Updater) applyProduct(ctx context.Context, cyc *cycleState, p *product.Product, pend *state.Pending, ps productState) error {
	if u.destructivePendingNow() {
		u.logDestructiveSkipOnce()
		return nil
	}

	exe := p.ExeName

	if u.isRunning(exe) {
		u.progress.close()
		if pend.Forced {
			warning := cyc.eff.Doc.Updater.CriticalWarning
			if warning.Enabled {
				seconds := warning.Seconds
				shown := false
				if p.Legacy {
					shown = notify.WarnCriticalUpdate(ps.Language, seconds)
				} else {
					shown = notify.WarnCriticalUpdateProduct(p.Name, seconds)
				}
				if shown {
					u.Log.Info("critical update warning shown, counting down",
						"product", p.Slug, "seconds", seconds, "language", ps.Language)
					select {
					case <-time.After(time.Duration(seconds) * time.Second):
					case <-ctx.Done():
						return ctx.Err()
					}
				} else {
					u.Log.Info("no active user session (console or RDP), skipping warning")
				}
			}
			if u.destructivePendingNow() {
				u.logDestructiveSkipOnce()
				return nil
			}
			killed, err := process.TerminateAll(exe)
			if err != nil {
				u.Log.Warn(fmt.Sprintf("terminating %s reported errors", p.Name), "killed", killed, "error", err.Error())
			}
			u.Log.WarnEvent(logging.EventForcedKill, fmt.Sprintf("terminated %s for forced update", p.Name),
				"instances", killed, "target", pend.Version)
		} else if p.Legacy {
			// ... blocco "Notify the user via MSGBox ..." + WaitForExit di oggi,
			// invariato, con notify.SendNotifyBox sostituito da u.notifyBox ...
		} else {
			// A product other than EMLy never blocks the cycle: it may be a
			// chat kept open all day, and waiting on it would hold every
			// product after it. The pending entry stays; the next cycle
			// installs as soon as the app is closed.
			u.notifyWaitingOnce(p, pend.Version)
			u.Log.Info("app is running and the update is not forced - install deferred to a later cycle",
				"product", p.Slug, "target", pend.Version)
			return nil
		}
	}

	return u.installProduct(ctx, cyc, p, pend, ps)
}

// notifyWaitingOnce tells the user at the machine that p's update waits for
// the app to close - once per target version per service session.
func (u *Updater) notifyWaitingOnce(p *product.Product, version string) {
	if u.waitNotified == nil {
		u.waitNotified = map[string]string{}
	}
	if u.waitNotified[p.Slug] == version {
		return
	}
	u.waitNotified[p.Slug] = version
	u.notifyBox(notify.ProductWaitingMessage(p.Name), 60)
}
```

Nota per l'implementatore: nel ramo `p.Legacy` il testo EMLy (`"EMLy - Aggiornamento sospeso"` / `"EMLy - Update Pending"`) e `process.WaitForExit(ctx, exe)` restano identici; usare `ps.Language` al posto di `emly.Language`.

`installProduct` = il vecchio `install`, parametrizzato:

```go
func (u *Updater) installProduct(ctx context.Context, cyc *cycleState, p *product.Product, pend *state.Pending, ps productState) error {
	if !u.beginInstall(p.Name + " install") {
		return fmt.Errorf("%s install skipped: a destructive client command is pending", p.Name)
	}
	defer u.endInstall()

	from := ps.from()

	// Final integrity gate immediately before execution.
	if err := download.VerifyFile(pend.SetupPath, pend.SHA256); err != nil {
		_ = os.Remove(pend.SetupPath)
		_ = u.Store.ClearPendingFor(p.Slug)
		if p.Legacy {
			u.emitUpdateFailed(updateEvent{Target: "emly", FromVersion: from, ToVersion: pend.Version,
				WillRetry: true, Error: &wsclient.ErrorBody{Code: "checksum_mismatch", Message: err.Error()}})
		}
		return fmt.Errorf("refusing to install: %w", err)
	}

	started := u.clock()
	if p.Legacy {
		u.emit(wsclient.EvtUpdateStarted, updateEvent{Target: "emly", FromVersion: from, ToVersion: pend.Version,
			Forced: pend.Forced, Attempt: 1, Trigger: u.cycleTrigger})
	}
	u.progress.installing()

	reinstalled := false
	if err := u.runSetupAndVerifyProduct(p, pend, "running setup"); err != nil {
		how := "retrying with a fresh download"
		if p.Installer.CleanReinstall {
			how = "forcing a clean reinstall over the existing state"
		}
		u.Log.WarnEvent(logging.EventInstallFailed,
			fmt.Sprintf("%s did not reach the target version, %s", p.Name, how),
			"version", pend.Version, "error", err.Error())
		reinstalled = true

		if fresh, ferr := u.forceRedownloadProduct(ctx, cyc, p, pend, ps.Channel); ferr != nil {
			u.Log.Warn("could not force a fresh download for the retry, retrying with the cached copy",
				"product", p.Slug, "version", pend.Version, "error", ferr.Error())
		} else {
			pend = fresh
		}

		u.progress.installing() // back from the re-download's progress
		label := "running setup (retry)"
		if p.Installer.CleanReinstall {
			label = "running setup (clean install)"
			if uerr := u.driverFor(p).Uninstall(); uerr != nil {
				// Best-effort: a failed cleanup is not itself a reason to give up
				// on the reinstall (e.g. no uninstaller present at all).
				u.Log.Warn("clean-install uninstall step reported an error, reinstalling anyway",
					"product", p.Slug, "version", pend.Version, "error", uerr.Error())
			}
		}

		if err := u.runSetupAndVerifyProduct(p, pend, label); err != nil {
			u.Log.ErrorEvent(logging.EventInstallFailed, fmt.Sprintf("%s clean install failed", p.Name),
				"version", pend.Version, "error", err.Error())
			if p.Legacy {
				u.emitUpdateFailed(updateEvent{Target: "emly", FromVersion: from, ToVersion: pend.Version,
					Attempt: 2, WillRetry: true, Error: &wsclient.ErrorBody{Code: installFailureCode(err), Message: err.Error()}})
			}
			return err // pending kept → retried next cycle
		}
	}

	u.Log.InfoEvent(logging.EventInstallOK, fmt.Sprintf("%s updated successfully", p.Name), "version", pend.Version)

	if p.Legacy {
		attempt := 1
		if reinstalled {
			attempt = 2
		}
		u.emit(wsclient.EvtUpdateApplied, updateEvent{Target: "emly", FromVersion: from, ToVersion: pend.Version,
			Forced: pend.Forced, Attempt: attempt, DurationMS: u.clock().Sub(started).Milliseconds(), Reinstalled: reinstalled})
	}

	u.progress.close() // before the toast, which says the same thing is over
	u.showProductToast(p, pend.Version)

	if err := u.Store.ClearPendingFor(p.Slug); err != nil {
		u.Log.Warn("failed to clear pending state", "product", p.Slug, "error", err.Error())
	}
	if err := u.downloadsFor(p).CleanupExcept(pend.Version); err != nil {
		u.Log.Warn("failed to clean up old downloads", "product", p.Slug, "error", err.Error())
	}

	if p.Legacy {
		// Association self-heal is a backstop; its failure must not fail the
		// (already successful) update.
		exePath := assoc.ExePath(u.Cfg.EMLyInstallDir, u.Cfg.EMLyExeName)
		mappings := assoc.DefaultMappings(u.Cfg.ProgIDEml, u.Cfg.ProgIDMsg)
		changed, err := assoc.Repair(exePath, mappings, func(format string, args ...any) {
			u.Log.Info(fmt.Sprintf(format, args...))
		})
		if err != nil {
			u.Log.Warn("file association repair failed", "error", err.Error())
		} else if changed {
			u.Log.InfoEvent(logging.EventAssocRepaired, "file associations repaired", "exe", exePath)
		}
	}
	return nil
}
```

Attenzione: con `p.Name == "EMLy"` e `CleanReinstall == true` i messaggi `"EMLy did not reach the target version, forcing a clean reinstall over the existing state"`, `"EMLy clean install failed"`, `"EMLy updated successfully"`, `"running setup (clean install)"` e `"EMLy install skipped: a destructive client command is pending"` sono identici a oggi. Stesso vale per il warn "could not force a fresh download…": il testo originale era `"could not force a fresh download for the clean-install retry, retrying with the cached copy"`; per EMLy usare quello:

```go
			msg := "could not force a fresh download for the retry, retrying with the cached copy"
			if p.Legacy {
				msg = "could not force a fresh download for the clean-install retry, retrying with the cached copy"
			}
```

Il resto:

```go
// forceRedownloadProduct is forceRedownload for any product: wipe p's cache
// and pending entry, re-resolve, fetch from scratch. The new entry keeps
// pend's Forced and Attempts.
func (u *Updater) forceRedownloadProduct(ctx context.Context, cyc *cycleState, p *product.Product, pend *state.Pending, channel string) (*state.Pending, error) {
	dl := u.downloadsFor(p)
	if err := dl.CleanupExcept(""); err != nil {
		u.Log.Warn("failed to fully clear the downloads cache before forcing a re-download", "product", p.Slug, "error", err.Error())
	}
	if err := u.Store.ClearPendingFor(p.Slug); err != nil {
		u.Log.Warn("failed to clear state.json before forcing a re-download", "product", p.Slug, "error", err.Error())
	}

	src, _, target, err := u.resolveProductTarget(ctx, cyc, p, channel)
	if err != nil {
		return nil, err
	}
	setupPath, err := dl.Ensure(u.progress.watch(ctx), src, target)
	if err != nil {
		return nil, fmt.Errorf("re-download failed: %w", err)
	}

	fresh := &state.Pending{
		Version:      target.Version,
		SetupPath:    setupPath,
		SHA256:       target.SHA256,
		Forced:       pend.Forced,
		DownloadedAt: time.Now().UTC(),
		Attempts:     pend.Attempts,
	}
	if err := u.Store.SetPendingFor(p.Slug, fresh); err != nil {
		u.Log.Warn("failed to persist re-downloaded pending update, continuing", "product", p.Slug, "error", err.Error())
	}
	u.Log.Info("re-downloaded setup for the retry", "product", p.Slug, "version", fresh.Version, "path", fresh.SetupPath)
	return fresh, nil
}

// runSetupAndVerifyProduct runs p's setup and confirms the target version is
// now on disk: EMLy through its config.ini (VerifyInstalled, unchanged),
// every other product by re-running its detection chain.
func (u *Updater) runSetupAndVerifyProduct(p *product.Product, pend *state.Pending, label string) error {
	u.Log.Info(label, "product", p.Slug, "path", pend.SetupPath, "version", pend.Version)
	if err := u.driverFor(p).Install(pend.SetupPath, pend.Version); err != nil {
		return err
	}
	if p.Legacy {
		return installer.VerifyInstalled(u.Cfg.EMLyConfigFile, pend.Version)
	}
	r := product.Detect(p)
	if r.Outcome != product.Installed {
		return fmt.Errorf("post-install verification failed: %s", r)
	}
	older, err := manifest.Less(r.Version, pend.Version)
	if err != nil {
		return fmt.Errorf("post-install verification failed: %w", err)
	}
	if older {
		return fmt.Errorf("post-install verification failed: %s reports %s, expected %s", r.Source, r.Version, pend.Version)
	}
	return nil
}

// showProductToast: EMLy keeps its localized toast; other products get the
// Italian one with their own icon.
func (u *Updater) showProductToast(p *product.Product, version string) {
	if p.Legacy {
		u.showUpdateToast(version)
		return
	}
	msg := notify.ProductUpdatedMessage(p.Name, version)
	icon := filepath.Join(p.InstallDir, p.ExeName)
	launch := u.toastFn
	if launch == nil {
		self, err := os.Executable()
		if err != nil {
			u.Log.Warn("failed to resolve own executable path, skipping update toast", "error", err.Error())
			return
		}
		launch = func(icon, title, body string) bool { return notify.LaunchToast(self, icon, title, body) }
	}
	if launch(icon, msg.Title, msg.Body) {
		u.Log.Info("update-complete toast shown", "product", p.Slug, "version", version)
	} else {
		u.Log.Info("update-complete toast skipped (no active user session, console or RDP)", "product", p.Slug, "version", version)
	}
}
```

- [ ] **Step 5: Ridurre `service.go` ai wrapper**

In `Cycle`, sostituire tutto da `emly := u.Cfg.ResolveEMLyWithChannel(...)` fino alla fine della funzione con:

```go
	return u.productCycle(ctx, cyc, u.emlyProduct(cyc))
}
```

Sostituire i corpi di `apply`, `install`, `forceRedownload`, `runSetupAndVerify` con wrapper (i commenti lunghi di oggi si spostano sulle funzioni `…Product` in `product.go`):

```go
// apply is applyProduct for EMLy.
func (u *Updater) apply(ctx context.Context, cyc *cycleState, p *state.Pending, emly config.EMLyInfo) error {
	return u.applyProduct(ctx, cyc, u.emlyProduct(cyc), p, emlyState(emly))
}

// install is installProduct for EMLy.
func (u *Updater) install(ctx context.Context, cyc *cycleState, p *state.Pending, emly config.EMLyInfo) error {
	return u.installProduct(ctx, cyc, u.emlyProduct(cyc), p, emlyState(emly))
}
```

Eliminare `forceRedownload` e `runSetupAndVerify` se, dopo la sostituzione, non hanno più chiamanti (`grep -n "forceRedownload(\|runSetupAndVerify(" internal/service`). Rimuovere gli import rimasti inutilizzati in `service.go`.

- [ ] **Step 6: Progress window per prodotto**

In `progress.go`:

```go
// newProductProgressUI is newProgressUI for any product: EMLy keeps its own
// window, every other product gets its name and its executable's icon.
func (u *Updater) newProductProgressUI(p *product.Product, target string) *progressUI {
	ui := u.newProgressUI(false, target)
	if ui == nil || p.Legacy {
		return ui
	}
	ui.product = p.Name
	ui.iconPath = filepath.Join(p.InstallDir, p.ExeName)
	return ui
}
```

In `installText`, rendere il testo non-self generico (per EMLy resta identico):

```go
	return heading, fmt.Sprintf("Attendere il completamento. %s non è disponibile fino al termine.", p.product)
```

- [ ] **Step 7: Eseguire tutta la suite**

Run: `go build ./... && go test ./...`
Expected: PASS, **senza aver modificato alcun test preesistente**. Se un test di `internal/service` fallisce, il refactor ha cambiato un comportamento di EMLy: correggere il codice, non il test.

- [ ] **Step 8: Commit (Task 3 + Task 5)**

```bash
git add internal/installer internal/service
git commit -m "refactor(service): per-product update engine with EMLy as the built-in product"
```

---

# Fase 2 — prodotti generici

## Task 6: Sorgenti `file` ed `exe`

**Files:**
- Modify: `internal/product/detect.go`
- Create: `internal/product/exe_windows.go`
- Test: `internal/product/detect_test.go`

**Interfaces:**
- Consumes: Task 1.
- Produces: `readSource` gestisce `SourceFile` e `SourceExe`; `func readExeVersion(path string) (string, error)`.

- [ ] **Step 1: Scrivere i test che falliscono** (in coda a `detect_test.go`)

```go
// version.txt as a Windows setup writes it: UTF-8 BOM, CRLF, trailing newline.
func TestDetectFileStripsBOMAndLineEndings(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "version.txt", "\xEF\xBB\xBF1.0.0\r\n")
	r := Detect(&Product{InstallDir: dir, Detect: []VersionSource{{Type: SourceFile, Path: "version.txt"}}})
	if r.Outcome != Installed || r.Version != "1.0.0" {
		t.Fatalf("got %+v, want installed 1.0.0", r)
	}
}

func TestDetectEmptyFileIsUnknown(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "version.txt", "\r\n")
	if r := Detect(&Product{InstallDir: dir, Detect: []VersionSource{{Type: SourceFile, Path: "version.txt"}}}); r.Outcome != Unknown {
		t.Fatalf("got %+v, want unknown", r)
	}
}

// RocketChat's chain today: version.txt first, the (stale) config.ini
// version only when version.txt is gone.
func TestDetectFileBeforeINI(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "version.txt", "1.1.0\n")
	write(t, dir, "config.ini", "[app]\nversion = 1.0.0\n")
	chain := []VersionSource{{Type: SourceFile, Path: "version.txt"}, iniSource("config.ini")}
	if r := Detect(&Product{InstallDir: dir, Detect: chain}); r.Version != "1.1.0" {
		t.Fatalf("got %+v, want 1.1.0 from version.txt", r)
	}
	_ = os.Remove(filepath.Join(dir, "version.txt"))
	if r := Detect(&Product{InstallDir: dir, Detect: chain}); r.Version != "1.0.0" || r.Source != "ini:config.ini" {
		t.Fatalf("got %+v, want 1.0.0 from the ini once version.txt is gone", r)
	}
}

// notepad.exe carries VERSIONINFO on every Windows install.
func TestDetectExeReadsVersionInfo(t *testing.T) {
	sys := os.Getenv("SystemRoot")
	if sys == "" {
		sys = `C:\Windows`
	}
	p := &Product{InstallDir: filepath.Join(sys, "System32"), Detect: []VersionSource{{Type: SourceExe, Path: "notepad.exe"}}}
	r := Detect(p)
	if r.Outcome != Installed || strings.Count(r.Version, ".") != 3 {
		t.Fatalf("got %+v, want installed a.b.c.d", r)
	}
}

// An exe without VERSIONINFO exists but says nothing: unknown, like the
// current 3g-RocketChat.exe. The test binary itself has none.
func TestDetectExeWithoutVersionInfoIsUnknown(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := &Product{InstallDir: filepath.Dir(self), Detect: []VersionSource{{Type: SourceExe, Path: filepath.Base(self)}}}
	if r := Detect(p); r.Outcome != Unknown {
		t.Fatalf("got %+v, want unknown", r)
	}
}
```

- [ ] **Step 2: Eseguire i test e verificare che falliscano**

Run: `go test ./internal/product/ -run "File|Exe"`
Expected: FAIL (`unknown version source type "file"` → Unknown invece di Installed).

- [ ] **Step 3: Implementare**

In `detect.go`, `readSource`:

```go
	case SourceFile:
		return readVersionFile(path)
	case SourceExe:
		return readExeVersion(path)
```

e aggiungere:

```go
// readVersionFile returns the first line of a plain-text version file,
// without a UTF-8 BOM and surrounding whitespace.
func readVersionFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	text := strings.TrimPrefix(string(data), "\xEF\xBB\xBF")
	line, _, _ := strings.Cut(text, "\n")
	v := strings.TrimSpace(line)
	if v == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return checkVersion(v)
}
```

`internal/product/exe_windows.go`:

```go
package product

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// readExeVersion returns the fixed VERSIONINFO of the executable at path:
// ProductVersion, or FileVersion when ProductVersion is all zeros, as
// "a.b.c.d". A missing file wraps fs.ErrNotExist; an exe without VERSIONINFO
// is an error (the file exists, so the chain reports Unknown).
func readExeVersion(path string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	size, err := windows.GetFileVersionInfoSize(path, nil)
	if err != nil || size == 0 {
		return "", fmt.Errorf("%s has no VERSIONINFO: %v", path, err)
	}
	buf := make([]byte, size)
	if err := windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&buf[0])); err != nil {
		return "", fmt.Errorf("read VERSIONINFO of %s: %w", path, err)
	}
	var fixed *windows.VS_FIXEDFILEINFO
	var fixedLen uint32
	if err := windows.VerQueryValue(unsafe.Pointer(&buf[0]), `\`, unsafe.Pointer(&fixed), &fixedLen); err != nil || fixed == nil {
		return "", fmt.Errorf("%s has no fixed VERSIONINFO: %v", path, err)
	}
	ms, ls := fixed.ProductVersionMS, fixed.ProductVersionLS
	if ms == 0 && ls == 0 {
		ms, ls = fixed.FileVersionMS, fixed.FileVersionLS
	}
	if ms == 0 && ls == 0 {
		return "", fmt.Errorf("%s reports version 0.0.0.0", path)
	}
	return fmt.Sprintf("%d.%d.%d.%d", ms>>16, ms&0xffff, ls>>16, ls&0xffff), nil
}
```

- [ ] **Step 4: Eseguire i test**

Run: `go test ./internal/product/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/product
git commit -m "feat(product): file and exe version sources"
```

---

## Task 7: Driver NSIS e controllo ACL dell'uninstaller

**Files:**
- Create: `internal/installer/nsis.go`
- Create: `internal/installer/acl_windows.go`
- Modify: `internal/installer/installer.go` (`For`), `internal/installer/inno.go` (controllo ACL)
- Test: `internal/installer/nsis_test.go`, `internal/installer/acl_windows_test.go`

**Interfaces:**
- Consumes: `Spec`, `runSilent` (Task 3).
- Produces:
  - `func nsisCmdLine(exe string, tail string) string`
  - `var ErrUserWritable error`
  - `func CheckNotUserWritable(paths ...string) error`

- [ ] **Step 1: Scrivere i test che falliscono**

`internal/installer/nsis_test.go`:

```go
package installer

import (
	"os"
	"path/filepath"
	"testing"
)

// NSIS reads /D= and _?= raw to the end of the command line: they must come
// last and must not be quoted, even with spaces - exec.Command would quote
// them, and NSIS would then install into a directory literally named with
// the quotes.
func TestNSISCommandLines(t *testing.T) {
	dir := `C:\Program Files\3gIT\3g-RocketChat`
	if got, want := nsisCmdLine(`C:\dl\3g-rocketchat-1.1.0-setup.exe`, "/S /D="+dir),
		`C:\dl\3g-rocketchat-1.1.0-setup.exe /S /D=C:\Program Files\3gIT\3g-RocketChat`; got != want {
		t.Errorf("install = %q\nwant      %q", got, want)
	}
	if got, want := nsisCmdLine(dir+`\uninstall.exe`, "/S _?="+dir),
		`"C:\Program Files\3gIT\3g-RocketChat\uninstall.exe" /S _?=C:\Program Files\3gIT\3g-RocketChat`; got != want {
		t.Errorf("uninstall = %q\nwant        %q", got, want)
	}
}

func TestForNSIS(t *testing.T) {
	if _, ok := For(Spec{Type: "nsis"}).(nsis); !ok {
		t.Fatal(`For(nsis) is not the NSIS driver`)
	}
}

func TestNSISUninstallWithoutUninstallerIsNoop(t *testing.T) {
	if err := For(Spec{Type: "nsis", InstallDir: t.TempDir()}).Uninstall(); err != nil {
		t.Fatalf("Uninstall = %v", err)
	}
}

// An uninstaller in a directory users can write to is never run as SYSTEM.
func TestNSISUninstallRefusesAUserWritableDirectory(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "uninstall.exe"), []byte("not really"), 0o644)
	grantEveryoneWrite(t, dir)
	err := For(Spec{Type: "nsis", InstallDir: dir}).Uninstall()
	if !errorsIs(err, ErrUserWritable) {
		t.Fatalf("Uninstall = %v, want ErrUserWritable", err)
	}
}
```

`internal/installer/acl_windows_test.go`:

```go
package installer

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

func errorsIs(err, target error) bool { return errors.Is(err, target) }

// grantEveryoneWrite adds an explicit "Everyone: full control" ACE to path.
// No elevation needed: the test owns its temp directory.
func grantEveryoneWrite(t *testing.T, path string) {
	t.Helper()
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	current, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(everyone),
		},
	}}, current)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}

// A per-user temp directory grants write only to its owner, SYSTEM and
// Administrators.
func TestCheckNotUserWritableAcceptsAPrivateDirectory(t *testing.T) {
	if err := CheckNotUserWritable(t.TempDir()); err != nil {
		t.Fatalf("CheckNotUserWritable = %v", err)
	}
}

func TestCheckNotUserWritableRejectsEveryoneWrite(t *testing.T) {
	dir := t.TempDir()
	grantEveryoneWrite(t, dir)
	if err := CheckNotUserWritable(dir); !errors.Is(err, ErrUserWritable) {
		t.Fatalf("CheckNotUserWritable = %v, want ErrUserWritable", err)
	}
}
```

- [ ] **Step 2: Eseguire i test e verificare che falliscano**

Run: `go test ./internal/installer/`
Expected: FAIL, `undefined: nsisCmdLine`, `undefined: CheckNotUserWritable`.

- [ ] **Step 3: Implementare `acl_windows.go`**

```go
package installer

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ErrUserWritable is wrapped when an uninstaller, or the directory it lives
// in, can be written by non-administrators: running it as SYSTEM would let
// any user who replaced it run code as SYSTEM.
var ErrUserWritable = errors.New("writable by non-administrators")

// writeRights are the rights that let a principal replace or alter a file.
// (If the compiler rejects mixing these constants with ace.Mask, wrap the
// expression in windows.ACCESS_MASK(...).)
const writeRights = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.WRITE_DAC |
	windows.WRITE_OWNER | windows.GENERIC_WRITE | windows.GENERIC_ALL | windows.DELETE

var untrustedSIDs = []struct {
	kind windows.WELL_KNOWN_SID_TYPE
	name string
}{
	{windows.WinBuiltinUsersSid, "Users"},
	{windows.WinAuthenticatedUserSid, "Authenticated Users"},
	{windows.WinWorldSid, "Everyone"},
}

// CheckNotUserWritable reports ErrUserWritable when any path's DACL grants a
// write right to Users, Authenticated Users or Everyone, or has no DACL at all.
func CheckNotUserWritable(paths ...string) error {
	for _, path := range paths {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return fmt.Errorf("read the ACL of %s: %w", path, err)
		}
		dacl, _, err := sd.DACL()
		if err != nil {
			return fmt.Errorf("read the DACL of %s: %w", path, err)
		}
		if dacl == nil {
			return fmt.Errorf("%s has a NULL DACL: %w", path, ErrUserWritable)
		}
		for i := uint32(0); i < uint32(dacl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, i, &ace); err != nil {
				return fmt.Errorf("read ACE %d of %s: %w", i, path, err)
			}
			if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
				continue
			}
			if ace.Mask&writeRights == 0 {
				continue
			}
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			for _, u := range untrustedSIDs {
				want, err := windows.CreateWellKnownSid(u.kind)
				if err != nil {
					return err
				}
				if sid.Equals(want) {
					return fmt.Errorf("%s grants write access to %s: %w", path, u.name, ErrUserWritable)
				}
			}
		}
	}
	return nil
}
```

- [ ] **Step 4: Implementare `nsis.go` e aggiornare `For` e `inno.Uninstall`**

```go
package installer

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// nsis drives an NSIS setup and its uninstall.exe.
type nsis struct{ s Spec }

// Install runs "setup.exe /S /D=<InstallDir>". /D is passed every time so the
// setup installs exactly where detection looks. NSIS writes no log of its
// own; the exit code is all there is, and the verification that follows is
// what decides.
func (d nsis) Install(setupPath, _ string) error {
	return runSilent(rawCommand(setupPath, "/S /D="+d.s.InstallDir), "")
}

// Uninstall runs "uninstall.exe /S _?=<InstallDir>". Without _?= the NSIS
// uninstaller copies itself to %TEMP%, relaunches from there and exits at
// once, so the caller would start the reinstall while the uninstall is still
// running. With it, the uninstaller runs in place and is not deleted - the
// reinstall overwrites it.
func (d nsis) Uninstall() error {
	exe := filepath.Join(d.s.InstallDir, "uninstall.exe")
	if _, err := os.Stat(exe); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := CheckNotUserWritable(d.s.InstallDir, exe); err != nil {
		return fmt.Errorf("not running %s: %w", exe, err)
	}
	return runSilent(rawCommand(exe, "/S _?="+d.s.InstallDir), "")
}

// rawCommand builds a command whose command line is exe (quoted when needed)
// followed by tail verbatim - see nsisCmdLine.
func rawCommand(exe, tail string) *exec.Cmd {
	cmd := exec.Command(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: nsisCmdLine(exe, tail)}
	return cmd
}

// nsisCmdLine is exe, quoted only if it needs it, then tail untouched: NSIS
// takes /D= and _?= raw up to the end of the line, quotes included.
func nsisCmdLine(exe, tail string) string {
	return syscall.EscapeArg(exe) + " " + tail
}
```

In `installer.go`, `For`:

```go
	switch s.Type {
	case "nsis":
		return nsis{s}
	default:
		return inno{s}
	}
```

In `inno.Uninstall`, prima di `runSilent`:

```go
	if err := CheckNotUserWritable(d.s.InstallDir, matches[0]); err != nil {
		return fmt.Errorf("not running %s: %w", matches[0], err)
	}
```

(Per EMLy il chiamante registra già l'errore dell'uninstall come Warn e prosegue col reinstall — comportamento best-effort invariato.)

- [ ] **Step 5: Eseguire i test**

Run: `go test ./internal/installer/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/installer
git commit -m "feat(installer): NSIS driver and refuse user-writable uninstallers"
```

---

## Task 8: Release vuota come risposta definitiva

**Files:**
- Modify: `internal/manifest/manifest.go`
- Modify: `internal/source/resolver.go`
- Test: `internal/manifest/manifest_test.go`, `internal/source/resolver_test.go` (creare se assente)

**Interfaces:**
- Produces: `var manifest.ErrNoRelease`, restituito (wrapped) da `Parse` quando mancano versione o download stable, e da `ChannelVersion` quando il canale non ha target. Il resolver smette di ritentare il primario su `ErrNoRelease`, come su `ErrNotFound`.

- [ ] **Step 1: Scrivere i test che falliscono**

In `internal/manifest/manifest_test.go`:

```go
// What the API serves for a registered product with nothing published.
func TestParseEmptyReleaseIsErrNoRelease(t *testing.T) {
	_, err := Parse([]byte(`{"stableVersion":"","stableDownload":"","isCritical":false,"sha256Checksums":{},"releaseNotes":{}}`))
	if !errors.Is(err, ErrNoRelease) {
		t.Fatalf("Parse = %v, want ErrNoRelease", err)
	}
}

func TestChannelVersionWithoutBetaIsErrNoRelease(t *testing.T) {
	m := &Manifest{StableVersion: "1.0.0", StableDownload: "x"}
	if _, _, err := m.ChannelVersion("beta"); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("ChannelVersion(beta) = %v, want ErrNoRelease", err)
	}
}
```

In `internal/source/resolver_test.go`:

```go
package source

import (
	"context"
	"fmt"
	"testing"

	"emlyupdater/internal/manifest"
)

type countingSource struct {
	calls int
	err   error
}

func (s *countingSource) Name() string { return "counting" }
func (s *countingSource) FetchManifest(context.Context) (*manifest.Manifest, error) {
	s.calls++
	return nil, s.err
}
func (s *countingSource) ResolveTarget(*manifest.Manifest, string) (manifest.Target, error) {
	return manifest.Target{}, nil
}
func (s *countingSource) FetchSetup(context.Context, manifest.Target, string) error { return nil }

// "Nothing published" does not change if asked again: one request, no backoff.
func TestResolverDoesNotRetryANoRelease(t *testing.T) {
	src := &countingSource{err: fmt.Errorf("x: %w", manifest.ErrNoRelease)}
	r := &Resolver{Primary: src, Attempts: 3}
	if _, _, err := r.Resolve(context.Background()); !errorsIsNoRelease(err) {
		t.Fatalf("Resolve = %v", err)
	}
	if src.calls != 1 {
		t.Fatalf("primary asked %d times, want 1", src.calls)
	}
}
```

con in coda:

```go
func errorsIsNoRelease(err error) bool { return errors.Is(err, manifest.ErrNoRelease) }
```

(aggiungere `"errors"` agli import).

- [ ] **Step 2: Eseguire i test e verificare che falliscano**

Run: `go test ./internal/manifest/ ./internal/source/ -run "NoRelease"`
Expected: FAIL, `undefined: ErrNoRelease`.

- [ ] **Step 3: Implementare**

`manifest.go`:

```go
// ErrNoRelease reports a manifest with nothing published for the requested
// channel - what the API answers for a product that exists but has no
// release yet. For a product other than EMLy it means "no update", not a
// failure.
var ErrNoRelease = errors.New("no release published")
```

In `Parse`: `return nil, fmt.Errorf("invalid manifest: missing stable version or download: %w", ErrNoRelease)`.
In `ChannelVersion`, i due `fmt.Errorf("manifest has no target for channel %q", channel)` diventano `fmt.Errorf("manifest has no target for channel %q: %w", channel, ErrNoRelease)`.

`resolver.go`, nel ciclo del primario:

```go
		// A 404, or a manifest with nothing published, is a definitive
		// answer, not a hiccup: asking the same question again cannot
		// change it.
		if errors.Is(err, ErrNotFound) || errors.Is(err, manifest.ErrNoRelease) {
```

- [ ] **Step 4: Eseguire i test**

Run: `go test ./internal/manifest/ ./internal/source/ ./internal/service/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/manifest internal/source
git commit -m "feat(manifest): ErrNoRelease for manifests with nothing published"
```

---

## Task 9: Sezione `products` nella policy + fixture

**Files:**
- Create: `internal/policy/products.go`
- Modify: `internal/policy/document.go` (campo, `PatchableSections`), `internal/policy/parse.go` (`complete`, `validateDocument`)
- Modify: `internal/policy/policy_test.go` (harness `effective`: campo opzionale)
- Create fixture: `testdata/remoteconfig/valid/products.json`, `testdata/remoteconfig/invalid/products-*.json` + `.problems.json`, `testdata/remoteconfig/effective/products-site-disable.json`

**Interfaces:**
- Produces:
  - `type ProductSettings struct { Enabled bool; Name, Channel, InstallDir, ExeName string; InstallWhenAbsent bool; Detect []ProductDetect; Installer ProductInstaller }` con tag JSON `enabled,name,channel,installDir,exeName,installWhenAbsent,detect,installer`
  - `type ProductDetect struct { Type, Path, Section, Key string }` (`type,path,section,key`)
  - `type ProductInstaller struct { Type string; CleanReinstall bool }` (`type,cleanReinstall`)
  - `Document.Products map[string]ProductSettings` (`json:"products"`)
  - `func validateProducts(ps map[string]ProductSettings, add func(path, msg string))`

- [ ] **Step 1: Scrivere le fixture (i test esistenti le scoprono da soli)**

`testdata/remoteconfig/valid/products.json`:

```json
{
  "schemaVersion": 1,
  "revision": 1,
  "generatedAt": "2026-09-30T00:00:00Z",
  "servers": { "srv-a": "https://a.example.com" },
  "defaultServer": "srv-a",
  "products": {
    "3g-rocketchat": {
      "enabled": true,
      "name": "3g-RocketChat",
      "channel": "stable",
      "installDir": "C:\\3gIT\\3g-RocketChat",
      "exeName": "3g-RocketChat.exe",
      "installWhenAbsent": false,
      "detect": [
        { "type": "file", "path": "version.txt" },
        { "type": "ini", "path": "config.ini", "section": "app", "key": "version" },
        { "type": "exe", "path": "3g-RocketChat.exe" }
      ],
      "installer": { "type": "nsis", "cleanReinstall": false }
    }
  }
}
```

Fixture invalide: ognuna è `valid/products.json` con **una sola** modifica; il `.problems.json` elenca esattamente i percorsi attesi (il test richiede che non ne compaiano altri). Base comune da copiare, poi applicare la modifica indicata:

| file | modifica rispetto a `valid/products.json` | `.problems.json` |
|---|---|---|
| `products-emly.json` | chiave `"3g-rocketchat"` rinominata `"emly"` | `["/products/emly"]` |
| `products-reserved-slug.json` | chiave rinominata `"updater"` | `["/products/updater"]` |
| `products-bad-slug.json` | chiave rinominata `"RocketChat"` | `["/products/RocketChat"]` |
| `products-no-name.json` | `"name": ""` | `["/products/3g-rocketchat/name"]` |
| `products-bad-channel.json` | `"channel": "nightly"` | `["/products/3g-rocketchat/channel"]` |
| `products-relative-installdir.json` | `"installDir": "3gIT\\3g-RocketChat"` | `["/products/3g-rocketchat/installDir"]` |
| `products-bad-exe-name.json` | `"exeName": "bin\\3g-RocketChat.exe"` | `["/products/3g-rocketchat/exeName"]` |
| `products-no-detect.json` | `"detect": []` | `["/products/3g-rocketchat/detect"]` |
| `products-absolute-detect-path.json` | primo detect `"path": "C:\\Windows\\win.ini"` | `["/products/3g-rocketchat/detect/0/path"]` |
| `products-dotdot-detect-path.json` | primo detect `"path": "..\\other\\version.txt"` | `["/products/3g-rocketchat/detect/0/path"]` |
| `products-unknown-detect-type.json` | primo detect `"type": "registry"` | `["/products/3g-rocketchat/detect/0/type"]` |
| `products-ini-without-key.json` | secondo detect senza `"key"` | `["/products/3g-rocketchat/detect/1/key"]` |
| `products-unknown-installer.json` | `"installer": { "type": "msi" }` | `["/products/3g-rocketchat/installer/type"]` |
| `override-products-invalid.json` | aggiungere `"overrides": [{ "id": "bad", "match": { "all": true }, "patch": { "products": { "3g-rocketchat": { "installer": { "type": "msi" } } } } }]` | `["/overrides/0/patch/products/3g-rocketchat/installer/type"]` |

`testdata/remoteconfig/effective/products-site-disable.json`:

```json
{
  "document": {
    "schemaVersion": 1,
    "revision": 2,
    "generatedAt": "2026-09-30T00:00:00Z",
    "servers": { "srv-cb": "http://172.16.96.73:8080" },
    "defaultServer": "srv-cb",
    "dcLookupMap": {
      "DC-RM2": { "internalSubnets": ["172.16.96.0/24"], "baseServer": "srv-cb" }
    },
    "products": {
      "3g-rocketchat": {
        "enabled": true,
        "name": "3g-RocketChat",
        "installDir": "C:\\3gIT\\3g-RocketChat",
        "exeName": "3g-RocketChat.exe",
        "detect": [{ "type": "file", "path": "version.txt" }],
        "installer": { "type": "nsis" }
      }
    },
    "overrides": [
      {
        "id": "rm2-no-rocketchat",
        "match": { "dcs": ["DC-RM2"] },
        "patch": { "products": { "3g-rocketchat": { "enabled": false } } }
      }
    ]
  },
  "host": {
    "hwid": "9A3F1C77-0000-0000-0000-000000000001",
    "hostname": "RM095",
    "dc": "DC-RM2",
    "ips": ["172.16.96.41"],
    "domain": "tregcc.local",
    "now": "2026-09-30T09:00:00Z"
  },
  "expect": {
    "applied": ["rm2-no-rocketchat"],
    "site": "DC-RM2",
    "chain": ["srv-cb"],
    "updaterEnabled": true,
    "pollIntervalMinutes": 60,
    "channelOverride": "",
    "loggingLevel": "info",
    "whitelisted": false,
    "productsEnabled": { "3g-rocketchat": false }
  }
}
```

Prima di committare: verificare `pollIntervalMinutes` e `loggingLevel` contro `testdata/remoteconfig/defaults.json` e correggere i due valori se i default sono diversi.

In `policy_test.go`, `effectiveFixture.Expect`, aggiungere:

```go
		ProductsEnabled map[string]bool `json:"productsEnabled"`
```

e in fondo al sub-test di `TestEffectiveFixtures`:

```go
			for slug, want := range fx.Expect.ProductsEnabled {
				if got := eff.Doc.Products[slug].Enabled; got != want {
					t.Errorf("products[%s].enabled = %v, want %v", slug, got, want)
				}
			}
```

- [ ] **Step 2: Eseguire i test e verificare che falliscano**

Run: `go test ./internal/policy/`
Expected: FAIL — le fixture invalide vengono accettate (`products` ignorato), l'override su `products` è rifiutato come "not a patchable section", `eff.Doc.Products` non esiste.

- [ ] **Step 3: Implementare**

`document.go`: nella struct `Document`, dopo `ClientWS`:

```go
	// Products lists the products other than EMLy this agent updates, keyed
	// by slug (spec 2026-09-30-multi-product-agent-design.md §4). EMLy is
	// built in and may not appear here. An object, not an array, so an
	// override can merge-patch one product.
	//
	// Agents <= 1.7.x reject a document whose override patches "products"
	// (not a patchable section for them): no override may touch it until the
	// whole fleet runs >= 1.8.0. A global "products" is ignored by them.
	Products map[string]ProductSettings `json:"products"`
```

`PatchableSections`: aggiungere `"products"`.

`parse.go`, `complete`: aggiungere `"products"` alla lista delle chiavi copiate così come sono:

```go
	for _, k := range []string{"schemaVersion", "revision", "generatedAt", "servers", "defaultServer", "dcLookupMap", "hostIntegrity", "products"} {
```

`validateDocument`, prima di `sort.SliceStable`:

```go
	validateProducts(d.Products, add)
```

`internal/policy/products.go`:

```go
package policy

import (
	"maps"
	"regexp"
	"slices"
	"strings"
)

// ProductSettings is one entry of the document's "products" section.
type ProductSettings struct {
	Enabled           bool             `json:"enabled"`
	Name              string           `json:"name"`
	Channel           string           `json:"channel"`
	InstallDir        string           `json:"installDir"`
	ExeName           string           `json:"exeName"`
	InstallWhenAbsent bool             `json:"installWhenAbsent"`
	Detect            []ProductDetect  `json:"detect"`
	Installer         ProductInstaller `json:"installer"`
}

// ProductDetect is one link of a product's version detection chain.
type ProductDetect struct {
	Type    string `json:"type"`
	Path    string `json:"path"`
	Section string `json:"section"`
	Key     string `json:"key"`
}

// ProductInstaller selects the setup driver.
type ProductInstaller struct {
	Type           string `json:"type"`
	CleanReinstall bool   `json:"cleanReinstall"`
}

// productSlug is the API's slug rule (emly-go-api internal/productreg).
var productSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,19}$`)

// reservedProductSlugs: the API's reserved slugs, plus "emly", which is built
// into the agent and configured elsewhere.
var reservedProductSlugs = []string{"emly", "updater", "all", "manifest", "releases", "download", "products"}

var windowsAbsPath = regexp.MustCompile(`^[A-Za-z]:\\`)

func hasDotDot(path string) bool {
	return slices.Contains(strings.FieldsFunc(path, func(r rune) bool { return r == '\\' || r == '/' }), "..")
}

func validateProducts(ps map[string]ProductSettings, add func(path, msg string)) {
	for _, slug := range slices.Sorted(maps.Keys(ps)) {
		p, base := ps[slug], "/products/"+slug
		if !productSlug.MatchString(slug) {
			add(base, "not a valid product slug (^[a-z0-9][a-z0-9-]{0,19}$)")
			continue
		}
		if slices.Contains(reservedProductSlugs, slug) {
			add(base, "reserved slug (emly is built in and configured elsewhere)")
			continue
		}
		if name := strings.TrimSpace(p.Name); name == "" || len(name) > 64 {
			add(base+"/name", "required, at most 64 characters")
		}
		switch p.Channel {
		case "", "stable", "beta":
		default:
			add(base+"/channel", "must be stable or beta")
		}
		if !windowsAbsPath.MatchString(p.InstallDir) || hasDotDot(p.InstallDir) {
			add(base+"/installDir", `must be an absolute Windows path (X:\...) without ".."`)
		}
		if p.ExeName == "" || strings.ContainsAny(p.ExeName, `\/:`) || !strings.HasSuffix(strings.ToLower(p.ExeName), ".exe") {
			add(base+"/exeName", "must be a file name ending in .exe")
		}
		if len(p.Detect) < 1 || len(p.Detect) > 5 {
			add(base+"/detect", "must list 1 to 5 sources")
		}
		for i, d := range p.Detect {
			dp := base + "/detect/" + strconv.Itoa(i)
			switch d.Type {
			case "ini", "file", "exe":
			default:
				add(dp+"/type", "must be ini, file or exe")
			}
			if d.Path == "" || windowsAbsPath.MatchString(d.Path) || strings.HasPrefix(d.Path, `\`) ||
				strings.HasPrefix(d.Path, "/") || hasDotDot(d.Path) {
				add(dp+"/path", `must be relative to installDir, without ".."`)
			}
			if d.Type == "ini" {
				if d.Section == "" {
					add(dp+"/section", "required for ini")
				}
				if d.Key == "" {
					add(dp+"/key", "required for ini")
				}
			}
		}
		switch p.Installer.Type {
		case "nsis", "inno":
		default:
			add(base+"/installer/type", "must be nsis or inno")
		}
	}
}
```

(aggiungere `"strconv"` agli import di `products.go`.)

- [ ] **Step 4: Eseguire i test**

Run: `go test ./internal/policy/ -v -run "Fixtures"`
Expected: PASS su `valid/products.json`, tutte le `invalid/products-*`, `override-products-invalid.json` e `effective/products-site-disable.json`.

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 5: Esempio di documento**

In `docs/remote-config.example.json` aggiungere la sezione `products` di `valid/products.json` (stesso contenuto).

- [ ] **Step 6: Commit**

```bash
git add internal/policy testdata/remoteconfig docs/remote-config.example.json
git commit -m "feat(policy): products section with shared validation fixtures"
```

---

## Task 10: Giro prodotti nel ciclo

**Files:**
- Create: `internal/service/products.go`
- Modify: `internal/service/service.go` (`Cycle`), `internal/service/product.go` (non-disponibilità, tentativi, gave-up)
- Test: `internal/service/products_test.go`

**Interfaces:**
- Consumes: Task 1–9. In particolare `policy.ProductSettings`, `manifest.ErrNoRelease`, `source.ErrNotFound`, `fakeDriver` (Task 5).
- Produces:
  - `func productFromPolicy(slug string, s policy.ProductSettings) *product.Product`
  - `func (u *Updater) documentProducts(cyc *cycleState, enabledOnly bool) []*product.Product` (ordinati per slug)
  - `func (u *Updater) cycleProducts(cyc *cycleState) []*product.Product` (abilitati + EMLy per ultimo)
  - `const productUnavailableFor = 6 * time.Hour`, `const maxProductAttempts = 3`
  - campo `unavailableUntil map[string]time.Time` su `Updater`
  - `func (u *Updater) markUnavailable(p *product.Product, err error) bool`

- [ ] **Step 1: Scrivere i test che falliscono**

`internal/service/products_test.go`:

```go
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"emlyupdater/internal/config"
	"emlyupdater/internal/download"
	"emlyupdater/internal/installer"
	"emlyupdater/internal/notify"
	"emlyupdater/internal/policy"
	"emlyupdater/internal/product"
	"emlyupdater/internal/state"
)

const rcSlug = "3g-rocketchat"

var rcSetup = []byte("rocketchat-setup-bytes")

// productServer serves EMLy's manifest at the installed version (EMLy is a
// no-op) and RocketChat's manifest and setup. rcManifest "" answers 404.
type productServer struct {
	*httptest.Server
	mu              sync.Mutex
	rcManifest      string
	rcManifestCalls atomic.Int32
	emlyVersion     string
}

func newProductServer(t *testing.T) *productServer {
	ps := &productServer{emlyVersion: "2.0.0"}
	sum := sha256.Sum256(rcSetup)
	ps.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ps.mu.Lock()
		defer ps.mu.Unlock()
		switch r.URL.Path {
		case "/v2/updates/manifest":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"stableVersion":"` + ps.emlyVersion + `","stableDownload":"` + ps.URL +
				`/v2/updates/releases/x/download","sha256Checksums":{"` + ps.emlyVersion + `":"00"}}`))
		case "/v2/updates/" + rcSlug + "/manifest":
			ps.rcManifestCalls.Add(1)
			if ps.rcManifest == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(ps.rcManifest))
		case "/v2/updates/" + rcSlug + "/releases/1.1.0/download":
			_, _ = w.Write(rcSetup)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ps.Close)
	ps.rcManifest = `{"stableVersion":"1.1.0","stableDownload":"` + ps.URL + `/v2/updates/` + rcSlug +
		`/releases/1.1.0/download","sha256Checksums":{"1.1.0":"` + hex.EncodeToString(sum[:]) + `"}}`
	return ps
}

func (ps *productServer) setRC(body string) { ps.mu.Lock(); ps.rcManifest = body; ps.mu.Unlock() }

type productHarness struct {
	u       *Updater
	rcDir   string
	driver  *fakeDriver
	running map[string]bool
	boxes   []notify.Message
}

// newProductHarness: EMLy installed at the manifest's version, RocketChat
// installed at 1.0.0 (version.txt) and enabled in the document.
func newProductHarness(t *testing.T, srv *productServer) *productHarness {
	t.Helper()
	cfg := internalCfg(t, config.SourceInternal)
	cfg.InternalManifestURL = srv.URL + "/v2/updates/manifest"
	cfg.ExternalManifestURL = srv.URL + "/v2/updates/manifest"
	emlyDir := t.TempDir()
	cfg.EMLyConfigFile = filepath.Join(emlyDir, "config.ini")
	_ = os.WriteFile(cfg.EMLyConfigFile, []byte("[EMLy]\nGUI_SEMVER="+srv.emlyVersion+"\n"), 0o644)

	h := &productHarness{rcDir: t.TempDir(), running: map[string]bool{}}
	_ = os.WriteFile(filepath.Join(h.rcDir, "version.txt"), []byte("1.0.0\r\n"), 0o644)
	h.driver = &fakeDriver{dir: h.rcDir}

	u := newTestUpdater(t, cfg, dcNamed("DC-RM2"), ipsOf("172.16.96.50"))
	u.Store = &state.Store{Path: filepath.Join(t.TempDir(), "state.json")}
	u.Downloads = &download.Manager{Dir: t.TempDir()}
	u.runningFn = func(exe string) bool { return h.running[exe] }
	u.driverFn = func(p *product.Product) installer.Driver {
		if p.Legacy {
			t.Fatalf("EMLy setup must not run in these tests")
		}
		return h.driver
	}
	u.notifyBoxFn = func(m notify.Message, _ int) bool { h.boxes = append(h.boxes, m); return true }
	u.toastFn = func(string, string, string) bool { return true }

	snap := u.Policy.Current()
	snap.Parsed.Global.Updater.Resolver = policy.ResolverSettings{Attempts: 1}
	snap.Parsed.Global.Updater.SelfUpdate.Enabled = false
	snap.Parsed.Global.Products = map[string]policy.ProductSettings{rcSlug: {
		Enabled: true, Name: "3g-RocketChat", InstallDir: h.rcDir, ExeName: "3g-RocketChat.exe",
		Detect:    []policy.ProductDetect{{Type: "file", Path: "version.txt"}},
		Installer: policy.ProductInstaller{Type: "nsis"},
	}}
	h.u = u
	return h
}

func (h *productHarness) cycle(t *testing.T) error {
	t.Helper()
	cyc := h.u.beginCycle(context.Background(), true)
	return h.u.Cycle(context.Background(), cyc)
}

func TestCycleUpdatesAProduct(t *testing.T) {
	h := newProductHarness(t, newProductServer(t))
	if err := h.cycle(t); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	if len(h.driver.installs) != 1 || h.driver.installs[0] != "1.1.0" {
		t.Fatalf("installs = %v, want [1.1.0]", h.driver.installs)
	}
	if p, _ := h.u.Store.PendingFor(rcSlug); p != nil {
		t.Errorf("pending not cleared: %+v", p)
	}
}

// A product kept open never blocks the cycle, and the user is told once per
// version, not once per cycle.
func TestRunningProductDefersWithOneNotification(t *testing.T) {
	h := newProductHarness(t, newProductServer(t))
	h.running["3g-RocketChat.exe"] = true
	for i := 0; i < 3; i++ {
		if err := h.cycle(t); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}
	if len(h.driver.installs) != 0 {
		t.Fatalf("installed while running: %v", h.driver.installs)
	}
	if len(h.boxes) != 1 {
		t.Fatalf("notifications = %d, want 1", len(h.boxes))
	}
	if p, _ := h.u.Store.PendingFor(rcSlug); p == nil || p.Version != "1.1.0" {
		t.Fatalf("pending = %+v, want 1.1.0 kept", p)
	}

	h.running["3g-RocketChat.exe"] = false
	if err := h.cycle(t); err != nil {
		t.Fatal(err)
	}
	if len(h.driver.installs) != 1 {
		t.Fatalf("installs after close = %v", h.driver.installs)
	}
}

// EMLy is last in the round: a product before it never waits on EMLy.
func TestCycleProductsOrderPutsEMLyLast(t *testing.T) {
	h := newProductHarness(t, newProductServer(t))
	snap := h.u.Policy.Current()
	snap.Parsed.Global.Products["aaa"] = snap.Parsed.Global.Products[rcSlug]
	list := h.u.cycleProducts(h.u.beginCycle(context.Background(), true))
	var slugs []string
	for _, p := range list {
		slugs = append(slugs, p.Slug)
	}
	if got := strings.Join(slugs, ","); got != "3g-rocketchat,aaa,emly" {
		t.Fatalf("order = %s", got)
	}
}

func TestDisabledProductIsNotPolled(t *testing.T) {
	srv := newProductServer(t)
	h := newProductHarness(t, srv)
	s := h.u.Policy.Current().Parsed.Global.Products[rcSlug]
	s.Enabled = false
	h.u.Policy.Current().Parsed.Global.Products[rcSlug] = s
	_ = h.cycle(t)
	if srv.rcManifestCalls.Load() != 0 {
		t.Fatalf("disabled product polled %d times", srv.rcManifestCalls.Load())
	}
}

// Absent and installWhenAbsent false: not polled at all.
func TestAbsentProductIsLeftAlone(t *testing.T) {
	srv := newProductServer(t)
	h := newProductHarness(t, srv)
	_ = os.Remove(filepath.Join(h.rcDir, "version.txt"))
	_ = h.cycle(t)
	if srv.rcManifestCalls.Load() != 0 || len(h.driver.installs) != 0 {
		t.Fatalf("absent product touched: polls=%d installs=%v", srv.rcManifestCalls.Load(), h.driver.installs)
	}
}

func TestNewerInstalledVersionIsNotDowngraded(t *testing.T) {
	h := newProductHarness(t, newProductServer(t))
	_ = os.WriteFile(filepath.Join(h.rcDir, "version.txt"), []byte("2.0.0"), 0o644)
	if err := h.cycle(t); err != nil {
		t.Fatal(err)
	}
	if len(h.driver.installs) != 0 {
		t.Fatalf("downgraded: %v", h.driver.installs)
	}
}

// 404 and an empty release both mean "no update"; the product is not asked
// again for productUnavailableFor.
func TestUnavailableProductIsBackedOff(t *testing.T) {
	for name, body := range map[string]string{
		"404":           "",
		"empty release": `{"stableVersion":"","stableDownload":"","sha256Checksums":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := newProductServer(t)
			srv.setRC(body)
			h := newProductHarness(t, srv)
			now := time.Now()
			h.u.nowFn = func() time.Time { return now }
			if err := h.cycle(t); err != nil {
				t.Fatalf("an unavailable product failed the cycle: %v", err)
			}
			calls := srv.rcManifestCalls.Load()
			_ = h.cycle(t)
			if srv.rcManifestCalls.Load() != calls {
				t.Fatalf("asked again within the back-off window")
			}
			now = now.Add(productUnavailableFor + time.Minute)
			_ = h.cycle(t)
			if srv.rcManifestCalls.Load() == calls {
				t.Fatalf("not asked again after the back-off window")
			}
		})
	}
}

// A failing setup is tried maxProductAttempts times for the same version,
// then left alone until the manifest offers another one. No uninstall runs:
// cleanReinstall is off by default.
func TestFailingProductGivesUpAfterThreeAttempts(t *testing.T) {
	srv := newProductServer(t)
	h := newProductHarness(t, srv)
	h.driver.failInstall = true
	for i := 0; i < 5; i++ {
		_ = h.cycle(t)
	}
	// Two setup runs per attempt (first + retry), three attempts.
	if len(h.driver.installs) != 2*maxProductAttempts {
		t.Fatalf("setup runs = %d, want %d", len(h.driver.installs), 2*maxProductAttempts)
	}
	if h.driver.uninstalls != 0 {
		t.Fatalf("uninstall ran %d times with cleanReinstall off", h.driver.uninstalls)
	}
	p, _ := h.u.Store.PendingFor(rcSlug)
	if p == nil || !p.GaveUp || p.Attempts != maxProductAttempts {
		t.Fatalf("pending = %+v, want gaveUp after %d attempts", p, maxProductAttempts)
	}
}

// A destructive command committed mid-round stops the products after it.
func TestDestructivePendingStopsTheRound(t *testing.T) {
	srv := newProductServer(t)
	h := newProductHarness(t, srv)
	h.u.destructivePending = true
	h.u.destructiveDeadline = time.Now().Add(time.Hour)
	_ = h.cycle(t)
	if srv.rcManifestCalls.Load() != 0 {
		t.Fatalf("product polled while a destructive command is pending")
	}
}
```

Aggiungere `"strings"` agli import.

- [ ] **Step 2: Eseguire i test e verificare che falliscano**

Run: `go test ./internal/service/ -run "Product|Unavailable|GivesUp|DestructivePendingStopsTheRound"`
Expected: FAIL di compilazione (`h.u.cycleProducts undefined`, `productUnavailableFor`, `maxProductAttempts`).

- [ ] **Step 3: Implementare `internal/service/products.go`**

```go
package service

import (
	"errors"
	"maps"
	"slices"
	"time"

	"emlyupdater/internal/manifest"
	"emlyupdater/internal/policy"
	"emlyupdater/internal/product"
	"emlyupdater/internal/source"
	"emlyupdater/internal/state"
)

// productUnavailableFor is how long a product whose manifest answered 404,
// or listed nothing published, is left alone (spec §5.3).
const productUnavailableFor = 6 * time.Hour

// maxProductAttempts caps the failing installs of one target version of a
// product other than EMLy (spec §6.2).
const maxProductAttempts = 3

// productFromPolicy turns a document entry into a product definition.
func productFromPolicy(slug string, s policy.ProductSettings) *product.Product {
	p := &product.Product{
		Slug: slug, Name: s.Name, InstallDir: s.InstallDir, ExeName: s.ExeName, Channel: s.Channel,
		Installer:         product.InstallerSpec{Type: s.Installer.Type, CleanReinstall: s.Installer.CleanReinstall},
		InstallWhenAbsent: s.InstallWhenAbsent,
	}
	for _, d := range s.Detect {
		p.Detect = append(p.Detect, product.VersionSource{Type: d.Type, Path: d.Path, Section: d.Section, Key: d.Key})
	}
	return p
}

// documentProducts lists the document's products, sorted by slug, from the
// cycle's effective document (overrides applied) - or, with no cycle yet,
// from the global one.
func (u *Updater) documentProducts(cyc *cycleState, enabledOnly bool) []*product.Product {
	var doc *policy.Document
	switch {
	case cyc != nil && cyc.eff != nil:
		doc = cyc.eff.Doc
	case u.Policy != nil && u.Policy.Current() != nil:
		doc = u.Policy.Current().Parsed.Global
	default:
		return nil
	}
	var out []*product.Product
	for _, slug := range slices.Sorted(maps.Keys(doc.Products)) {
		s := doc.Products[slug]
		if enabledOnly && !s.Enabled {
			continue
		}
		out = append(out, productFromPolicy(slug, s))
	}
	return out
}

// cycleProducts is this cycle's round: the enabled products, then EMLy. EMLy
// goes last because it can block (WaitForExit); a product before it never
// waits on it.
func (u *Updater) cycleProducts(cyc *cycleState) []*product.Product {
	return append(u.documentProducts(cyc, true), u.emlyProduct(cyc))
}

// markUnavailable records that p's manifest has nothing for this machine and
// reports whether err was such an answer.
func (u *Updater) markUnavailable(p *product.Product, err error) bool {
	if !errors.Is(err, source.ErrNotFound) && !errors.Is(err, manifest.ErrNoRelease) {
		return false
	}
	if u.unavailableUntil == nil {
		u.unavailableUntil = map[string]time.Time{}
	}
	u.unavailableUntil[p.Slug] = u.clock().Add(productUnavailableFor)
	u.Log.Info("no release available for this product, not asking again for a while",
		"product", p.Slug, "for", productUnavailableFor.String(), "reason", err.Error())
	return true
}

func (u *Updater) unavailable(p *product.Product) bool {
	until, ok := u.unavailableUntil[p.Slug]
	if !ok {
		return false
	}
	if u.clock().Before(until) {
		return true
	}
	delete(u.unavailableUntil, p.Slug)
	return false
}

// recordFailedAttempt counts one failed install of pend's version and gives
// the version up at maxProductAttempts.
func (u *Updater) recordFailedAttempt(p *product.Product, pend *state.Pending) {
	pend.Attempts++
	if pend.Attempts >= maxProductAttempts {
		pend.GaveUp = true
		u.Log.Error("giving up on this version after repeated install failures; waiting for a different release",
			"product", p.Slug, "version", pend.Version, "attempts", pend.Attempts)
	}
	if err := u.Store.SetPendingFor(p.Slug, pend); err != nil {
		u.Log.Warn("failed to persist the attempt counter", "product", p.Slug, "error", err.Error())
	}
}
```

Campo in `Updater` (accanto a `productDownloads`):

```go
	// unavailableUntil backs off products whose manifest had nothing
	// (markUnavailable). Poll goroutine only.
	unavailableUntil map[string]time.Time
```

- [ ] **Step 4: `Cycle` sul giro prodotti**

In `service.go`, la riga finale di `Cycle` introdotta nel Task 5 diventa:

```go
	// One product at a time, EMLy last: never two downloads or two setups
	// at once on this machine (MPLS, and beginInstall).
	for _, p := range u.cycleProducts(cyc) {
		if u.destructivePendingNow() {
			u.logDestructiveSkipOnce()
			return nil
		}
		if err := u.productCycle(ctx, cyc, p); err != nil {
			if p.Legacy {
				return err // EMLy is last: its error stays the cycle's, as before
			}
			u.Log.Warn("product update failed this cycle", "product", p.Slug, "error", err.Error())
		}
	}
	return nil
}
```

- [ ] **Step 5: Non-disponibilità, gave-up e tentativi in `productCycle`/`installProduct`**

In `productCycle`, subito dopo `if ps.Fresh && !p.InstallWhenAbsent { return nil }`:

```go
	if !p.Legacy && u.unavailable(p) {
		return nil
	}
```

Nel ramo d'errore di `resolveProductTarget`:

```go
	if err != nil {
		if !p.Legacy && u.markUnavailable(p, err) {
			return nil
		}
		if p.Legacy {
			u.notifySourcesUnreachable()
		}
		return err
	}
```

Subito dopo, prima di `manifest.Less(ps.Installed, target.Version)`:

```go
	if pend != nil && pend.GaveUp {
		if pend.Version == target.Version {
			return nil // this release already failed maxProductAttempts times
		}
		u.Log.Info("a different release is offered, retrying after an earlier give-up",
			"product", p.Slug, "gaveUpOn", pend.Version, "target", target.Version)
		_ = u.Store.ClearPendingFor(p.Slug)
	}
```

In `installProduct`, nel ramo del secondo fallimento, prima di `return err`:

```go
			if !p.Legacy {
				u.recordFailedAttempt(p, pend)
			}
```

In `newProductResolver`, dopo aver costruito `urls` dalla catena preferita, aggiungere il `defaultServer` se non c'è già (spec §5.3 — un mirror di sito vecchio risponde 404 alle route con slug):

```go
	if def := cyc.eff.Doc.DefaultServer; !slices.Contains(u.preferredChain(cyc), def) {
		if base := cyc.eff.BaseURL(def); base != "" {
			urls = append(urls, base+p.ManifestPath())
		}
	}
```

(aggiungere `"slices"` agli import di `product.go`.)

- [ ] **Step 6: Test del `defaultServer`** (in coda a `products_test.go`)

```go
// A site mirror older than multi-product answers 404 to slug routes: the
// document's defaultServer is asked before the product is deemed unavailable.
func TestProduct404OnSiteMirrorFallsBackToDefaultServer(t *testing.T) {
	mirror := newProductServer(t)
	mirror.setRC("")
	central := newProductServer(t)
	h := newProductHarness(t, mirror)
	g := h.u.Policy.Current().Parsed.Global
	g.Servers = map[string]string{"srv-site": mirror.URL, "srv-central": central.URL}
	g.DefaultServer = "srv-central"
	g.DCLookupMap = map[string]policy.Site{"DC-RM2": {InternalSubnets: []string{"172.16.96.0/24"}, BaseServer: "srv-site"}}

	if err := h.cycle(t); err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	if central.rcManifestCalls.Load() == 0 || len(h.driver.installs) != 1 {
		t.Fatalf("central asked %d times, installs %v", central.rcManifestCalls.Load(), h.driver.installs)
	}
}
```

- [ ] **Step 7: Eseguire tutta la suite**

Run: `go test ./internal/service/ -v -run "Product|Unavailable|GivesUp|DestructivePendingStopsTheRound|DefaultServer" && go test ./...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/service
git commit -m "feat(service): update document products in a sequential round, EMLy last"
```

---

## Task 11: Inventario esteso a tutti i prodotti

**Files:**
- Modify: `internal/service/service.go` (`installedProducts`)
- Test: `internal/service/service_test.go`

**Interfaces:**
- Consumes: `documentProducts(nil, false)` (Task 10), `product.Detect` (Task 1).
- Produces: `installedProducts()` stessa firma `(emlyVersion string, inventory map[string]string)`.

- [ ] **Step 1: Scrivere i test che falliscono** (in coda a `service_test.go`)

```go
// Every product of the document is reported - disabled ones too: enabled
// turns updates off, not dashboard visibility. One unreadable product voids
// the whole inventory, which is complete by definition.
func TestInstalledProductsCoversDocumentProducts(t *testing.T) {
	srv := newProductServer(t)
	h := newProductHarness(t, srv)
	s := h.u.Policy.Current().Parsed.Global.Products[rcSlug]
	s.Enabled = false
	h.u.Policy.Current().Parsed.Global.Products[rcSlug] = s

	_, inv := h.u.installedProducts()
	if inv["emly"] != "2.0.0" || inv[rcSlug] != "1.0.0" {
		t.Fatalf("inventory = %v, want emly 2.0.0 and %s 1.0.0", inv, rcSlug)
	}

	_ = os.WriteFile(filepath.Join(h.rcDir, "version.txt"), []byte("not-a-version"), 0o644)
	if _, inv := h.u.installedProducts(); inv != nil {
		t.Fatalf("inventory with an unknown product = %v, want nil", inv)
	}

	_ = os.Remove(filepath.Join(h.rcDir, "version.txt"))
	if _, inv := h.u.installedProducts(); inv == nil || len(inv) != 1 || inv["emly"] != "2.0.0" {
		t.Fatalf("inventory with the product absent = %v, want only emly", inv)
	}
}
```

- [ ] **Step 2: Eseguire il test e verificare che fallisca**

Run: `go test ./internal/service/ -run TestInstalledProductsCoversDocumentProducts`
Expected: FAIL (`inventory = map[emly:2.0.0]`).

- [ ] **Step 3: Implementare**

In `installedProducts`, dopo aver costruito la mappa con EMLy e prima del `return`:

```go
	for _, p := range u.documentProducts(u.current(), false) {
		r := product.Detect(p)
		switch r.Outcome {
		case product.Installed:
			inventory[p.Slug] = r.Version
		case product.Unknown:
			// The inventory is complete by definition: leaving this product
			// out would report it uninstalled. Send nothing instead.
			return version, nil
		}
	}
```

Aggiornare il doc comment: "inventory is the complete product list - EMLy plus every product of the document, enabled or not…". Verificare che `u.current()` gestisca un `Updater` senza ciclo (restituisce nil → `documentProducts` ripiega sul documento globale, o nil se `u.Policy` è nil come in `TestInstalledProductsForHeaders`).

- [ ] **Step 4: Eseguire i test**

Run: `go test ./internal/service/ ./internal/source/`
Expected: PASS (compreso `TestInstalledProductsForHeaders`).

- [ ] **Step 5: Commit**

```bash
git add internal/service
git commit -m "feat(service): installed-products inventory covers every document product"
```

---

## Task 12: Sottocomando `emly-updater products`

**Files:**
- Create: `internal/service/report.go`
- Modify: `main.go`
- Test: `internal/service/report_test.go`

**Interfaces:**
- Consumes: `cycleProducts`, `documentProducts`, `resolveProductState`, `resolveProductTarget`, `Store.PendingFor`.
- Produces: `func (u *Updater) ReportProducts(ctx context.Context, w io.Writer, check bool) error`

- [ ] **Step 1: Scrivere il test che fallisce**

`internal/service/report_test.go`:

```go
package service

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestReportProductsListsDetectionAndManifest(t *testing.T) {
	srv := newProductServer(t)
	h := newProductHarness(t, srv)
	var out bytes.Buffer
	if err := h.u.ReportProducts(context.Background(), &out, true); err != nil {
		t.Fatalf("ReportProducts: %v", err)
	}
	text := out.String()
	for _, want := range []string{"3g-rocketchat", "installed 1.0.0 (file:version.txt)", "1.1.0", "emly", "2.0.0"} {
		if !strings.Contains(text, want) {
			t.Errorf("report lacks %q:\n%s", want, text)
		}
	}
	if len(h.driver.installs) != 0 {
		t.Fatalf("the report installed something: %v", h.driver.installs)
	}
}
```

- [ ] **Step 2: Eseguire il test e verificare che fallisca**

Run: `go test ./internal/service/ -run TestReportProducts`
Expected: FAIL, `h.u.ReportProducts undefined`.

- [ ] **Step 3: Implementare `report.go`**

```go
package service

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"emlyupdater/internal/product"
)

// ReportProducts prints, for EMLy and every product of the effective
// document (disabled ones included): whether it is enabled, what detection
// finds, the pending entry, and - with check - the version the manifest
// offers. Read-only: it downloads and installs nothing and does not touch
// the preferred server. Backs `emly-updater products`.
func (u *Updater) ReportProducts(ctx context.Context, w io.Writer, check bool) error {
	if u.Policy == nil {
		u.initPolicy() // cached document, or the one derived from config.ini
	}
	cyc := u.beginCycle(ctx, true)
	enabled := map[string]bool{product.EMLySlug: true}
	for _, p := range u.documentProducts(cyc, true) {
		enabled[p.Slug] = true
	}
	list := append(u.documentProducts(cyc, false), u.emlyProduct(cyc))

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PRODUCT\tENABLED\tDETECTED\tPENDING\tOFFERED")
	for _, p := range list {
		detected := product.Detect(p).String()
		pending := "-"
		if pend, err := u.Store.PendingFor(p.Slug); err == nil && pend != nil {
			pending = fmt.Sprintf("%s (attempts %d, gaveUp %v)", pend.Version, pend.Attempts, pend.GaveUp)
		}
		offered := "-"
		if check {
			ps, ok := u.resolveProductState(p)
			if !ok {
				offered = "not checked (version unknown)"
			} else if _, _, target, err := u.resolveProductTarget(ctx, cyc, p, ps.Channel); err != nil {
				offered = "error: " + err.Error()
			} else {
				offered = target.Version + " (" + ps.Channel + ")"
			}
		}
		fmt.Fprintf(tw, "%s\t%v\t%s\t%s\t%s\n", p.Slug, enabled[p.Slug], detected, pending, offered)
	}
	return tw.Flush()
}
```

Nota: per EMLy `resolveProductTarget` passa da `resolveTarget` con `notePreferred=true`. Per restare in sola lettura, in `ReportProducts` usare per EMLy `u.resolveTargetWith(ctx, cyc, ps.Channel, false)`:

```go
			} else if p.Legacy {
				if _, _, target, err := u.resolveTargetWith(ctx, cyc, ps.Channel, false); err != nil {
					offered = "error: " + err.Error()
				} else {
					offered = target.Version + " (" + ps.Channel + ")"
				}
			} else if _, _, target, err := u.resolveProductTarget(ctx, cyc, p, ps.Channel); err != nil {
```

- [ ] **Step 4: Collegare `main.go`**

Nello `switch os.Args[1]`:

```go
	case "products":
		err = cmdProducts(os.Args[2:])
```

`usage()`: `"usage: %s install|uninstall|start|stop|run|products [--check]\n"`.

```go
// cmdProducts prints what the agent knows about each product on this
// machine. Read-only, so it runs beside an installed service (no singleton
// mutex); its own log goes to a temp directory, never to the service's.
func cmdProducts(args []string) error {
	fs := flag.NewFlagSet("products", flag.ContinueOnError)
	check := fs.Bool("check", false, "also ask the server which version each manifest offers")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(config.ConfigPath())
	if err != nil {
		return err
	}
	log := logging.New(filepath.Join(os.TempDir(), "emly-updater-products"), "", false)
	u := service.New(cfg, log, false)
	return u.ReportProducts(context.Background(), os.Stdout, *check)
}
```

Aggiungere a `main.go` gli import mancanti tra `context` e `path/filepath`. `ReportProducts` inizializza da sé la policy (`initPolicy`) e non avvia IPC né WS.

- [ ] **Step 5: Eseguire test e build**

Run: `go test ./internal/service/ -run TestReportProducts -v && go build -o build\bin\emly-updater.exe . && build\bin\emly-updater.exe products`
Expected: test PASS; il comando stampa la tabella con almeno la riga `emly`.

- [ ] **Step 6: Commit**

```bash
git add internal/service/report.go internal/service/report_test.go main.go
git commit -m "feat: products subcommand to inspect detection and manifests"
```

---

## Task 13: Documentazione e versione 1.8.0

**Files:**
- Modify: `AGENTS.md`, `README.md`, `versioninfo.json`
- Generated: `internal/version/version_generated.go`, `installer/installer.iss`, `resource.syso` (via `go generate`)

- [ ] **Step 1: AGENTS.md**

- Package map: aggiungere `internal/product` (definizione + rilevamento) e aggiornare `internal/installer` (driver `inno`/`nsis`, `CheckNotUserWritable`).
- *Key Conventions*, nuova voce **"Products other than EMLy"**: giro sequenziale con EMLy ultimo; `Legacy` e cosa copre; catena di rilevamento a tre esiti; `enabled` non tocca l'inventario; non bloccante quando l'app è aperta; 404/release vuota → 6 h; tetto 3 tentativi; `cleanReinstall` default false e perché (RocketChat perderebbe `config.ini` e `data\`).
- *Common Pitfalls*:
  - "Never patch `products` from an override while agents ≤ 1.7.x are in the field: they reject the whole document."
  - "Removing a product from the document drops it from the inventory, so from the dashboard: use `enabled: false`."
  - "NSIS: `/D=` and `_?=` must be last and unquoted (raw `SysProcAttr.CmdLine`); without `_?=` the uninstaller returns immediately."
  - "An uninstaller in a directory writable by Users is never run as SYSTEM (`installer.CheckNotUserWritable`)."
  - "3g-RocketChat's `config.ini` `[app] version` is not rewritten by its setup today: the chain reads `version.txt` first."
- Aggiornare la voce su `X-EMLy-InstalledProducts` scritta nel Task 0: ora include tutti i prodotti del documento.

- [ ] **Step 2: README.md**

Nel riferimento della configurazione aggiungere la sezione `products` (tabella di spec §4). Nella ricetta *Testing end-to-end without infrastructure* aggiungere: servire `/v2/updates/3g-rocketchat/manifest` e il setup, mettere `products` nel documento di test, usare `emly-updater products --check` per verificare prima di `run`.

- [ ] **Step 3: Versione**

In `versioninfo.json` impostare `StringFileInfo.FileVersion` e `ProductVersion` a `1.8.0` (e i campi numerici `FixedFileInfo` coerenti, come nei bump precedenti — vedere `git show 2e8b056 -- versioninfo.json`).

Run: `go generate ./...`
Expected: `internal/version/version_generated.go` e `installer/installer.iss` riportano 1.8.0.

- [ ] **Step 4: Verifica finale**

Run: `go vet ./... ; go test ./... ; go build -ldflags "-s -w" -o build\bin\emly-updater.exe .`
Expected: test PASS, build ok; `go vet` riporta solo il warning pre-esistente di `sessionchange.go:84`.

- [ ] **Step 5: Commit**

```bash
git add AGENTS.md README.md versioninfo.json internal/version installer/installer.iss resource.syso
git commit -m "docs: multi-product agent; bump version to 1.8.0"
```

---

## Task 14: Gemello lato API (`emly-go-api`, branch `feat/multi-product`)

Da eseguire nel repo `..\emly-go-api`. Senza questo la dashboard non può salvare un documento con `products`.

**Files (da individuare leggendo il repo):** `internal/remoteconfig/*.go` (tipi del documento, validatore, sezioni patchabili), `testdata/remoteconfig/**`, test di conformità delle fixture.

- [ ] **Step 1: Copiare le fixture**

Copiare **identiche** da questo repo: `testdata/remoteconfig/valid/products.json`, tutte le `testdata/remoteconfig/invalid/products-*.json` e `.problems.json`, `invalid/override-products-invalid.*`, `effective/products-site-disable.json`.

- [ ] **Step 2: Eseguire i test di conformità e verificare che falliscano**

Run: `go test ./internal/remoteconfig/...`
Expected: FAIL sulle fixture nuove.

- [ ] **Step 3: Implementare le stesse regole**

Leggere prima come `internal/remoteconfig` modella `clientWs` (tipo, validazione, sezione patchabile, harness `effective`) e replicare per `products` le regole di `internal/policy/products.go` di questo repo, una per una: slug (regex + riservati + `emly`), `name`, `channel`, `installDir`, `exeName`, `detect` (1–5, `type`, `path` relativo senza `..`, `section`/`key` per `ini`), `installer.type`; `products` tra le sezioni patchabili; `productsEnabled` nell'harness `effective`. I messaggi possono differire, i **percorsi** dei problemi no.

- [ ] **Step 4: Eseguire i test**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/remoteconfig testdata/remoteconfig
git commit -m "feat(remoteconfig): products section, twin of the agent validator"
```
