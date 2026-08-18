package machineinfo

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modNetapi32          = windows.NewLazySystemDLL("netapi32.dll")
	procDsGetDcNameW     = modNetapi32.NewProc("DsGetDcNameW")
	procNetApiBufferFree = modNetapi32.NewProc("NetApiBufferFree")
)

// dsReturnDNSName asks DsGetDcNameW to return the domain controller's DNS
// name (rather than its NetBIOS name) in DomainControllerName.
const dsReturnDNSName = 0x40000000

// domainControllerInfo mirrors the Win32 DOMAIN_CONTROLLER_INFOW struct.
// Field order and sizes must match the native layout exactly.
type domainControllerInfo struct {
	DomainControllerName        *uint16
	DomainControllerAddress     *uint16
	DomainControllerAddressType uint32
	DomainGUID                  windows.GUID
	DomainName                  *uint16
	DNSForestName               *uint16
	Flags                       uint32
	DCSiteName                  *uint16
	ClientSiteName              *uint16
}

// DomainControllerInfo is the resolved domain controller: its DNS name and
// the AD site it belongs to.
type DomainControllerInfo struct {
	Name string
	Site string
}

// NearestDomainController resolves the domain controller nearest to this
// machine using the native DsGetDcNameW Win32 API (site-aware, no external
// process spawned). Pass an empty domain to resolve the DC for the machine's
// own AD domain.
//
// Unlike the rest of this package it needs the network: on a workgroup
// machine, or before netlogon is ready at boot, it returns an error
// rather than empty values. It is deliberately not part of Collect() for
// that reason - call it on demand, and expect to retry.
func NearestDomainController(domain string) (*DomainControllerInfo, error) {
	var domainPtr *uint16
	if domain != "" {
		var err error
		domainPtr, err = syscall.UTF16PtrFromString(domain)
		if err != nil {
			return nil, fmt.Errorf("invalid domain name %q: %w", domain, err)
		}
	}

	// infoPtr is declared as a typed pointer (rather than uintptr) so the
	// Win32-written pointer value can be read back directly, without a
	// second unsafe.Pointer(uintptr(...)) conversion that `go vet` cannot
	// verify as safe.
	var infoPtr *domainControllerInfo
	ret, _, _ := procDsGetDcNameW.Call(
		0, // ComputerName (NULL = local machine)
		uintptr(unsafe.Pointer(domainPtr)),
		0, // DomainGuid
		0, // SiteName
		uintptr(dsReturnDNSName),
		uintptr(unsafe.Pointer(&infoPtr)),
	)
	if ret != 0 {
		return nil, fmt.Errorf("DsGetDcName failed: %w", syscall.Errno(ret))
	}
	defer procNetApiBufferFree.Call(uintptr(unsafe.Pointer(infoPtr)))

	// DsGetDcName prefixes both names with "\\"; strip it for display/use.
	return &DomainControllerInfo{
		Name: strings.TrimPrefix(windows.UTF16PtrToString(infoPtr.DomainControllerName), `\`),
		Site: windows.UTF16PtrToString(infoPtr.DCSiteName),
	}, nil
}
