package policy

import (
	"strings"

	"emlyupdater/internal/config"
)

// Legacy server names used by the default policy derived from config.ini.
// They are what shows up in the log as "site chain" on a machine that has
// never received a remote document.
const (
	LegacyExternal       = "external"
	LegacyInternal       = "internal"
	LegacyInternalBackup = "internalBackup"
)

// DefaultsFromConfig derives the fallback values for the merged sections
// from the bootstrap config. ipc carries the compiled-in compatibility
// matrix (the policy package cannot import ipc, which imports it).
// consoleDebug mirrors logging.New's console flag: the foreground `run`
// mode logs at debug and the default policy must not lower it.
func DefaultsFromConfig(cfg *config.Config, ipc IPCProtocol, consoleDebug bool) Defaults {
	level := "info"
	if consoleDebug {
		level = "debug"
	}
	var channel *string
	if cfg.ChannelOverride != "" {
		c := cfg.ChannelOverride
		channel = &c
	}
	pollMinutes := max(int(cfg.PollInterval.Minutes()), 1)
	return Defaults{
		Refresh: Refresh{IntervalMinutes: pollMinutes, StaleAfterDays: 7},
		Control: Control{
			Updater: ControlUpdater{Enabled: true},
			App:     ControlApp{Enabled: true, Mode: AppModeNormal},
		},
		Updater: UpdaterSettings{
			PollIntervalMinutes: pollMinutes,
			ChannelOverride:     channel,
			CriticalWarning:     CriticalWarning{Enabled: cfg.CriticalWarningEnabled, Seconds: cfg.CriticalWarningSeconds},
			DCLookupRetry:       DCLookupRetry{Attempts: cfg.DCLookupRetryAttempts, DelaySeconds: int(cfg.DCLookupRetryDelay.Seconds())},
			Resolver:            ResolverSettings{Attempts: 3, BaseBackoffSeconds: 5},
			SelfUpdate:          Toggle{Enabled: cfg.SelfUpdateEnabled},
			InstallCertificate:  Toggle{Enabled: cfg.CertificateEnabled},
		},
		Logging: LoggingSettings{Level: level, MaxSizeMB: 2, Backups: 5, Compress: true, EventLog: true},
		IPCProtocol: ipc,
	}
}

// FromLegacy builds the default policy: the document a machine runs when it
// has no valid remote document cached. Its topology is the legacy [source]
// section re-expressed in the new shape, so the behaviour is the one this
// service had before remote configuration existed:
//
//   - servers: one per non-empty manifest URL, named external / internal /
//     internalBackup; the base URL is the manifest URL minus ManifestPath
//     (a URL with a different path is kept whole, see legacyManifestURLs).
//   - dcLookupMap: defaultMappingDCSubnets, every site on the internal
//     server with [internalBackup, external] as its fallback chain - the
//     exact order the resolver used.
//   - defaultServer: external, which is where every unmapped machine went.
//
// One legacy corner is not reproduced: `primary = internal` with an empty
// defaultMappingDCSubnets used to mean "internal, no questions asked". Here
// it maps to defaultServer = internal, which keeps the primary but drops the
// fallbacks - a chain needs a site, and that configuration never had one.
func FromLegacy(cfg *config.Config, d Defaults) *Parsed {
	servers := map[string]string{}
	legacyURLs := map[string]string{}
	addServer := func(name, manifestURL string) {
		if manifestURL == "" {
			return
		}
		base, ok := strings.CutSuffix(manifestURL, ManifestPath)
		if !ok {
			base = manifestURL
			legacyURLs[name] = manifestURL
		}
		servers[name] = base
	}
	addServer(LegacyExternal, cfg.ExternalManifestURL)
	addServer(LegacyInternal, cfg.InternalManifestURL)
	addServer(LegacyInternalBackup, cfg.BackupInternalManifestURL)

	defaultServer := LegacyExternal
	if _, ok := servers[LegacyExternal]; !ok {
		defaultServer = LegacyInternal
	}
	if _, ok := servers[LegacyInternal]; ok && len(cfg.DCSubnetMap) == 0 && cfg.Primary == config.SourceInternal {
		defaultServer = LegacyInternal
	}

	sites := map[string]Site{}
	if _, ok := servers[LegacyInternal]; ok {
		var backups []string
		if _, ok := servers[LegacyInternalBackup]; ok {
			backups = append(backups, LegacyInternalBackup)
		}
		if _, ok := servers[LegacyExternal]; ok {
			backups = append(backups, LegacyExternal)
		}
		for dc, subnets := range cfg.DCSubnetMap {
			cidrs := make([]string, 0, len(subnets))
			for _, n := range subnets {
				cidrs = append(cidrs, n.String())
			}
			sites[dc] = Site{InternalSubnets: cidrs, BaseServer: LegacyInternal, BackupServer: backups}
		}
	}

	doc := &Document{
		SchemaVersion: SchemaVersion,
		Refresh:       d.Refresh,
		Servers:       servers,
		DefaultServer: defaultServer,
		IPCProtocol:   d.IPCProtocol,
		DCLookupMap:   sites,
		HostIntegrity: HostIntegrity{},
		Control:       d.Control,
		Updater:       d.Updater,
		Logging:       d.Logging,
	}

	defMap, _ := toMap(d)
	raw, _ := toMap(doc)
	delete(raw, "overrides")
	return &Parsed{Global: doc, raw: raw, defaults: defMap, legacyManifestURLs: legacyURLs}
}
