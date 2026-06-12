// Package process detects, waits on, and terminates EMLy instances using the
// toolhelp snapshot API and kernel object waits - no busy-polling of the
// process list while EMLy stays open.
package process

import (
	"context"
	"fmt"
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
		pids, err := ListPIDs(exeName)
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
