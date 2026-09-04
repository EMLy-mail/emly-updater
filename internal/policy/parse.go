package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"emlyupdater/internal/manifest"
)

// Problem is one validation failure, addressed by a JSON-pointer-like path
// so a log line or a dashboard can point at the offending field.
type Problem struct {
	Path    string
	Message string
}

func (p Problem) String() string { return p.Path + ": " + p.Message }

// Problems is the error returned when a document is rejected. Every problem
// is reported, not just the first, so one round-trip fixes them all.
type Problems []Problem

func (ps Problems) Error() string {
	if len(ps) == 0 {
		return "no problems"
	}
	parts := make([]string, 0, len(ps))
	for _, p := range ps {
		parts = append(parts, p.String())
	}
	return strings.Join(parts, "; ")
}

// Defaults are the values a document falls back to for the sections it
// leaves out (or sets to null). They come from config.ini and from the
// compiled-in IPC compatibility constants, see DefaultsFromConfig.
type Defaults struct {
	Refresh     Refresh         `json:"refresh"`
	Control     Control         `json:"control"`
	Updater     UpdaterSettings `json:"updater"`
	Logging     LoggingSettings `json:"logging"`
	IPCProtocol IPCProtocol     `json:"ipcProtocol"`
}

// Parsed is a validated document plus what is needed to re-evaluate it for a
// given host: the raw global sections (so an override's merge patch can be
// applied to what was actually sent, not to the defaults-filled result) and
// the defaults to fill in afterwards.
type Parsed struct {
	// Global is the document with no override applied.
	Global *Document

	raw      map[string]any
	defaults map[string]any
	// legacyManifestURLs pins a full manifest URL for a server whose legacy
	// config.ini URL did not end with ManifestPath, so a hand-edited path
	// keeps working through the default policy exactly as it did before.
	legacyManifestURLs map[string]string
}

// mergedSections are filled field by field from the defaults; a document
// that names only some of their keys keeps the default for the others.
var mergedSections = []string{"refresh", "control", "updater", "logging"}

// Parse validates data against this build's schema and fills the omitted
// sections from defaults. The document is accepted whole or rejected whole:
// a non-nil Problems means nothing in it may be used.
func Parse(data []byte, defaults Defaults) (*Parsed, Problems) {
	if len(data) > MaxDocumentSize {
		return nil, Problems{{Path: "/", Message: fmt.Sprintf("document is %d bytes, the limit is %d", len(data), MaxDocumentSize)}}
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, Problems{{Path: "/", Message: "invalid JSON: " + err.Error()}}
	}
	if raw == nil {
		return nil, Problems{{Path: "/", Message: "document is null"}}
	}

	// schemaVersion gates everything else: a document written for a schema
	// this build does not know is not worth typing field by field.
	sv, ok := raw["schemaVersion"]
	if !ok {
		return nil, Problems{{Path: "/schemaVersion", Message: "required"}}
	}
	if n, ok := asInt(sv); !ok || n != SchemaVersion {
		return nil, Problems{{Path: "/schemaVersion", Message: fmt.Sprintf("must be %d, got %v", SchemaVersion, sv)}}
	}

	defMap, err := toMap(defaults)
	if err != nil {
		return nil, Problems{{Path: "/", Message: "internal: defaults are not serialisable: " + err.Error()}}
	}

	overridesRaw := raw["overrides"]
	global := cloneValue(raw).(map[string]any)
	delete(global, "overrides")

	p := &Parsed{raw: global, defaults: defMap}

	var problems Problems
	doc, probs := p.build(global)
	problems = append(problems, probs...)
	if doc != nil {
		problems = append(problems, validateDocument(doc, true)...)
	}

	overrides, probs := parseOverrides(overridesRaw)
	problems = append(problems, probs...)

	if doc != nil && len(problems) == 0 {
		doc.Overrides = overrides
		problems = append(problems, p.dryRunOverrides(overrides)...)
	}

	if len(problems) > 0 {
		return nil, problems
	}
	p.Global = doc
	return p, nil
}

// build fills defaults into the raw global sections and decodes the result.
func (p *Parsed) build(global map[string]any) (*Document, Problems) {
	completed := complete(p.defaults, global)
	doc, err := decode(completed)
	if err != nil {
		return nil, Problems{{Path: "/", Message: err.Error()}}
	}
	return doc, nil
}

// complete produces the effective generic document for raw: the
// topology sections are taken as sent (absent ones get their neutral
// value), the merged sections are layered over the defaults.
func complete(defaults, raw map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"schemaVersion", "revision", "generatedAt", "servers", "defaultServer", "dcLookupMap", "hostIntegrity"} {
		if v, ok := raw[k]; ok && v != nil {
			out[k] = cloneValue(v)
		}
	}
	if _, ok := out["dcLookupMap"]; !ok {
		// A remote document is authoritative on topology: no sites listed
		// means no sites, not "whatever config.ini said".
		out["dcLookupMap"] = map[string]any{}
	}
	if _, ok := out["hostIntegrity"]; !ok {
		out["hostIntegrity"] = map[string]any{"enabled": false}
	}
	if v, ok := raw["ipcProtocol"]; ok && v != nil {
		out["ipcProtocol"] = cloneValue(v)
	} else {
		out["ipcProtocol"] = cloneValue(defaults["ipcProtocol"])
	}
	for _, k := range mergedSections {
		if v, ok := raw[k].(map[string]any); ok {
			out[k] = mergePatch(defaults[k], stripNulls(v))
		} else {
			out[k] = cloneValue(defaults[k])
		}
	}
	return out
}

// decode turns the generic document into its typed form. Unknown fields are
// ignored by encoding/json, which is the forward-compatibility rule the
// schema relies on.
func decode(m map[string]any) (*Document, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var doc Document
	if err := json.Unmarshal(b, &doc); err != nil {
		if te, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
			return nil, fmt.Errorf("/%s: expected %s, got %s", strings.ReplaceAll(te.Field, ".", "/"), te.Type, te.Value)
		}
		return nil, err
	}
	return &doc, nil
}

func toMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// asInt accepts a JSON number that is integral.
func asInt(v any) (int64, bool) {
	f, ok := v.(float64)
	if !ok || f != float64(int64(f)) {
		return 0, false
	}
	return int64(f), true
}

// parseOverrides types the overrides list and checks everything about it
// that does not need a dry-run: ids, selector shape, expiry, patch keys.
func parseOverrides(raw any) ([]Override, Problems) {
	if raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, Problems{{Path: "/overrides", Message: "must be a list"}}
	}
	var problems Problems
	out := make([]Override, 0, len(list))
	seen := map[string]bool{}
	for i, item := range list {
		path := "/overrides/" + strconv.Itoa(i)
		m, ok := item.(map[string]any)
		if !ok {
			problems = append(problems, Problem{path, "must be an object"})
			continue
		}
		b, _ := json.Marshal(m)
		var o Override
		if err := json.Unmarshal(b, &o); err != nil {
			problems = append(problems, Problem{path, err.Error()})
			continue
		}

		if o.ID == "" {
			problems = append(problems, Problem{path + "/id", "required"})
		} else if seen[o.ID] {
			problems = append(problems, Problem{path + "/id", "duplicate id " + strconv.Quote(o.ID)})
		}
		seen[o.ID] = true

		if _, ok := m["match"]; !ok {
			problems = append(problems, Problem{path + "/match", "required"})
		} else {
			problems = append(problems, validateSelector(o.Match, m["match"], path+"/match", true)...)
		}
		if ex, ok := m["except"]; ok && ex != nil {
			problems = append(problems, validateSelector(*o.Except, ex, path+"/except", false)...)
		}
		if o.Until != nil {
			if _, err := time.Parse(time.RFC3339, *o.Until); err != nil {
				problems = append(problems, Problem{path + "/until", "not an RFC 3339 timestamp"})
			}
		}
		if o.Patch == nil {
			problems = append(problems, Problem{path + "/patch", "required"})
		}
		for k := range o.Patch {
			if !contains(PatchableSections, k) {
				problems = append(problems, Problem{path + "/patch/" + k, "not a patchable section (allowed: " + strings.Join(PatchableSections, ", ") + ")"})
			}
		}
		out = append(out, o)
	}
	return out, problems
}

// validateSelector enforces the selector shape: `all` alone (match only),
// or at least one non-empty list; empty lists are an error, never "all".
func validateSelector(s Selector, raw any, path string, allowAll bool) Problems {
	m, ok := raw.(map[string]any)
	if !ok {
		return Problems{{path, "must be an object"}}
	}
	var problems Problems
	if allRaw, present := m["all"]; present {
		if !allowAll {
			problems = append(problems, Problem{path + "/all", "not allowed here"})
		} else if allRaw != true {
			problems = append(problems, Problem{path + "/all", "must be true when present"})
		} else if len(m) > 1 {
			problems = append(problems, Problem{path + "/all", "must be the only key in the selector"})
		}
		if len(problems) > 0 {
			return problems
		}
		if allowAll {
			return nil
		}
	}
	lists := 0
	for _, l := range [][]string{s.HWIDs, s.Hostnames, s.DCs, s.Domains, s.Subnets} {
		if len(l) > 0 {
			lists++
		}
	}
	if lists == 0 {
		problems = append(problems, Problem{path, "needs at least one non-empty list (hwids, hostnames, dcs, subnets, domains)"})
	}
	for i, c := range s.Subnets {
		if err := validCIDR(c); err != nil {
			problems = append(problems, Problem{path + "/subnets/" + strconv.Itoa(i), err.Error()})
		}
	}
	return problems
}

// dryRunOverrides applies every override on its own and then all of them in
// order, validating each result, so a patch that would produce an invalid
// value is refused at fetch time rather than discovered when a host matches.
func (p *Parsed) dryRunOverrides(overrides []Override) Problems {
	var problems Problems
	all := p.raw
	for i, o := range overrides {
		path := "/overrides/" + strconv.Itoa(i) + "/patch"
		single := mergePatch(p.raw, o.Patch).(map[string]any)
		if doc, probs := p.build(single); probs != nil {
			problems = append(problems, prefix(path, probs)...)
		} else {
			problems = append(problems, prefix(path, validateDocument(doc, true))...)
		}
		all = mergePatch(all, o.Patch).(map[string]any)
	}
	if len(overrides) > 1 && len(problems) == 0 {
		if doc, probs := p.build(all); probs != nil {
			problems = append(problems, prefix("/overrides", probs)...)
		} else {
			problems = append(problems, prefix("/overrides", validateDocument(doc, true))...)
		}
	}
	return problems
}

func prefix(path string, ps Problems) Problems {
	out := make(Problems, 0, len(ps))
	for _, p := range ps {
		out = append(out, Problem{Path: path + p.Path, Message: p.Message})
	}
	return out
}

// validateDocument applies the field-level rules of the spec (§7) plus
// referential integrity. remote is false for the legacy-derived default
// policy, which has no revision or generatedAt to check.
func validateDocument(d *Document, remote bool) Problems {
	var ps Problems
	add := func(path, msg string) { ps = append(ps, Problem{path, msg}) }

	if remote {
		if d.Revision < 0 {
			add("/revision", "must be >= 0")
		}
		if d.GeneratedAt == "" {
			add("/generatedAt", "required")
		} else if _, err := time.Parse(time.RFC3339, d.GeneratedAt); err != nil {
			add("/generatedAt", "not an RFC 3339 timestamp")
		}
	}

	if d.Refresh.IntervalMinutes < 1 {
		d.Refresh.IntervalMinutes = 1
	} else if d.Refresh.IntervalMinutes > 1440 {
		d.Refresh.IntervalMinutes = 1440
	}
	if d.Refresh.StaleAfterDays < 0 {
		add("/refresh/staleAfterDays", "must be >= 0")
	}

	if len(d.Servers) == 0 {
		add("/servers", "required, at least one entry")
	}
	for name, raw := range d.Servers {
		if name == "" {
			add("/servers", "empty server name")
			continue
		}
		if err := validServerURL(raw); err != nil {
			add("/servers/"+name, err.Error())
		}
	}
	serverRef := func(path, name string) {
		if name == "" {
			add(path, "required")
			return
		}
		if _, ok := d.Servers[name]; !ok {
			add(path, "unknown server "+strconv.Quote(name))
		}
	}
	serverRef("/defaultServer", d.DefaultServer)

	// ipcProtocol: keys must be positive integers; defaultVersion must name
	// an enabled entry; bounds must parse.
	for key, v := range d.IPCProtocol.Versions {
		path := "/ipcProtocol/versions/" + key
		if n, err := strconv.Atoi(key); err != nil || n <= 0 {
			add(path, "version key must be a positive integer")
		}
		for _, r := range []struct {
			name string
			rng  VersionRange
		}{{"updater", v.Updater}, {"emly", v.EMLy}} {
			if r.rng.Min != nil && !validVersion(*r.rng.Min) {
				add(path+"/"+r.name+"/min", "not a semver")
			}
			if r.rng.Max != nil && !validVersion(*r.rng.Max) {
				add(path+"/"+r.name+"/max", "not a semver")
			}
		}
	}
	if dv, ok := d.IPCProtocol.Versions[strconv.Itoa(d.IPCProtocol.DefaultVersion)]; !ok {
		add("/ipcProtocol/defaultVersion", "does not name a listed version")
	} else if !dv.IsEnabled() {
		add("/ipcProtocol/defaultVersion", "names a disabled version")
	}

	seenDC := map[string]string{}
	for name, site := range d.DCLookupMap {
		path := "/dcLookupMap/" + name
		label := strings.ToLower(hostLabel(name))
		if label == "" {
			add(path, "empty domain controller name")
		} else if prev, dup := seenDC[label]; dup {
			add(path, "same domain controller as "+strconv.Quote(prev)+" (names are compared without case or DNS suffix)")
		}
		seenDC[label] = name
		if len(site.InternalSubnets) == 0 {
			add(path+"/internalSubnets", "required, at least one CIDR")
		}
		for i, c := range site.InternalSubnets {
			if err := validCIDR(c); err != nil {
				add(path+"/internalSubnets/"+strconv.Itoa(i), err.Error())
			}
		}
		serverRef(path+"/baseServer", site.BaseServer)
		for i, b := range site.BackupServer {
			serverRef(path+"/backupServer/"+strconv.Itoa(i), b)
		}
	}

	for _, u := range []struct {
		path  string
		until *string
	}{{"/control/updater/until", d.Control.Updater.Until}, {"/control/app/until", d.Control.App.Until}} {
		if u.until != nil {
			if _, err := time.Parse(time.RFC3339, *u.until); err != nil {
				add(u.path, "not an RFC 3339 timestamp")
			}
		}
	}
	switch d.Control.App.Mode {
	case AppModeNormal, AppModeReadOnly, AppModeMaintenance:
	default:
		add("/control/app/mode", "must be one of normal, readonly, maintenance")
	}

	up := d.Updater
	if up.PollIntervalMinutes < 1 {
		add("/updater/pollIntervalMinutes", "must be >= 1")
	}
	if c := up.Channel(); c != "" && c != "stable" && c != "beta" {
		add("/updater/channelOverride", "must be \"stable\", \"beta\" or null")
	}
	if up.CriticalWarning.Seconds < 0 {
		add("/updater/criticalWarning/seconds", "must be >= 0")
	}
	if up.DCLookupRetry.Attempts < 0 || up.DCLookupRetry.Attempts > 60 {
		add("/updater/dcLookupRetry/attempts", "must be between 0 and 60")
	}
	if up.DCLookupRetry.DelaySeconds < 0 || up.DCLookupRetry.DelaySeconds > 300 {
		add("/updater/dcLookupRetry/delaySeconds", "must be between 0 and 300")
	}
	if up.Resolver.Attempts < 1 {
		add("/updater/resolver/attempts", "must be >= 1")
	}
	if up.Resolver.BaseBackoffSeconds < 0 {
		add("/updater/resolver/baseBackoffSeconds", "must be >= 0")
	}

	lg := d.Logging
	switch lg.Level {
	case "debug", "info", "warn", "error":
	default:
		add("/logging/level", "must be one of debug, info, warn, error")
	}
	if lg.MaxSizeMB < 1 || lg.MaxSizeMB > 100 {
		add("/logging/maxSizeMB", "must be between 1 and 100")
	}
	if lg.Backups < 0 || lg.Backups > 50 {
		add("/logging/backups", "must be between 0 and 50")
	}

	sort.SliceStable(ps, func(i, j int) bool { return ps[i].Path < ps[j].Path })
	return ps
}

func validServerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("not a URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("scheme must be http or https")
	}
	if u.Host == "" {
		return errors.New("missing host")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("must not carry a query string or fragment")
	}
	if strings.HasSuffix(u.Path, "/") {
		return errors.New("must not end with a slash")
	}
	return nil
}

func validCIDR(c string) error {
	ip, _, err := net.ParseCIDR(strings.TrimSpace(c))
	if err != nil {
		return fmt.Errorf("%q is not a valid CIDR block", c)
	}
	if ip.To4() == nil {
		return fmt.Errorf("%q is not IPv4", c)
	}
	return nil
}

func validVersion(v string) bool {
	_, err := manifest.Less(v, v)
	return err == nil
}

func contains(list []string, s string) bool { return slices.Contains(list, s) }

// hostLabel keeps the leading label of a host name, dropping any DNS suffix.
func hostLabel(name string) string {
	label, _, _ := strings.Cut(strings.TrimSpace(name), ".")
	return label
}

// SameDCName compares two domain controller names the way the site lookup
// does: case-insensitively and ignoring any DNS suffix, so "DC-RM2" matches
// "dc-rm2.tregcc.local".
func SameDCName(a, b string) bool {
	return strings.EqualFold(hostLabel(a), hostLabel(b))
}
