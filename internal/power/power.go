// Package power reboots the machine for the client channel's
// machine.reboot command. Windows only; not unit tested (it would reboot
// the test machine) - see AGENTS.md's manual verification list.
package power

import (
	"fmt"
	"time"

	"golang.org/x/sys/windows"
)

// Reboot asks Windows to restart after delay. Windows itself shows the
// countdown to the logged-on users, so there is no dialog of our own.
// Applications are closed without saving when the countdown ends: the user
// was warned for at least the delay.
func Reboot(delay time.Duration) error {
	if err := enableShutdownPrivilege(); err != nil {
		return err
	}
	reason := uint32(windows.SHTDN_REASON_MAJOR_APPLICATION | windows.SHTDN_REASON_MINOR_MAINTENANCE | windows.SHTDN_REASON_FLAG_PLANNED)
	if err := windows.InitiateSystemShutdownEx(nil, nil, uint32(delay/time.Second), true, true, reason); err != nil {
		return fmt.Errorf("InitiateSystemShutdownEx: %w", err)
	}
	return nil
}

// enableShutdownPrivilege enables SeShutdownPrivilege in this process's own
// token (it is commonly present but disabled by default even for
// LocalSystem). Mirrors notify.enablePrivilege's API usage.
func enableShutdownPrivilege() error {
	var procToken windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &procToken); err != nil {
		return fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer procToken.Close()

	namePtr, err := windows.UTF16PtrFromString("SeShutdownPrivilege")
	if err != nil {
		return err
	}
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, namePtr, &luid); err != nil {
		return fmt.Errorf("LookupPrivilegeValue(SeShutdownPrivilege): %w", err)
	}

	tp := windows.Tokenprivileges{
		PrivilegeCount: 1,
		Privileges: [1]windows.LUIDAndAttributes{
			{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED},
		},
	}
	if err := windows.AdjustTokenPrivileges(procToken, false, &tp, 0, nil, nil); err != nil {
		return fmt.Errorf("AdjustTokenPrivileges(SeShutdownPrivilege): %w", err)
	}
	return nil
}
