package ipc

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// clientIdentity is what verifyClient establishes about an accepted
// connection before any request is processed. It is populated best-effort
// even on a rejected connection, purely for logging — never trust it for
// anything else when verifyClient also returned an error.
type clientIdentity struct {
	PID  uint32
	Path string
}

// verifyClient resolves and authenticates the process on the other end of
// conn against the expected EMLy install path.
//
// This never fails open: any error at any step (a failed PID/path lookup is
// treated exactly the same as an explicit path mismatch) means the
// connection must be rejected.
func verifyClient(conn net.Conn, expectedExePath string) (clientIdentity, error) {
	handle, err := handleOf(conn)
	if err != nil {
		return clientIdentity{}, err
	}

	var pid uint32
	if err := windows.GetNamedPipeClientProcessId(handle, &pid); err != nil {
		return clientIdentity{}, fmt.Errorf("ipc: resolving client PID: %w", err)
	}
	id := clientIdentity{PID: pid}

	proc, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return id, fmt.Errorf("ipc: opening client process %d: %w", pid, err)
	}
	defer windows.CloseHandle(proc)

	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(proc, 0, &buf[0], &size); err != nil {
		return id, fmt.Errorf("ipc: resolving client image path for pid %d: %w", pid, err)
	}
	id.Path = windows.UTF16ToString(buf[:size])

	if !strings.EqualFold(filepath.Clean(id.Path), filepath.Clean(expectedExePath)) {
		return id, fmt.Errorf("ipc: client path %q does not match expected %q", id.Path, expectedExePath)
	}

	return id, nil
}
