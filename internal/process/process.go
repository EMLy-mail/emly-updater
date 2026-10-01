// Package process detects, waits on, and terminates EMLy instances using the
// toolhelp snapshot API and kernel object waits - no busy-polling of the
// process list while EMLy stays open.
package process

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ListPIDs returns the PIDs of every process whose image name matches exeName
// (case-insensitive, e.g. "EMLy.exe").
func ListPIDs(exeName string) ([]uint32, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("CreateToolhelp32Snapshot failed: %w", err)
	}
	defer windows.CloseHandle(snapshot)

	var pids []uint32
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))

	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil, fmt.Errorf("Process32First failed: %w", err)
	}
	for {
		name := windows.UTF16ToString(entry.ExeFile[:])
		if strings.EqualFold(name, exeName) {
			pids = append(pids, entry.ProcessID)
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if err == syscall.ERROR_NO_MORE_FILES {
				break
			}
			return nil, fmt.Errorf("Process32Next failed: %w", err)
		}
	}
	return pids, nil
}

// IsRunning reports whether at least one exeName instance exists. Snapshot
// errors are treated as "running" so the caller takes the conservative
// (queue-and-wait) path instead of killing or installing blindly.
func IsRunning(exeName string) bool {
	pids, err := ListPIDs(exeName)
	if err != nil {
		return true
	}
	return len(pids) > 0
}

// WaitForExit blocks until no exeName instance is left, the context is
// cancelled, or an unrecoverable wait error occurs. Instead of polling the
// process list, it holds SYNCHRONIZE handles to the current instances and
// sleeps in WaitForMultipleObjects; after any instance exits it re-snapshots,
// which also catches instances launched while waiting.
func WaitForExit(ctx context.Context, exeName string) error {
	return waitForExit(ctx, func() ([]uint32, error) { return ListPIDs(exeName) })
}

// waitForExit is WaitForExit over any PID lister, re-run after every exit.
func waitForExit(ctx context.Context, list func() ([]uint32, error)) error {
	// A manual-reset event bridges context cancellation into the Win32 wait.
	cancelEvent, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return fmt.Errorf("CreateEvent failed: %w", err)
	}
	defer windows.CloseHandle(cancelEvent)

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = windows.SetEvent(cancelEvent)
		case <-stop:
		}
	}()

	for {
		pids, err := list()
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			return nil
		}

		// WaitForMultipleObjects tops out at 64 handles; one slot is reserved
		// for the cancel event. More than 63 EMLy instances is implausible,
		// but cap defensively - the survivors are picked up on re-snapshot.
		if len(pids) > 63 {
			pids = pids[:63]
		}

		handles := []windows.Handle{cancelEvent}
		for _, pid := range pids {
			h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
			if err != nil {
				continue // already gone or inaccessible; re-snapshot below
			}
			handles = append(handles, h)
		}

		if len(handles) == 1 {
			// Every OpenProcess failed - the instances raced to exit between
			// snapshot and open. Loop to confirm via a fresh snapshot.
			continue
		}

		event, err := windows.WaitForMultipleObjects(handles, false, windows.INFINITE)
		for _, h := range handles[1:] {
			windows.CloseHandle(h)
		}
		if err != nil {
			return fmt.Errorf("WaitForMultipleObjects failed: %w", err)
		}
		if event == windows.WAIT_OBJECT_0 { // index 0 = cancel event
			return ctx.Err()
		}
		// One process handle signaled: re-snapshot and either wait on the
		// remaining instances or return.
	}
}

// TerminateAll force-kills every exeName instance (no graceful IPC - this is
// the critical-update path) and waits briefly for each to disappear.
// Returns how many processes were terminated.
func TerminateAll(exeName string) (int, error) {
	pids, err := ListPIDs(exeName)
	if err != nil {
		return 0, err
	}
	return terminatePIDs(pids)
}

// terminatePIDs force-kills pids and waits briefly for each to disappear.
func terminatePIDs(pids []uint32) (int, error) {
	killed := 0
	var firstErr error
	for _, pid := range pids {
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("OpenProcess(%d) failed: %w", pid, err)
			}
			continue
		}
		if err := windows.TerminateProcess(h, 1); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("TerminateProcess(%d) failed: %w", pid, err)
			}
			windows.CloseHandle(h)
			continue
		}
		// TerminateProcess is asynchronous; give the kernel a moment so the
		// installer does not race a dying process holding file locks.
		_, _ = windows.WaitForSingleObject(h, uint32(5*time.Second/time.Millisecond))
		windows.CloseHandle(h)
		killed++
	}
	return killed, firstErr
}

// The *Under variants below are for products other than EMLy. Their exeName
// comes from the remote-configuration document and the service runs as
// SYSTEM, so a name match alone could reach any process on the machine that
// happens to share it. They consider only instances whose image lives inside
// the product's installDir.

// ListPIDsUnder is ListPIDs restricted to processes whose full image path is
// inside dir. A process whose image path cannot be read (gone, protected) is
// left out: it cannot be shown to be the product's.
func ListPIDsUnder(exeName, dir string) ([]uint32, error) {
	pids, err := ListPIDs(exeName)
	if err != nil {
		return nil, err
	}
	var out []uint32
	for _, pid := range pids {
		path, err := imagePath(pid)
		if err != nil {
			continue
		}
		if pathUnder(path, dir) {
			out = append(out, pid)
		}
	}
	return out, nil
}

// IsRunningUnder is IsRunning restricted to instances inside dir. Snapshot
// errors still count as "running" (the conservative path).
func IsRunningUnder(exeName, dir string) bool {
	pids, err := ListPIDsUnder(exeName, dir)
	if err != nil {
		return true
	}
	return len(pids) > 0
}

// TerminateAllUnder is TerminateAll restricted to instances inside dir.
func TerminateAllUnder(exeName, dir string) (int, error) {
	pids, err := ListPIDsUnder(exeName, dir)
	if err != nil {
		return 0, err
	}
	return terminatePIDs(pids)
}

// WaitForExitUnder is WaitForExit restricted to instances inside dir. An
// instance of the same name launched elsewhere while waiting is ignored.
func WaitForExitUnder(ctx context.Context, exeName, dir string) error {
	return waitForExit(ctx, func() ([]uint32, error) { return ListPIDsUnder(exeName, dir) })
}

// imagePath returns the full Win32 path of pid's executable.
func imagePath(pid uint32) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:size]), nil
}

// pathUnder reports whether path lies inside dir: case-insensitive, and on a
// separator boundary, so C:\3gIT\X does not contain C:\3gIT\XY\a.exe. An
// empty or relative dir contains nothing.
func pathUnder(path, dir string) bool {
	if dir == "" || !filepath.IsAbs(dir) {
		return false
	}
	dir = strings.TrimRight(filepath.Clean(dir), `\`)
	path = filepath.Clean(path)
	if len(path) <= len(dir)+1 || !strings.EqualFold(path[:len(dir)], dir) {
		return false
	}
	return path[len(dir)] == '\\'
}
