package ipc

import "golang.org/x/sys/windows"

// pipeSDDL is the security descriptor applied to the pipe's first instance
// (go-winio's ListenPipe only accepts a SecurityDescriptor on first==true;
// every subsequent per-connection instance inherits it).
//
// SY (LocalSystem, the service) and BA (Administrators) get full control.
// AU (Authenticated Users) get exactly ClientAccessMask below — connect,
// read and write — and nothing more.
//
// This deliberately does NOT grant GENERIC_WRITE/FILE_GENERIC_WRITE to AU.
// On a named pipe object those map to a mask that includes FILE_APPEND_DATA
// (0x4), which is bit-identical to FILE_CREATE_PIPE_INSTANCE for a pipe
// object: granting it would let any authenticated user create a competing
// instance of this pipe name (squat it) before or instead of the service.
// Enumerating the exact rights avoids that.
const pipeSDDL = "D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x120083;;;AU)"

// ClientAccessMask is the exact desired-access value a client must request
// when dialing this pipe (e.g. via winio.DialPipeAccess): READ_CONTROL |
// SYNCHRONIZE | FILE_READ_DATA | FILE_WRITE_DATA | FILE_READ_ATTRIBUTES,
// numerically 0x120083 — matching (and never exceeding) what pipeSDDL grants
// to AU above. A plain winio.DialPipe/DialPipeContext requests
// GENERIC_READ|GENERIC_WRITE instead and will be denied by this DACL.
//
// The EMLy client (a separate repository/module) cannot import this Go
// constant directly; it defines the identical numeric value itself. Keep
// both definitions in sync if this mask ever changes.
const ClientAccessMask = windows.FILE_READ_DATA | windows.FILE_WRITE_DATA |
	windows.FILE_READ_ATTRIBUTES | windows.STANDARD_RIGHTS_READ | windows.SYNCHRONIZE
