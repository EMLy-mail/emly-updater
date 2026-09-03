// Package machineinfo collects the host-identifying data sent as EMLy
// request headers (hostname, hardware ID, AD domain, internal IP).
package machineinfo

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/denisbrodbeck/machineid"
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

	if hwid, err := hardwareID(); err == nil {
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

// hwidAppID keys the machineid.ProtectedID fallback. It must stay identical
// to the app ID emly-app passes, or the two agents would report different
// HWIDs for the same machine.
const hwidAppID = "emly-machine-id"

// hardwareID returns this machine's hardware identifier, mirroring
// emly-app's utils.getHWID so both agents report the same X-EMLy-HWID for a
// given machine.
//
// The preferred source is the SMBIOS UUID, which comes from firmware: unlike
// MachineGuid it survives an OS reinstall and, more importantly, differs
// between machines deployed from the same image when that image was not
// sysprepped.
//
// When no usable SMBIOS UUID is available we fall back to what emly-app falls
// back to: machineid.ProtectedID, i.e. HMAC-SHA256 of hwidAppID keyed by
// HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid. That value is per
// OS-install, so clones of a non-sysprepped image do share it.
func hardwareID() (string, error) {
	if uuid := smbiosUUID(); uuid != "" {
		return uuid, nil
	}
	return machineid.ProtectedID(hwidAppID)
}

// smbiosUUID reads Win32_ComputerSystemProduct.UUID, returning "" when it is
// unavailable or not a usable identifier.
//
// emly-app reads it with `wmic csproduct get uuid`. WMIC is deprecated and no
// longer present on current Windows 11 builds, so we try it first (it is the
// cheaper of the two when installed) and fall back to the same WMI class via
// PowerShell, which yields a byte-identical value.
func smbiosUUID() string {
	if out, err := hiddenCommand("wmic", "csproduct", "get", "uuid").Output(); err == nil {
		if u := parseWMICUUID(string(out)); u != "" {
			return u
		}
	}

	out, err := hiddenCommand(
		"powershell", "-NoProfile", "-NonInteractive", "-Command",
		"(Get-CimInstance -ClassName Win32_ComputerSystemProduct).UUID",
	).Output()
	if err != nil {
		return ""
	}
	return normalizeUUID(string(out))
}

// parseWMICUUID pulls the UUID out of `wmic csproduct get uuid` output, which
// looks like "UUID \r\r\n<uuid>   \r\r\n\r\r\n": a column header, then the
// value padded with trailing spaces.
func parseWMICUUID(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.EqualFold(line, "UUID") {
			continue
		}
		return normalizeUUID(line)
	}
	return ""
}

// normalizeUUID upper-cases and trims an SMBIOS UUID, rejecting the
// placeholder values some firmware reports. An all-zero or all-F UUID is not
// an identifier: every affected machine would report the same one, which is
// the collision this whole code path exists to avoid. WMIC already returns
// upper-case, so normalizing does not move us off emly-app's value.
func normalizeUUID(s string) string {
	u := strings.ToUpper(strings.TrimSpace(s))
	switch u {
	case "", "00000000-0000-0000-0000-000000000000", "FFFFFFFF-FFFF-FFFF-FFFF-FFFFFFFFFFFF":
		return ""
	}
	return u
}

// hiddenCommand builds an exec.Cmd that spawns no console window. The updater
// normally runs as a service in session 0 where this is moot, but the same
// code runs from the interactive CLI.
func hiddenCommand(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	return cmd
}

// adDomain queries the Active Directory domain via WMI, mirroring the
// approach used by emly-app's machine-identifier.go.
func adDomain() (string, error) {
	out, err := hiddenCommand(
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
	ips, err := LocalIPv4Addresses()
	if err != nil {
		return "", err
	}
	if len(ips) == 0 {
		return "", nil
	}
	return ips[0], nil
}

// LocalIPv4Addresses returns every non-loopback IPv4 address bound to an up
// interface on this machine, in the order net.Interfaces() reports them. A
// machine can present more than one (VPN adapter, wired + wireless, ...); the
// startup source policy checks all of them against the subnets configured
// for the resolved domain controller, rather than picking just one.
func LocalIPv4Addresses() ([]string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []string
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
				out = append(out, ip4.String())
			}
		}
	}
	return out, nil
}
