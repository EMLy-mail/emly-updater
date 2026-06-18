// Package machineinfo collects the host-identifying data sent as EMLy
// request headers (hostname, hardware ID, AD domain, internal IP).
package machineinfo

import (
	"net"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// Info holds the four values sent as X-EMLy-* request headers.
type Info struct {
	Hostname   string
	HWID       string
	ADDomain   string
	InternalIP string
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

	return info
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
