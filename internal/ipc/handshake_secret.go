package ipc

// sharedSecret authenticates ClientAuthResponse's HMAC during the v2
// handshake (see handshake.go). It is a static value compiled into both
// EMLyUpdater and EMLy — defense in depth layered on top of the pipe ACL
// (sddl.go) and the per-connection PID/path check (verifyClient in
// auth.go), not a replacement for either.
//
// THIS MUST STAY IDENTICAL, BYTE FOR BYTE, to
// emly/backend/utils/updateripc/handshake_secret.go. There is no shared Go
// module between the two repos (same posture as proto/updateripc.proto) —
// nothing enforces this automatically, so a one-sided edit makes every v2
// ClientAuthResponse fail HMAC verification and rejects every client.
var sharedSecret = []byte{
	0xc3, 0xb5, 0x17, 0x64, 0xdb, 0x97, 0x66, 0x31,
	0x4e, 0x2f, 0x2b, 0xb8, 0x6e, 0x73, 0xa1, 0x62,
	0x9b, 0xf2, 0xf1, 0xf2, 0x9b, 0x38, 0x96, 0x85,
	0x52, 0x38, 0xcc, 0x36, 0xea, 0x38, 0xeb, 0x90,
}
