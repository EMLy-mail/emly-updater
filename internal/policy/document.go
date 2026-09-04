// Package policy implements the remote configuration document served by
// GET /v2/config: its types, validation, per-host override evaluation, the
// on-disk last-known-good cache, and the snapshot the running service reads.
//
// The design is described in
// docs/superpowers/specs/2026-09-04-remote-config-design.md. In short: the
// document is validated all-or-nothing, overrides are evaluated client-side
// against this machine's facts (HWID, hostname, nearest DC, local IPs, AD
// domain), and any section the document leaves out falls back to a default
// derived from config.ini so a machine that never reaches the endpoint
// behaves exactly as it did before this package existed.
package policy

import "time"

// SchemaVersion is the only document schema this build understands. Additive
// changes (new fields) do not bump it - unknown fields are ignored - so a
// document written for a newer updater still loads here.
const SchemaVersion = 1

// MaxDocumentSize caps a document at 1 MiB before it is even parsed. The same
// number is enforced server-side.
const MaxDocumentSize = 1 << 20

// ManifestPath is appended to a server's base URL to reach EMLy's update
// manifest; the updater's own manifest lives one segment further down (see
// config.UpdaterManifestURL).
const ManifestPath = "/v2/updates/manifest"

// Document is the typed form of the remote configuration. Every field maps
// 1:1 onto the JSON schema; see the spec for semantics.
type Document struct {
	SchemaVersion int    `json:"schemaVersion"`
	Revision      int64  `json:"revision"`
	GeneratedAt   string `json:"generatedAt"`

	Refresh       Refresh           `json:"refresh"`
	Servers       map[string]string `json:"servers"`
	DefaultServer string            `json:"defaultServer"`
	IPCProtocol   IPCProtocol       `json:"ipcProtocol"`
	DCLookupMap   map[string]Site   `json:"dcLookupMap"`
	HostIntegrity HostIntegrity     `json:"hostIntegrity"`
	Control       Control           `json:"control"`
	Updater       UpdaterSettings   `json:"updater"`
	Logging       LoggingSettings   `json:"logging"`
	Overrides     []Override        `json:"overrides"`
}

// Refresh governs how often the document itself is re-fetched and when a
// cache that could not be refreshed is reported as stale.
type Refresh struct {
	IntervalMinutes int `json:"intervalMinutes"`
	StaleAfterDays  int `json:"staleAfterDays"`
}

// IPCProtocol is the document's view of updater ⇄ EMLy compatibility. It can
// only narrow what the binaries compile in; see ipc.EffectiveCompat.
type IPCProtocol struct {
	Versions       map[string]IPCVersion `json:"versions"`
	DefaultVersion int                   `json:"defaultVersion"`
}

// IPCVersion describes one protocol version's accepted peer ranges.
type IPCVersion struct {
	Updater VersionRange `json:"updater"`
	EMLy    VersionRange `json:"emly"`
	// Enabled is a pointer so an entry that leaves it out reads as enabled
	// rather than as disabled by omission.
	Enabled *bool `json:"enabled"`
}

// IsEnabled reports whether the entry is enabled, treating absence as true.
func (v IPCVersion) IsEnabled() bool { return v.Enabled == nil || *v.Enabled }

// VersionRange bounds a peer's semver. nil means "no bound".
type VersionRange struct {
	Min *string `json:"min"`
	Max *string `json:"max"`
}

// Site is one entry of dcLookupMap: the subnets that count as "at this site"
// for machines whose nearest domain controller is the map key, and the
// servers those machines should use, in order.
type Site struct {
	InternalSubnets []string `json:"internalSubnets"`
	BaseServer      string   `json:"baseServer"`
	BackupServer    []string `json:"backupServer"`
	Enabled         *bool    `json:"enabled"`
}

// IsEnabled reports whether the site is enabled, treating absence as true.
func (s Site) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// HostIntegrity carries the host whitelist. The updater only evaluates
// whether this host is listed and hands the answer to EMLy over IPC.
type HostIntegrity struct {
	Enabled   bool      `json:"enabled"`
	Whitelist Whitelist `json:"whitelist"`
}

// Whitelist lists hosts by name and by hardware id. Either list matching is
// enough.
type Whitelist struct {
	Hostnames []string `json:"hostnames"`
	HWIDs     []string `json:"hwids"`
}

// Control holds the kill switches. Both are fail-open: an unreachable
// endpoint never pauses anything, and an expired Until re-enables the block
// on its own.
type Control struct {
	Updater ControlUpdater `json:"updater"`
	App     ControlApp     `json:"app"`
}

// ControlUpdater pauses the update machinery (manifest fetch, download,
// install, self-update) while leaving the config fetch, IPC and the
// self-heal steps running.
type ControlUpdater struct {
	Enabled bool    `json:"enabled"`
	Reason  *string `json:"reason"`
	Until   *string `json:"until"`
}

// ControlApp is handed to EMLy as-is; the updater does not enforce it.
type ControlApp struct {
	Enabled bool    `json:"enabled"`
	Mode    string  `json:"mode"`
	Reason  *string `json:"reason"`
	Until   *string `json:"until"`
}

// App modes accepted in control.app.mode.
const (
	AppModeNormal      = "normal"
	AppModeReadOnly    = "readonly"
	AppModeMaintenance = "maintenance"
)

// UpdaterSettings are the runtime knobs that used to live only in config.ini.
type UpdaterSettings struct {
	PollIntervalMinutes int              `json:"pollIntervalMinutes"`
	ChannelOverride     *string          `json:"channelOverride"`
	CriticalWarning     CriticalWarning  `json:"criticalWarning"`
	DCLookupRetry       DCLookupRetry    `json:"dcLookupRetry"`
	Resolver            ResolverSettings `json:"resolver"`
	SelfUpdate          Toggle           `json:"selfUpdate"`
	InstallCertificate  Toggle           `json:"installCertificate"`
}

// CriticalWarning configures the dialog shown before a forced update kills
// EMLy, and how long to wait after it.
type CriticalWarning struct {
	Enabled bool `json:"enabled"`
	Seconds int  `json:"seconds"`
}

// DCLookupRetry bounds the boot-time retry of the domain controller lookup.
type DCLookupRetry struct {
	Attempts     int `json:"attempts"`
	DelaySeconds int `json:"delaySeconds"`
}

// ResolverSettings tune the retry loop against a site's base server.
type ResolverSettings struct {
	Attempts           int `json:"attempts"`
	BaseBackoffSeconds int `json:"baseBackoffSeconds"`
}

// Toggle is a bare {enabled} object.
type Toggle struct {
	Enabled bool `json:"enabled"`
}

// LoggingSettings are applied to the running logger as soon as a document is
// accepted, no restart.
type LoggingSettings struct {
	Level     string `json:"level"`
	MaxSizeMB int    `json:"maxSizeMB"`
	Backups   int    `json:"backups"`
	Compress  bool   `json:"compress"`
	EventLog  bool   `json:"eventLog"`
}

// Override is one per-host/per-site exception: a JSON Merge Patch applied
// on top of the global document for the hosts its selector matches.
type Override struct {
	ID     string         `json:"id"`
	Match  Selector       `json:"match"`
	Except *Selector      `json:"except"`
	Until  *string        `json:"until"`
	Patch  map[string]any `json:"patch"`
}

// Selector picks hosts. Keys are ANDed, values within one list are ORed.
// All is only valid on its own, and only in Match.
type Selector struct {
	All       bool     `json:"all"`
	HWIDs     []string `json:"hwids"`
	Hostnames []string `json:"hostnames"`
	DCs       []string `json:"dcs"`
	Subnets   []string `json:"subnets"`
	Domains   []string `json:"domains"`
}

// Host is what a selector is evaluated against: the facts about this
// machine gathered at the start of a poll cycle.
type Host struct {
	HWID     string
	Hostname string
	// DC is the nearest domain controller's name as resolved this cycle;
	// empty when the lookup failed or the machine is off the domain.
	DC     string
	Domain string
	IPs    []string
	// Now is the clock used for every `until`; tests pin it.
	Now time.Time
}

// PatchableSections are the only top-level keys an override's patch may
// touch. Anything else rejects the whole document.
var PatchableSections = []string{"control", "updater", "logging", "defaultServer"}

// Duration helpers, so callers do not re-derive units from field names.

func (r Refresh) Interval() time.Duration     { return time.Duration(r.IntervalMinutes) * time.Minute }
func (r Refresh) StaleAfter() time.Duration   { return time.Duration(r.StaleAfterDays) * 24 * time.Hour }
func (u UpdaterSettings) PollInterval() time.Duration {
	return time.Duration(u.PollIntervalMinutes) * time.Minute
}
func (d DCLookupRetry) Delay() time.Duration { return time.Duration(d.DelaySeconds) * time.Second }
func (r ResolverSettings) BaseBackoff() time.Duration {
	return time.Duration(r.BaseBackoffSeconds) * time.Second
}

// Channel returns the channel override as a plain string, "" when unset.
func (u UpdaterSettings) Channel() string {
	if u.ChannelOverride == nil {
		return ""
	}
	return *u.ChannelOverride
}
