package machineinfo

import (
	"os"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// SystemFacts are the hardware/uptime facts of machine.info. Every field
// is best-effort: zero means "could not read it", and the caller omits it.
type SystemFacts struct {
	BootTime           time.Time
	CPU                string
	Cores              int
	MemoryMB           uint64
	SystemDriveTotalMB uint64
	SystemDriveFreeMB  uint64
}

var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGetTickCount64       = kernel32.NewProc("GetTickCount64")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

// memoryStatusEx mirrors MEMORYSTATUSEX.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func CollectSystemFacts(now time.Time) SystemFacts {
	f := SystemFacts{Cores: runtime.NumCPU()}

	if r, _, _ := procGetTickCount64.Call(); r != 0 {
		f.BootTime = now.Add(-time.Duration(r) * time.Millisecond).UTC().Truncate(time.Second)
	}

	if k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE); err == nil {
		if s, _, err := k.GetStringValue("ProcessorNameString"); err == nil {
			f.CPU = strings.TrimSpace(s)
		}
		k.Close()
	}

	m := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r != 0 {
		f.MemoryMB = m.TotalPhys / (1 << 20)
	}

	drive := os.Getenv("SystemDrive")
	if drive == "" {
		drive = "C:"
	}
	if p, err := windows.UTF16PtrFromString(drive + `\`); err == nil {
		var free, total, totalFree uint64
		if windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree) == nil {
			f.SystemDriveTotalMB, f.SystemDriveFreeMB = total/(1<<20), free/(1<<20)
		}
	}
	return f
}
