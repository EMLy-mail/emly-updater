package ipc

import (
	"fmt"
	"net"

	"golang.org/x/sys/windows"
)

// fder matches the promoted Fd() method exposed by go-winio's concrete pipe
// connection types (win32Pipe / win32MessageBytePipe embed *win32File, which
// implements Fd() uintptr). This is an implementation detail of go-winio,
// not a documented interface contract — the go-winio version is pinned in
// go.mod specifically because of this dependency, and the Windows-only
// integration test in server_windows_test.go exercises this exact path
// against a real pipe so a future go-winio upgrade that removes it fails
// loudly instead of silently.
type fder interface {
	Fd() uintptr
}

// handleOf returns the raw pipe handle backing conn, if the concrete type
// still exposes it via a promoted Fd() method.
func handleOf(conn net.Conn) (windows.Handle, error) {
	f, ok := conn.(fder)
	if !ok {
		return 0, fmt.Errorf("ipc: connection type %T does not expose a raw handle", conn)
	}
	return windows.Handle(f.Fd()), nil
}
