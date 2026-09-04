# Remote configuration via `/v2/config`

Design document — 2026-09-04

## 1. Problem

Every operational decision the updater makes today is driven by
`%ProgramData%\EMLyUpdater\config.ini`: which manifest servers exist, the
DC → subnet map that picks the internal source, the poll interval, the IPC
compatibility range, logging limits. That file is rewritten from the embedded
defaults on every upgrade (`config.Reset`), so changing any of it fleet-wide
means shipping a new updater release and waiting for the self-update to land.

Some decisions cannot wait for a release cycle at all: stopping the fleet from
installing an update that turned out to be broken, freezing a site during an
accounting closure, moving a site to a new mirror after a network change, or
turning debug logging on for one machine that keeps failing.

EMLy itself has the same need and no channel for it. The updater already runs
on every machine as LocalSystem, already talks to the API, and already exposes
an IPC pipe to EMLy. It is the natural single fetcher.

## 2. Goals

- One HTTP document, served by the same API hosts as the manifest, carries the
  policy for both the updater and EMLy.
- A machine keeps working exactly as today when the endpoint has never been
  reached, is unreachable, serves garbage, or serves an old copy.
- No policy push can leave a machine unable to reach the API again.
- Policy changes apply without a service restart, and without a release.
- Per-host, per-site, and per-group exceptions are expressed with one
  mechanism (`overrides`), not one ad-hoc field per feature.
- Every disabling or restricting setting can carry an expiry.

## 3. Non-goals

- Replacing `config.ini`. It stays, reduced to a bootstrap file (§5).
- Remote commands (force check, collect logs, reinstall), telemetry/heartbeat,
  maintenance windows, percentage rollouts. Good candidates for a later
  revision of the same document; the schema leaves room for them.
- Widening what the binary can do. The remote document can only *narrow*
  compiled-in compatibility and trust (§7.4, §12).
- Document signing. Recommended (§12) but a separate piece of work; this
  design must remain safe without it.
- EMLy's own handling of the `app` policy. This document defines what the
  updater hands to EMLy over IPC and what it means; enforcement inside EMLy is
  specified in the `emly` repo.

## 4. Three layers, one effective policy

```
 ┌──────────────────────────────────────────────────────────────┐
 │ 1. Bootstrap        config.ini   (embedded default, on disk)  │
 │    how to reach *a* server; never written at runtime          │
 ├──────────────────────────────────────────────────────────────┤
 │ 2. Policy           /v2/config → remote-config.json cache     │
 │    everything operational; validated all-or-nothing           │
 ├──────────────────────────────────────────────────────────────┤
 │ 3. Default policy   derived from the legacy [source] keys     │
 │    used only when no valid cache exists yet                   │
 └──────────────────────────────────────────────────────────────┘
                         ↓ merge in memory, per cycle
                Effective policy (global ⊕ matching overrides)
```

The two options considered and rejected:

- **Remote document overwrites `config.ini`.** The file that says *where the
  config comes from* would be rewritten by the config itself. One bad push
  (a typo in a server URL, a decommissioned host) strands the machine with no
  way back except a hands-on visit. It also fights `config.Reset`, which
  restores the embedded defaults on every upgrade, and makes `config.ini` a
  file with two writers and no single source of truth (the `SetPrimary`
  write-back is already that wart today).
- **Remote document with `config.ini` as the fallback.** Closer, but it
  implies `config.ini` keeps a full copy of the policy (`defaultMappingDCSubnets`
  and friends), so the two drift and nobody knows which one a given machine is
  running.

The chosen model: `config.ini` holds only what is needed to reach *any*
server and is read once at startup. The remote document is cached in a
separate file, atomically, and is the only thing that changes at runtime.
When there is no cache, the policy is derived from the legacy `[source]` keys
so a machine that has never seen the endpoint behaves byte-for-byte like
today's release.

## 5. Bootstrap: `config.ini` changes

New section, read by `config.Load`:

```ini
[remoteConfig]
; Abilita il download della configurazione remota (/v2/config).
; Se disattivato, l'updater usa solo le chiavi di questo file.
enabled = true

; Endpoint da cui scaricare la configurazione, in ordine di preferenza,
; separati da "|". Sono URL base: il percorso viene aggiunto dall'updater.
; Provati dopo i server della policy in cache (se presente).
endpoints = https://api.emly.ffois.it

; Percorso della rotta di configurazione, aggiunto a ogni URL base.
; Stesso prefisso del manifest (/v2/updates/manifest).
configPath = /v2/config

; Timeout in secondi per ogni tentativo. Il download della configurazione
; e' best-effort: un fallimento non ferma il ciclo di aggiornamento.
timeoutSeconds = 10
```

Kept, with their current meaning, as **bootstrap** (immutable at runtime):
`[updater]` paths and `emlyExeName`, `[source] userAgent`, `[source] xApiKey`,
`[ipc]`, `[fileAssociations]`.

Kept as **legacy fallback only** (§7.1): `[source] primary`,
`externalManifestURL`, `internalManifestURL`, `bkInternManifestURL`,
`defaultMappingDCSubnets`, `dcLookupRetry*`; `[updater] pollIntervalMinutes`,
`channelOverride`; `[criticalUpdate]`; `[selfUpdate] enabled`;
`[certificate] enabled`. They seed the default policy and are ignored once a
valid remote document is cached. Their comments in `config.default.ini` must
say so.

Removed behaviour: `applySourcePolicy` no longer writes `primary` back to
`config.ini` (`config.SetPrimary` goes away). The source decision is a runtime
fact reported in the log, not a config value.

## 6. The endpoint

`GET <server><configPath>` — e.g. `https://api.emly.ffois.it/v2/config`. The
`/v2` prefix is the same one the manifest uses (`/v2/updates/manifest`).

**Request headers** (same as every other updater request, see
`source.HTTPSource.applyHeaders`): `User-Agent`, `X-Api-Key`,
`X-EMLy-Hostname`, `X-EMLy-HWID`, `X-EMLy-ADDomain`, `X-EMLy-IntIP`. Plus
`If-None-Match: <etag>` when the cache holds one.

The headers let the server tailor the document per host in the future; the
updater does not rely on that. Overrides are evaluated client-side.

**Responses**

| Status | Meaning for the updater |
|---|---|
| `200` + JSON + `ETag` | New document. Validate (§8); on success replace the cache. |
| `304` | Cache is current. Refresh `fetchedAt` only. |
| `204` | Server reachable, nothing published (a fresh API, or a mirror that has not synced yet). Not an error: keep the cache or the default policy, do **not** log 901, do not try the next candidate. |
| `4xx` / `5xx` / timeout / TLS or connection error | Try the next candidate server. If all fail: keep the cache, log event 901 once per outage. |

**Candidate server order**, deduplicated, one attempt each, no backoff (the
fetch runs every cycle anyway): the site chain from the *cached* policy for
this machine (`baseServer`, then `backupServer` in order, then
`defaultServer`), then `[remoteConfig] endpoints`. A machine with no cache
starts from the bootstrap endpoints.

**Full example document**

Shown as JSONC so every field can carry a comment. The served document is
plain JSON: no comments, no trailing commas. §7 is the normative reference;
the comments here are the short version.

```jsonc
{
  // ── Document identity ──────────────────────────────────────────────────
  // Only breaking changes bump this. An updater that does not know the value
  // rejects the whole document and keeps its cache. Unknown *fields* inside
  // a known schema are ignored, so additive changes do not need a bump.
  "schemaVersion": 1,

  // Monotonic integer, per environment. The updater accepts a document only
  // if revision >= the cached one, so a lagging mirror can never roll a
  // machine back. To roll back on purpose, republish the old content under
  // a new, higher revision.
  "revision": 42,

  // When the document was published. For logs and tickets only: never
  // compared with the machine's clock (PC clocks drift, staleness is
  // computed from the local fetchedAt instead).
  "generatedAt": "2026-08-28T10:00:00Z",

  // ── How often to come back ─────────────────────────────────────────────
  "refresh": {
    // Minimum minutes between two fetches of this document. The fetch is
    // attempted at the top of a poll cycle once this much time has passed.
    // Clamped to [1, 1440]. Defaults to updater.pollIntervalMinutes.
    "intervalMinutes": 15,

    // Days after which a cache that could not be refreshed is reported as
    // stale (event 903, once a day). Reporting only: the cached policy
    // stays in force. 0 disables the warning.
    "staleAfterDays": 7
  },

  // ── Servers ────────────────────────────────────────────────────────────
  // Symbolic name → base URL. Everything else in the document refers to
  // servers by name, so moving a host means editing one line. Base URLs
  // only: the updater appends /v2/updates/manifest, /v2/updates/manifest/
  // updater and /v2/config itself. No query string, no trailing slash.
  "servers": {
    "srv-cb-rete-3g":   "http://172.16.96.73:8080",
    "srv-cloud":        "https://api.emly.ffois.it",
    "srv-cb-rete-enel": "http://10.12.254.123:8080"
  },

  // Server used when the machine matches no site in dcLookupMap: off the
  // domain, DC not listed, no local IP inside the site's subnets, or the
  // site is disabled. A single server, no fallback chain.
  "defaultServer": "srv-cb-rete-3g",

  // ── IPC compatibility (updater ⇄ EMLy named pipe) ──────────────────────
  // The document can only *narrow* what the binaries compile in: it may
  // disable a protocol version or raise a minimum, never enable a version
  // the running updater does not implement or lower a minimum below the
  // compiled one. Version numbers unknown to this updater are ignored.
  "ipcProtocol": {
    "versions": {
      "1": {
        // Range of updater builds that speak this version. Informational:
        // logged as a warning if the running updater is outside it, and
        // handed to EMLy so it can decide what to do.
        "updater": { "min": "1.2.0b", "max": null },

        // Range of EMLy builds accepted on this version. `min` is enforced
        // as max(compiled, remote): a client below it is rejected with
        // UNSUPPORTED_VERSION. `max` is a warning only; null = no ceiling.
        "emly":    { "min": "2.0.0",  "max": null },

        // false rejects every client on this protocol version.
        "enabled": true
      }
    },
    // Protocol version the updater advertises. Must name an enabled entry.
    "defaultVersion": 1
  },

  // ── Site detection ─────────────────────────────────────────────────────
  // Keyed by domain controller name, matched case-insensitively and without
  // the DNS suffix ("DC-RM2" matches "dc-rm2.tregcc.local"). A machine is
  // "at" a site when its nearest DC is the key AND one of its local IPv4
  // addresses is inside one of the site's subnets. Re-evaluated every poll
  // cycle, so a laptop that moves follows its network.
  "dcLookupMap": {
    "DC-RM2": {
      // CIDR list. At least one entry. IPv4 only.
      "internalSubnets": ["172.16.96.0/24", "10.12.8.0/24"],
      // Tried first, with retries and backoff (updater.resolver.*).
      "baseServer": "srv-cb-rete-3g",
      // Tried once each, in order, after baseServer gives up. The cloud is
      // NOT appended implicitly: list it here if you want it as last resort.
      "backupServer": ["srv-cloud"],
      // false makes the site behave as unmapped (→ defaultServer) without
      // deleting its definition.
      "enabled": true
    },
    "DC-CB": {
      "internalSubnets": ["172.16.33.0/24", "172.16.34.0/24"],
      "baseServer": "srv-cb-rete-3g",
      "backupServer": ["srv-cb-rete-enel", "srv-cloud"],
      "enabled": true
    }
  },

  // ── Host whitelist ─────────────────────────────────────────────────────
  // The updater only computes "is this host listed" and passes the answer
  // to EMLy over IPC (host_whitelisted). What EMLy does with it is defined
  // in the emly repo. A host is whitelisted when EITHER list matches.
  "hostIntegrity": {
    "enabled": true,
    "whitelist": {
      // Case-insensitive, exact. Hostnames are neither unique nor stable;
      // prefer hwids for anything that matters.
      "hostnames": ["FOISX-PC", "RM095"],
      // SMBIOS UUID as reported by the updater (X-EMLy-HWID header,
      // SystemInfoResponse.hwid). Case-insensitive, exact.
      "hwids": []
    }
  },

  // ── Kill switches ──────────────────────────────────────────────────────
  // Fail-open by design: an unreachable endpoint never pauses anything, and
  // an `until` in the past re-enables the block on its own. Setting these
  // is the one thing this document can do that stops the fleet, so every
  // flip is logged (event 904) with the reason.
  "control": {
    "updater": {
      // false = no manifest fetch, no download, no install, no pending
      // resume, no self-update. Config fetch, IPC and the trust-store /
      // file-association self-heal keep running, so a paused updater can
      // always be un-paused from here.
      "enabled": true,
      // Free text for logs and tickets. Never shown to the user.
      "reason": null,
      // RFC 3339. When in the past (local clock) the block counts as
      // enabled. null = until the next push says otherwise.
      "until": null
    },
    "app": {
      // Handed to EMLy as-is (after overrides and expiry); the updater does
      // not enforce it.
      "enabled": true,
      // "normal" | "readonly" | "maintenance". EMLy owns the semantics and
      // any user-facing text.
      "mode": "normal",
      "reason": null,
      "until": null
    }
  },

  // ── Updater runtime tuning ─────────────────────────────────────────────
  // One-to-one with the config.ini keys they replace; same validation. Any
  // field left out falls back to the value in config.ini.
  "updater": {
    // Minutes between poll cycles, >= 1. Applies at the next sleep.
    "pollIntervalMinutes": 15,
    // "stable" | "beta" | null. null follows EMLy's GUI_RELEASE_CHANNEL.
    "channelOverride": null,
    // Warning dialog before a forced (isCritical) update kills EMLy, and
    // how long to wait after it before terminating the process.
    "criticalWarning": { "enabled": true, "seconds": 30 },
    // Boot-time retry of the domain controller lookup, when the service
    // starts before netlogon/DNS are ready. attempts=0 or delay=0 = one try.
    "dcLookupRetry":   { "attempts": 6, "delaySeconds": 5 },
    // Retries against baseServer before falling through to backupServer.
    // Backoff doubles from baseBackoffSeconds (5s, 10s, ...).
    "resolver":        { "attempts": 3, "baseBackoffSeconds": 5 },
    // Let the updater install newer releases of itself.
    "selfUpdate":      { "enabled": true },
    // Keep the 3gIT code-signing certificate in the Root/TrustedPublisher
    // stores (machine + console user) so setups elevate as a known publisher.
    "installCertificate": { "enabled": true }
  },

  // ── Logging ────────────────────────────────────────────────────────────
  // Applied as soon as the document is accepted, no restart. Events
  // 900-904 (this feature's own) are always written to the Windows Event
  // Log regardless of these settings, so a bad push stays diagnosable.
  "logging": {
    // "debug" | "info" | "warn" | "error"
    "level": "info",
    // Rolling file: size per file [1, 100] and how many rotated copies to
    // keep [0, 50]. Take effect at the next rotation.
    "maxSizeMB": 2,
    "backups": 5,
    // gzip rotated copies.
    "compress": true,
    // Mirror to the Windows Event Log (source "EMLyUpdater").
    "eventLog": true
  },

  // ── Per-host / per-site exceptions ─────────────────────────────────────
  // Applied in list order on top of the global document, client-side, so a
  // cached document keeps matching after the machine moves. Each entry is a
  // JSON Merge Patch (RFC 7386): objects merge, arrays and scalars replace,
  // null deletes (= back to the config.ini value). `patch` may only touch
  // "control", "updater", "logging" and "defaultServer"; anything else
  // rejects the whole document. Every override is dry-run at validation
  // time, so a patch that would produce an invalid value is refused before
  // it reaches any machine.
  "overrides": [
    {
      // Required, unique in the document. Appears in every log line the
      // override influences.
      "id": "pilot-beta",

      // Selector. Keys are ANDed, values inside one list are ORed. Either
      // { "all": true } on its own, or at least one non-empty list.
      // Available keys:
      //   "all":       true                     every host; must be the only
      //                                         key. {} or empty lists are
      //                                         an error, not "all".
      //   "hwids":     ["<SMBIOS UUID>", ...]   case-insensitive exact
      //   "hostnames": ["<hostname>", ...]      case-insensitive exact
      //   "dcs":       ["<DC label>", ...]      nearest DC this cycle, DNS
      //                                         suffix ignored (same rule
      //                                         as dcLookupMap keys)
      //   "subnets":   ["<cidr>", ...]          any local IPv4 inside any
      //                                         listed CIDR
      //   "domains":   ["<AD domain>", ...]     case-insensitive against the
      //                                         machine's AD domain
      // Off-domain machines never match "dcs" or "domains"; use "hwids" or
      // "hostnames" for those.
      "match": { "hwids": ["9A3F1C77-0000-0000-0000-000000000001"] },

      // Optional. Same keys and rules as "match", except "all" is not
      // allowed here. The entry applies when "match" matches AND "except"
      // does not. Omitted or null = no exemption.
      "except": null,

      // Optional expiry, local clock. Expired entries are skipped.
      "until": "2026-09-30T00:00:00Z",

      "patch": {
        "updater": { "channelOverride": "beta", "pollIntervalMinutes": 5 },
        "logging": { "level": "debug" }
      }
    },
    {
      // Freeze one whole site until a date: every machine whose nearest DC
      // is DC-CB stops installing, config fetch and IPC keep running.
      "id": "cb-freeze",
      "match": { "dcs": ["DC-CB"] },
      "until": "2026-09-05T18:00:00Z",
      "patch": {
        "control": { "updater": { "enabled": false, "reason": "chiusura contabile" } }
      }
    },
    {
      // Two keys in AND: only laptops of the RM2 site that are *currently*
      // on the guest Wi-Fi get a longer poll interval.
      "id": "rm2-guest-wifi",
      "match": { "dcs": ["DC-RM2"], "subnets": ["192.168.100.0/24"] },
      "until": null,
      "patch": {
        "updater": { "pollIntervalMinutes": 60 }
      }
    },
    {
      // Everyone, but only until a date: tighter polling during a rollout
      // window. The point of "all" is the expiry; a permanent fleet-wide
      // value goes in the global "updater" section instead.
      "id": "rollout-window",
      "match": { "all": true },
      "until": "2026-09-10T20:00:00Z",
      "patch": {
        "updater": { "pollIntervalMinutes": 5 }
      }
    },
    {
      // Everyone except the pilot machines: the fleet is frozen while a
      // suspect release is investigated, the pilot group keeps updating so
      // the fix can be verified on it first.
      "id": "fleet-freeze-except-pilot",
      "match": { "all": true },
      "except": { "hwids": ["9A3F1C77-0000-0000-0000-000000000001"] },
      "until": "2026-09-12T08:00:00Z",
      "patch": {
        "control": { "updater": { "enabled": false, "reason": "release 2.1.3 sotto verifica" } }
      }
    }
  ]
}
```

## 7. Document reference

Types: `string`, `int`, `bool`, `semver` (a string `manifest.Less` can parse;
pre-release suffixes like `1.2.0b` allowed), `rfc3339` (UTC timestamp string),
`cidr` (`net.ParseCIDR`), `serverRef` (a key of `servers`). `null` on an
optional field means "not set". Unknown fields anywhere are **ignored**, so a
newer server can add fields without breaking older updaters; only
`schemaVersion` gates compatibility.

### 7.1 Top level

| Field | Type | Required | Rules |
|---|---|---|---|
| `schemaVersion` | int | yes | Must equal `1`. Any other value rejects the document (event 902). |
| `revision` | int ≥ 0 | yes | Monotonic per environment. Replaces the cache only if `>=` cached `revision` (§9.3). |
| `generatedAt` | rfc3339 | yes | Logged and exposed over IPC. **Never** compared with the local clock. |

**Default policy** (no cache, or `[remoteConfig] enabled = false`) is built
from the legacy ini keys: `servers` gets one entry per non-empty
`externalManifestURL` / `internalManifestURL` / `bkInternManifestURL` (base
URL = the URL with the `/v2/updates/manifest` suffix stripped),
`defaultServer` = the external one, `dcLookupMap` = `defaultMappingDCSubnets`
with `baseServer` = internal and `backupServer` = `[bkInternal, external]`,
`updater.*` / `logging.*` from their ini equivalents, `control` all enabled,
`overrides` empty, `revision` = 0. This is what makes the rollout a no-op on a
machine that never reaches the endpoint.

### 7.2 `refresh`

| Field | Type | Default | Rules |
|---|---|---|---|
| `intervalMinutes` | int ≥ 1 | `updater.pollIntervalMinutes` | How often the config is re-fetched. Clamped to `[1, 1440]`. In practice the fetch runs at the top of every poll cycle when at least this much time has passed. |
| `staleAfterDays` | int ≥ 0 | 7 | Age of the cache (from local `fetchedAt`) after which event 903 is logged once per day. `0` disables. The policy stays in force; staleness never changes behaviour. |

### 7.3 `servers` / `defaultServer`

`servers` is a map `name → base URL`. Rules: at least one entry; every value
parses as an absolute `http` or `https` URL with no query string and no
trailing slash (the updater appends `/v2/updates/manifest`, `/updater`, and
`configPath`). `defaultServer` is a `serverRef` and is the chain used when the
machine matches no site (off-site, off-domain, DC not mapped). It is a single
server, no fallback: a machine that matches no site and cannot reach it logs
event 101 as today.

### 7.4 `ipcProtocol`

`versions` is a map `"<n>" → { updater{min,max}, emly{min,max}, enabled }`,
`defaultVersion` is a key of that map. All `min`/`max` are `semver | null`.

Effective compatibility, per protocol version *n*, is the **intersection** of
what the binary compiles in and what the document says:

| Aspect | Compiled-in | Remote can… |
|---|---|---|
| Version *n* exists | yes/no | disable it (`enabled: false`), never enable an unknown one |
| `emly.min` | `ipc.MinCompatibleEMLyVersion` | raise it; a lower value is ignored (effective = max of the two) |
| `emly.max` | `ipc.MaxCompatibleEMLyVersion`, informational | replace it, in both directions; still informational (warning only) |
| `updater.min` / `updater.max` | none | informational; exposed to EMLy over IPC, logged as a warning if this binary is outside the range |

A document listing an unknown version number is **not** rejected (an older
updater must survive a document written for a newer one); the entry is logged
at debug and ignored. `defaultVersion` pointing at a disabled or unknown
version rejects the document.

### 7.5 `dcLookupMap`

Map `DC label → site`. The label is matched against the resolved DC name the
way `sameDCName` does today: case-insensitive, DNS suffix ignored.

| Field | Type | Rules |
|---|---|---|
| `internalSubnets` | `[]cidr` | ≥ 1 entry. IPv4 only for now. |
| `baseServer` | serverRef | required |
| `backupServer` | `[]serverRef` | may be empty; order is the fallback order |
| `enabled` | bool | `false` makes the site behave as unmapped (→ `defaultServer`) without deleting it |

Replaces `defaultMappingDCSubnets` + the three manifest URLs. The resolver
chain for a matched site is `baseServer` (with retries) then `backupServer[*]`
(once each), which maps 1:1 onto `source.Resolver{Primary, Fallbacks}`. Unlike
today the chain does **not** implicitly append the external URL: if a site
wants the cloud as last resort, it lists `srv-cloud` in `backupServer`. The
default policy (§7.1) does list it, preserving current behaviour.

### 7.6 `hostIntegrity`

| Field | Type | Rules |
|---|---|---|
| `enabled` | bool | |
| `whitelist.hostnames` | `[]string` | case-insensitive match against `machineinfo.Hostname` |
| `whitelist.hwids` | `[]string` | case-insensitive exact match against `machineinfo.HWID` (SMBIOS UUID) |

The updater validates the shape, evaluates whether *this* host is whitelisted
(either list matches), and passes the result to EMLy over IPC
(`host_whitelisted`). What EMLy does with it is EMLy's business (open question
§16).

### 7.7 `control`

```
control.updater: { enabled: bool, reason: string|null, until: rfc3339|null }
control.app:     { enabled: bool, mode: "normal"|"readonly"|"maintenance",
                   reason: string|null, until: rfc3339|null }
```

- `updater.enabled = false` ("paused"): `Cycle` skips manifest fetch, download,
  install, pending-install resume, and self-update. It still fetches the
  config, still runs the certificate/association self-heal, still serves IPC.
  Logged as event 904 once when the gate closes and once when it opens.
  A paused updater can therefore always be un-paused remotely.
- `app.*` is not enforced by the updater; it is exposed over IPC verbatim
  (after overrides) plus the effective `enabled` after expiry.
- `until`: when set and in the past **by the local clock**, the block is
  treated as `enabled: true` / `mode: "normal"`. This is the one place the
  local clock is consulted, deliberately: a wrong clock can only make a
  restriction end early or late, never make the machine unreachable.
  A forgotten kill switch heals itself.
- `reason` is for logs and tickets only. It is never shown to the user; any
  user-facing text for a paused or read-only app is EMLy's own, localized
  from `mode`.

### 7.8 `updater`

Same meaning and same validation as the ini keys they replace.

| Field | Type | Default | Rules |
|---|---|---|---|
| `pollIntervalMinutes` | int | 15 | ≥ 1. A change takes effect at the next sleep, not mid-sleep. |
| `channelOverride` | `"stable"`\|`"beta"`\|`null` | null | `null` follows EMLy's `GUI_RELEASE_CHANNEL`. |
| `criticalWarning.enabled` | bool | true | |
| `criticalWarning.seconds` | int | 30 | ≥ 0 |
| `dcLookupRetry.attempts` | int | 6 | ≥ 0 |
| `dcLookupRetry.delaySeconds` | int | 5 | ≥ 0 |
| `resolver.attempts` | int | 3 | ≥ 1 |
| `resolver.baseBackoffSeconds` | int | 5 | ≥ 0 |
| `selfUpdate.enabled` | bool | true | |
| `installCertificate.enabled` | bool | true | replaces `[certificate] enabled` |

Missing sub-objects or fields fall back to the default policy value, so a
minimal document `{schemaVersion, revision, generatedAt, servers,
defaultServer}` is valid.

### 7.9 `logging`

| Field | Type | Default | Rules |
|---|---|---|---|
| `level` | `"debug"`\|`"info"`\|`"warn"`\|`"error"` | `"info"` | Applied immediately after the document is accepted, no restart. |
| `maxSizeMB` | int | 2 | `[1, 100]`. Takes effect at the next rotation. |
| `backups` | int | 5 | `[0, 50]` |
| `compress` | bool | true | |
| `eventLog` | bool | true | Windows Event Log sink on/off. Events 900–904 are always written regardless, so a mis-push is diagnosable. |

Requires `logging.Logger` to grow a `SetLevel` and a way to reconfigure the
lumberjack sink in place; today both are fixed at construction.

### 7.10 `overrides`

Ordered list. Each entry:

| Field | Type | Rules |
|---|---|---|
| `id` | string | required, unique within the document; appears in every log line the override influences |
| `match` | object | required. Either `{ "all": true }` alone, or at least one non-empty list. Keys in **AND**, values within a list in **OR**. |
| `match.all` | `true` | matches every host. Must be the only key in `match`; `false`, or `all` combined with any list, rejects the document. An empty `match` (`{}`) or a `match` whose lists are all empty is **not** "all": it rejects the document, so a typo cannot turn a targeted patch into a fleet-wide one. |
| `match.hwids` | `[]string` | case-insensitive exact |
| `match.hostnames` | `[]string` | case-insensitive exact |
| `match.dcs` | `[]string` | against the DC resolved this cycle, `sameDCName` semantics |
| `match.subnets` | `[]cidr` | any local IPv4 inside any listed CIDR |
| `match.domains` | `[]string` | case-insensitive against `machineinfo.ADDomain` |
| `except` | object\|null | optional. Same shape and same rules as `match`, minus `all` (an `except` with `all` rejects the document). The entry applies when `match` matches **and** `except` does not. Inside `except` the keys are ANDed too: `{ "dcs": ["DC-CB"], "subnets": [...] }` exempts only hosts that satisfy both. To exempt "any of several groups" list them all in the one relevant key, or use two overrides. |
| `until` | rfc3339\|null | expired by the local clock → entry ignored, logged at debug |
| `patch` | object | JSON Merge Patch (RFC 7386) applied to the global document |

`patch` may only contain the top-level keys `control`, `updater`, `logging`,
`defaultServer`. Any other key rejects the whole document. Merge-patch rules:
objects merge recursively, arrays and scalars replace, `null` deletes (which
then means "default policy value", not "absent from validation"). Overrides
are applied in list order after the global document, and the result is
re-validated with the §7.8/§7.9 rules; a patch that yields an invalid value
(e.g. `pollIntervalMinutes: 0`) rejects the document at fetch time, not at
apply time, because every override is dry-run against a synthetic "all match"
host during validation.

Overrides are **not** nested and do not see each other.

`all: true` exists for one reason: a fleet-wide change **with an expiry**.
The global sections have no `until` (except `control`), so "debug logging
for everyone until tomorrow" or "poll every 5 minutes during the rollout
window" would otherwise mean editing the global document twice. A permanent
fleet-wide value belongs in the global document, not in an `all` override;
the validator does not enforce that, the reviewer of the document should.

`except` exists for the most common real case, "everyone but the pilot
group": freeze the fleet, keep the test machines updating. Without it that
takes two overrides relying on list order (an `all` followed by a targeted
one that restores the value), which reads as a mistake to anyone who did not
write it. `except` is evaluated with the same matchers as `match`, on the
same per-cycle facts (§9.4), so a machine that moves into or out of the
exempted set changes side at its next cycle. An `except` that can never
match (e.g. an off-domain host and `except: { dcs: [...] }`) is not an
error: the host is simply not exempted.

The full selector, then, is: **apply if** `match` matches **and** (`except`
is absent **or** `except` does not match) **and** (`until` is absent **or**
`until` is in the future).

## 8. Validation: all or nothing

A fetched document goes through, in order:

1. JSON parse. Size cap 1 MiB.
2. `schemaVersion == 1`.
3. Structural typing of every known field (§7). Unknown fields ignored.
4. Referential integrity: every `serverRef` exists; `defaultVersion` exists
   and is enabled; every CIDR parses; every `until`/`generatedAt` parses;
   override `id`s unique; every `match` is either `{all: true}` alone or has
   at least one non-empty list; every `except`, when present, has at least
   one non-empty list and no `all`; every `patch` limited to the allowed
   keys.
5. Dry-run of every override (§7.10) and range checks on the result.
6. `revision >= cached.revision` (§9.3).

Any failure in 1–5 discards the document, keeps the cache, logs **event 902**
with the first error and the `revision` if it was readable, and does not
retry until the next cycle. Nothing is ever partially applied. Step 6 is not
an error: the document is silently ignored and logged at debug.

## 9. Fetch, cache, apply

### 9.1 Cache file

`%ProgramData%\EMLyUpdater\remote-config.json` (`config.RemoteConfigPath()`):

```json
{
  "fetchedAt": "2026-09-04T08:12:31+02:00",
  "fetchedFrom": "srv-cb-rete-3g",
  "etag": "\"a1b2c3\"",
  "document": { "...": "the validated document verbatim" }
}
```

Written the way `state.Store` writes `state.json`: temp file in the same
directory, `fsync`, rename. A write failure (disk full, ACL) logs a warning,
keeps the previous file, and keeps the new document **in memory** for this
run — the machine still follows the new policy until the next restart, and
retries the write on the next fetch.

On startup the cache is read and validated with the same §8 rules. A corrupt
cache is renamed to `remote-config.bad.json` (one copy, overwritten) and the
default policy is used until a fetch succeeds.

### 9.2 When

- **Startup**, in `RunLoop` before `applySourcePolicy`: load cache → build
  effective policy → one fetch attempt (bounded by `timeoutSeconds`, not
  retried — the first cycle must not wait on the network) → source policy.
- **Every cycle**, at the top of `Cycle`, if `now - lastFetchAttempt ≥
  refresh.intervalMinutes`: fetch, validate, swap.
- The effective policy is held behind an atomic pointer
  (`policy.Current()`); a cycle reads it once at its start and uses that
  snapshot throughout, so a swap mid-cycle cannot mix two policies.

### 9.3 Revision rule

Accept when `document.revision >= cache.revision`. Equal is accepted: it
refreshes `fetchedAt` and the ETag, and lets the server re-push a corrected
document without bumping (only sensible when the operator knows the content
is what every machine should have). Lower is ignored: a backup server serving
a copy from last month must not roll a machine back. To roll back
deliberately, republish the old content under a new revision.

Bootstrap and policy are independent: changing `[remoteConfig] endpoints` or
`configPath` does **not** invalidate the cache.

### 9.4 What re-evaluates per cycle

Today `applySourcePolicy` runs once at startup. With a policy that can change
under it, and laptops that change subnet, the cheap part runs **every cycle**:

- local IPv4 addresses → `machineinfo.LocalIPv4Addresses` (no network)
- DC name → cached from the last successful `DsGetDcName`; re-queried (single
  attempt, no retry window) when the local IP set changed since the last
  cycle or the cached name is older than 1 h. The boot-time retry window
  (`dcLookupRetry`) applies only to the very first lookup, as today.
- site match → resolver chain (§7.5) → overrides (§7.10) → effective policy.

Event 700 keeps its meaning (source decision) and is logged only when the
decision *changes*, plus once at startup, so a stable machine does not emit
it 96 times a day.

## 10. IPC: exposing the policy to EMLy

New request/response pair in `proto/updateripc.proto`, following
`ADDING_IPC_EVENTS.md` (both repos, identical file, next free field numbers):

```proto
message ConfigRequest {}

message ConfigResponse {
  // Effective policy for this host: global document with matching
  // overrides applied and expired `until`s resolved. JSON, same schema as
  // the endpoint, so EMLy parses one format whether it comes from the pipe
  // or (in a future fallback) from the API directly.
  bytes  document_json    = 1;
  int64  revision         = 2;
  string generated_at     = 3;  // from the document
  string fetched_at       = 4;  // local clock, RFC 3339
  enum Source { SOURCE_UNSPECIFIED = 0; REMOTE = 1; CACHE = 2; DEFAULT = 3; }
  Source source           = 5;
  bool   stale            = 6;  // fetchedAt older than refresh.staleAfterDays
  bool   host_whitelisted = 7;  // §7.6, evaluated by the updater
}
```

`source` tells EMLy whether it is looking at something fetched this run
(`REMOTE`), the last-known-good from disk (`CACHE`), or the legacy-derived
policy (`DEFAULT`). EMLy keeps its own last-known-good copy of
`document_json` for the case where the pipe is down; it does not fetch the
endpoint itself in this revision.

`ipc.MaxCompatibleEMLyVersion` is bumped to the EMLy release that adds the
client; protocol version stays `1` (additive message).

## 11. Logging and events

| Event | When | Level |
|---|---|---|
| 900 `EventRemoteConfigApplied` | a new revision was accepted (startup from cache included). Fields: `revision`, `generatedAt`, `fetchedFrom`, `source`, matched override ids | Info |
| 901 `EventRemoteConfigUnreachable` | all candidate servers failed; once per outage (flag cleared on the next success, same pattern as `sourcesUnreachableNotified`) | Warning |
| 902 `EventRemoteConfigRejected` | document failed §8 | Error |
| 903 `EventRemoteConfigStale` | cache older than `staleAfterDays`; once per day | Warning |
| 904 `EventControlGate` | `control.updater.enabled` flipped (either direction), with `reason` and `until` | Warning |

Every cycle's first log line carries `policyRevision` and `policySource`, so
any log excerpt says which policy the machine was running.

## 12. Security considerations

The document decides which host serves setups and can pause the fleet. It
travels over plain HTTP on internal LANs, exactly like the manifest.

What already holds regardless of this feature: a setup is never executed
unless its SHA-256 matches the manifest **and** (for the updater's own setup)
its Authenticode signature matches the pinned thumbprint. A redirected
manifest can therefore make a machine install a *3gIT-signed* build it should
not have, or nothing, but not arbitrary code.

Rules that keep the blast radius bounded:

- Remote can only narrow compiled-in compatibility (§7.4) and never touches
  the signer thumbprint, the API key, the pipe name, or the paths.
- `control.updater.enabled = false` is fail-open: unreachable ≠ paused, and
  `until` expires by itself.
- The cache is written with the same ACLs as `state.json` (ProgramData,
  SYSTEM + Administrators write). A local admin can edit it, which is no more
  than they can do to `config.ini` today.
- The fetch honours the same TLS validation as every other request; the
  `https` cloud endpoint is authenticated, the `http` mirrors are not.

**Recommended follow-up, not in scope:** a detached signature (Ed25519,
public key embedded in the binary, signature in an `X-EMLy-Signature` header
or a top-level `signature` field over the canonical JSON). Until then, the
`X-Api-Key` is *not* a substitute — it is a shared secret sitting in clear on
every machine.

## 13. Failure scenarios

| Scenario | Behaviour |
|---|---|
| Never reached the endpoint (fresh image, offline site) | Default policy from legacy ini keys: identical to today. |
| Endpoint down, cache present | Cache used; 901 once; machine unaffected until `staleAfterDays`, then 903 daily. Never stops. |
| Laptop moves subnet / VPN / off-site | Site match re-evaluated next cycle (§9.4); resolver chain follows. |
| Machine leaves the domain | DC lookup fails → `defaultServer` chain, `domains`/`dcs` overrides stop matching; `hwids`/`hostnames` overrides keep matching. |
| Disk full | Cache write fails, new policy kept in memory, warning logged, retried next fetch. Bootstrap file is never written, so it is never damaged. |
| Malformed / inconsistent document | Rejected whole (902), cache kept. |
| Backup server serves an old revision | Ignored (§9.3). |
| Operator pushes a bad `servers` map | The next fetch still tries the *cached* chain first and then the bootstrap endpoints, so publishing a fixed document with a higher revision recovers every machine at its next cycle without touching them. |
| Kill switch left on | `until` expires; without `until` it stays until the next push, which is what the operator asked for. |
| Wrong local clock | Only `until` evaluation and the staleness warning are affected; both fail towards "keep working". |
| Two updaters on the same schema, different binaries | Older binary ignores unknown fields and unknown IPC versions; `schemaVersion` bump is the only breaking signal. |

## 14. Testing

Pure-Go, no Windows API, `go test ./...` without admin — same rule as the
rest of the repo:

- `internal/policy` (new): table tests for validation (§8) — one case per
  rule, including every rejection path and the unknown-field tolerance;
  merge-patch semantics; override matching (AND/OR, case, DNS suffix,
  `all`, `except`, expiry). The validation and matching cases are driven by
  the **shared conformance fixtures** in `testdata/remoteconfig/`
  (`valid/`, `invalid/` with expected problem paths, `effective/` with
  document + host + expected result), copied verbatim from the API repo the
  way `proto/updateripc.proto` is: the same files must pass on both sides,
  which is what keeps the two validators equal; intersection with compiled IPC constants; default policy derived
  from a legacy ini equals today's behaviour for the shipped
  `config.default.ini`.
- Cache round-trip with a corrupt file, a missing file, and a write failure
  injected through a temp dir made read-only.
- `sourcepolicy_test.go`: existing cases rewritten against `dcLookupMap`;
  new cases for the per-cycle re-evaluation on IP change.
- `source` tests: `304` handling and `If-None-Match`.
- `service_test.go`: `Cycle` with `control.updater.enabled = false` performs
  no fetch/download/install but still runs the config fetch and self-heal.
- Manual: point `[remoteConfig] endpoints` at a local `python -m http.server`
  serving the example document; flip `logging.level`, `control`, and
  `pollIntervalMinutes` and watch the log without restarting the service.

## 15. Implementation order

1. `internal/policy`: document types, validation, merge-patch, override
   matching, default-from-legacy, effective snapshot with atomic pointer.
   Tests. No wiring yet.
2. `internal/logging`: runtime `SetLevel` and sink reconfiguration.
3. Cache read/write (`config.RemoteConfigPath`, atomic write), startup load.
4. `internal/source`: config fetch with ETag; candidate ordering.
5. `internal/service`: per-cycle fetch, source policy from `dcLookupMap`
   re-evaluated per cycle, resolver built from the site chain, `control`
   gate in `Cycle`, events 900–904, `policyRevision` on the cycle log line.
   Remove the `config.ini` write-back.
6. `[remoteConfig]` section in `config.default.ini` + `config.Load`;
   legacy-key comments updated; README "Configuration" and "Update sources"
   sections rewritten.
7. IPC `ConfigRequest`/`ConfigResponse` in both repos; bump
   `MaxCompatibleEMLyVersion`.
8. Server side: see `emly-go-api/docs/superpowers/specs/2026-09-04-remote-config-api-design.md`
   (route, `ETag`, revision allocation, admin publish/rollback, mirror
   replication).

Steps 1–6 ship as one updater release with `[remoteConfig] enabled = true`
and the cloud endpoint as the only bootstrap entry; until the route exists
every machine logs 901 once and runs the default policy, i.e. nothing changes.

## 16. Open questions

- **`hostIntegrity` semantics.** The updater only computes
  `host_whitelisted`. Does EMLy refuse to run, run read-only, or merely
  report when the host is not whitelisted and `enabled` is true?
- **Per-host documents from the server.** The request already carries HWID
  and hostname. Server-side tailoring would make `overrides` partly
  redundant; client-side evaluation was chosen so a cached document keeps
  matching after the machine moves. Revisit if the override list grows past
  a few hundred entries.
- **Should `control.updater.enabled = false` also block the updater's
  self-update?** This design says yes (a paused fleet is fully frozen). The
  alternative — let self-update through so a fix to the pause logic itself
  can land — is defensible.
- **Signature** (§12): which key, where it lives, and whether an unsigned
  document is rejected or merely logged during the transition.
