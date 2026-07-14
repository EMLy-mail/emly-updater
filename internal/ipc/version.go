package ipc

import (
	"fmt"

	"emlyupdater/internal/manifest"
)

// Version is defined in version_generated.go, regenerated from
// versioninfo.json by "go generate" (see tools/genversion) — that is the
// single source of truth for the version string, propagated from there to
// every place that needs it (see tools/genversion's doc comment).

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
	MinCompatibleEMLyVersion = "1.8.0"
	MaxCompatibleEMLyVersion = "1.8.0"
)

// checkPeerVersion enforces MinCompatibleEMLyVersion against a connecting
// client's declared sender_version and logs (without rejecting) a client
// newer than MaxCompatibleEMLyVersion. Fails closed: a missing or
// unparseable sender_version is treated the same as one that is too old.
func (s *Server) checkPeerVersion(senderVersion string) error {
	if senderVersion == "" {
		return fmt.Errorf("missing sender_version (requires EMLy >= %s)", MinCompatibleEMLyVersion)
	}
	belowMin, err := manifest.Less(senderVersion, MinCompatibleEMLyVersion)
	if err != nil {
		return fmt.Errorf("invalid sender_version %q: %w", senderVersion, err)
	}
	if belowMin {
		return fmt.Errorf("EMLy %s is older than the minimum supported %s", senderVersion, MinCompatibleEMLyVersion)
	}
	if aboveMax, err := manifest.Less(MaxCompatibleEMLyVersion, senderVersion); err == nil && aboveMax {
		s.log.Warn("ipc peer version newer than tested range",
			"senderVersion", senderVersion, "maxTested", MaxCompatibleEMLyVersion)
	}
	return nil
}
