// Package machineinfo collects the host-identifying data sent as EMLy
// request headers (hostname, hardware ID, AD domain, internal IP).
package machineinfo

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// Info holds machine identity data: the four values sent as X-EMLy-*
// request headers, plus OSVersion (added for the IPC SystemInfo payload,
// not sent as a header).
type Info struct {
	Hostname   string
	HWID       string
	ADDomain   string
	InternalIP string
	OSVersion  string
}

// Collect gathers machine identity data. Fields that cannot be retrieved are
// left empty; callers should handle missing values gracefully.
func Collect() Info {
	info := Info{}

	if h, err := os.Hostname(); err == nil {
		info.Hostname = h
	}

	if hwid, err := machineGUID(); err == nil {
		info.HWID = hwid
	}

	if domain, err := adDomain(); err == nil {
		info.ADDomain = domain
	}

	if ip, err := internalIP(); err == nil {
		info.InternalIP = ip
	}

	if v, err := osVersion(); err == nil {
		info.OSVersion = v
	}

	return info
}

// DomainJoined derives whether the machine is AD domain-joined from the
// already-collected ADDomain string: WMI's Win32_ComputerSystem.Domain
// returns the local workgroup name ("WORKGROUP") or the hostname itself
// when the machine is not domain-joined, rather than an empty string.
func DomainJoined(adDomain, hostname string) bool {
	d := strings.TrimSpace(adDomain)
	if d == "" || strings.EqualFold(d, "WORKGROUP") {
		return false
	}
	return !strings.EqualFold(d, hostname)
}

// machineGUID reads the persistent machine GUID from the Windows registry.
// This is the same source used by the machineid library as its Windows backend.
func machineGUID() (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer k.Close()
	guid, _, err := k.GetStringValue("MachineGuid")
	return guid, err
}

// adDomain queries the Active Directory domain via WMI, mirroring the
// approach used by emly-app's machine-identifier.go.
func adDomain() (string, error) {
	out, err := exec.Command(
		"powershell", "-NoProfile", "-NonInteractive", "-Command",
		"(Get-WmiObject -Class Win32_ComputerSystem).Domain",
	).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// osVersion reads a human-readable OS version string from the registry,
// e.g. "Windows 11 Pro 23H2 (Build 22631.3007)".
func osVersion() (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer k.Close()

	product, _, _ := k.GetStringValue("ProductName")
	display, _, _ := k.GetStringValue("DisplayVersion")
	build, _, _ := k.GetStringValue("CurrentBuild")
	ubr, _, _ := k.GetIntegerValue("UBR")

	v := strings.TrimSpace(product)
	if display != "" {
		v += " " + display
	}
	if build != "" {
		v += fmt.Sprintf(" (Build %s.%d)", build, ubr)
	}
	return strings.TrimSpace(v), nil
}

// internalIP returns the first non-loopback IPv4 address found on an up
// interface, matching the selection logic in emly-app's machine-identifier.go.
func internalIP() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip4 := ip.To4(); ip4 != nil && !ip4.IsLoopback() {
				return ip4.String(), nil
			}
		}
	}
	return "", nil
}
