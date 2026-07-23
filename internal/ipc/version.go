package ipc

import (
	"fmt"

	"emlyupdater/internal/manifest"
)

// MinCompatibleEMLyVersionV1 / MaxCompatibleEMLyVersionV1 bound the EMLy
// releases known to speak ProtocolVersionV1 (the frozen legacy one-shot
// exchange) — see the compatibility matrix atop proto/updateripc.proto.
// Frozen: never bump these. A new row (V2 below) is where post-handshake
// releases get tracked.
const (
	MinCompatibleEMLyVersionV1 = "1.8.0"
	MaxCompatibleEMLyVersionV1 = "1.8.0"
)

// MinCompatibleEMLyVersionV2 / MaxCompatibleEMLyVersionV2 bound the EMLy
// releases known to speak ProtocolVersion (the v2 handshake). A connecting
// client whose ClientSemverSend.client_version is below Min is rejected
// with ServerSemverReject even though protocol_version matched, since not
// every required fix changes the wire format. Max is informational only
// (logged, never enforced): a newer EMLy is assumed forward-compatible
// unless proven otherwise. Bump Max on every EMLyUpdater release, even one
// that doesn't touch this package; bump Min (and freeze the previous row's
// Max in the proto file's compatibility matrix) only when EMLyUpdater ships
// a build that requires a newer ProtocolVersion.
const (
	MinCompatibleEMLyVersionV2 = "2.1.0"
	MaxCompatibleEMLyVersionV2 = "2.1.0"
)

// checkPeerVersion enforces min against senderVersion and logs (without
// rejecting) a senderVersion newer than max. Fails closed: a missing or
// unparseable senderVersion is treated the same as one that is too old.
func (s *Server) checkPeerVersion(senderVersion, min, max string) error {
	if senderVersion == "" {
		return fmt.Errorf("missing sender_version (requires EMLy >= %s)", min)
	}
	belowMin, err := manifest.Less(senderVersion, min)
	if err != nil {
		return fmt.Errorf("invalid sender_version %q: %w", senderVersion, err)
	}
	if belowMin {
		return fmt.Errorf("EMLy %s is older than the minimum supported %s", senderVersion, min)
	}
	if aboveMax, err := manifest.Less(max, senderVersion); err == nil && aboveMax {
		s.log.Warn("ipc peer version newer than tested range",
			"senderVersion", senderVersion, "maxTested", max)
	}
	return nil
}

// checkPeerVersionV1 enforces the frozen legacy (protocol_version 1)
// compatibility range against a v1 Envelope's sender_version.
func (s *Server) checkPeerVersionV1(senderVersion string) error {
	return s.checkPeerVersion(senderVersion, MinCompatibleEMLyVersionV1, MaxCompatibleEMLyVersionV1)
}

// checkPeerVersionV2 enforces the v2 handshake compatibility range against
// a ClientSemverSend's client_version.
func (s *Server) checkPeerVersionV2(clientVersion string) error {
	return s.checkPeerVersion(clientVersion, MinCompatibleEMLyVersionV2, MaxCompatibleEMLyVersionV2)
}
