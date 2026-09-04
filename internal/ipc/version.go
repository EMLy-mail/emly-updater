package ipc

import (
	"fmt"
	"strconv"

	"emlyupdater/internal/manifest"
	"emlyupdater/internal/policy"
)

// MinCompatibleEMLyVersion / MaxCompatibleEMLyVersion bound the EMLy
// releases known to speak ProtocolVersion — see the compatibility matrix
// atop proto/updateripc.proto. A connecting client whose sender_version is
// below Min is rejected with ERROR_CODE_UNSUPPORTED_VERSION even though
// protocol_version matched, since not every required fix changes the wire
// format. Max is informational only (logged, never enforced): a newer
// EMLy is assumed forward-compatible unless proven otherwise. Bump Max on
// every EMLy release, even one that doesn't touch this package; bump Min
// (and copy the frozen old Max into a new compatibility-matrix row) only
// when EMLy ships a build that requires a newer ProtocolVersion.
const (
	MinCompatibleEMLyVersion = "2.0.0"
	MaxCompatibleEMLyVersion = "2.1.0"
)

// Compat is the accepted EMLy range for one protocol version.
type Compat struct {
	Enabled bool
	// Min is enforced; Max ("" = no ceiling) is a warning only.
	Min, Max string
}

// CompiledCompat is what this binary knows on its own, before any remote
// document has a say.
func CompiledCompat() Compat {
	return Compat{Enabled: true, Min: MinCompatibleEMLyVersion, Max: MaxCompatibleEMLyVersion}
}

// CompiledIPCProtocol expresses the compiled-in matrix in the document's
// shape, for the policy defaults.
func CompiledIPCProtocol() policy.IPCProtocol {
	min, max := MinCompatibleEMLyVersion, MaxCompatibleEMLyVersion
	enabled := true
	return policy.IPCProtocol{
		Versions: map[string]policy.IPCVersion{
			strconv.Itoa(ProtocolVersion): {
				EMLy:    policy.VersionRange{Min: &min, Max: &max},
				Enabled: &enabled,
			},
		},
		DefaultVersion: ProtocolVersion,
	}
}

// EffectiveCompat intersects the compiled matrix with the remote document
// for protocol version v. The document can only narrow: it may disable the
// version or raise the minimum, never enable one the binary lacks or lower
// the minimum below what the binary requires. Max is informational and the
// document replaces it in either direction, null meaning "no ceiling". A
// version the document does not list is left as compiled.
func EffectiveCompat(compiled Compat, remote policy.IPCProtocol, v int) Compat {
	entry, ok := remote.Versions[strconv.Itoa(v)]
	if !ok {
		return compiled
	}
	out := compiled
	out.Enabled = compiled.Enabled && entry.IsEnabled()
	if entry.EMLy.Min != nil && *entry.EMLy.Min != "" {
		if higher, err := manifest.Less(compiled.Min, *entry.EMLy.Min); err == nil && higher {
			out.Min = *entry.EMLy.Min
		}
	}
	if entry.EMLy.Max != nil {
		out.Max = *entry.EMLy.Max
	} else {
		out.Max = ""
	}
	return out
}

// checkPeerVersion enforces the effective minimum against a connecting
// client's declared sender_version and logs (without rejecting) a client
// newer than the effective maximum. Fails closed: a missing or unparseable
// sender_version is treated the same as one that is too old.
func (s *Server) checkPeerVersion(senderVersion string) error {
	compat := s.compat()
	if !compat.Enabled {
		s.log.Error("ipc protocol version disabled by the remote configuration", "protocolVersion", ProtocolVersion)
		return fmt.Errorf("protocol version %d is disabled by policy", ProtocolVersion)
	}
	if senderVersion == "" {
		s.log.Error("missing sender_version (requires EMLy >= %s)", compat.Min)
		return fmt.Errorf("missing sender_version (requires EMLy >= %s)", compat.Min)
	}
	belowMin, err := manifest.Less(senderVersion, compat.Min)
	if err != nil {
		s.log.Error("invalid sender_version %q: %v", senderVersion, err)
		return fmt.Errorf("invalid sender_version %q: %w", senderVersion, err)
	}
	if belowMin {
		s.log.Error("ipc peer version older than minimum supported",
			"senderVersion", senderVersion, "minSupported", compat.Min)
		return fmt.Errorf("EMLy %s is older than the minimum supported %s", senderVersion, compat.Min)
	}
	if compat.Max != "" {
		if aboveMax, err := manifest.Less(compat.Max, senderVersion); err == nil && aboveMax {
			s.log.Warn("ipc peer version newer than tested range",
				"senderVersion", senderVersion, "maxTested", compat.Max)
		}
	}
	return nil
}

// compat resolves the range to enforce right now: compiled-in, narrowed by
// the current policy when a provider is wired in.
func (s *Server) compat() Compat {
	compiled := CompiledCompat()
	if s.policy == nil {
		return compiled
	}
	view := s.policy()
	if view == nil {
		return compiled
	}
	return EffectiveCompat(compiled, view.IPC, ProtocolVersion)
}
