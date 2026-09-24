package machineinfo

import (
	"net"
	"strings"
)

// NetInterface is one up, non-loopback adapter for machine.info
// (CLIENT_WS_PROTOCOL.md §6.5).
type NetInterface struct {
	Name string   `json:"name"`
	MAC  string   `json:"mac,omitempty"`
	IPv4 []string `json:"ipv4"`
	IPv6 []string `json:"ipv6"`
}

func NetworkInterfaces() []NetInterface {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	return interfacesFrom(ifs, func(i net.Interface) ([]net.Addr, error) { return i.Addrs() })
}

func interfacesFrom(ifs []net.Interface, addrs func(net.Interface) ([]net.Addr, error)) []NetInterface {
	var out []NetInterface
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		n := NetInterface{Name: i.Name, MAC: strings.ToUpper(i.HardwareAddr.String()), IPv4: []string{}, IPv6: []string{}}
		list, _ := addrs(i)
		for _, a := range list {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if v4 := ipnet.IP.To4(); v4 != nil {
				n.IPv4 = append(n.IPv4, v4.String())
			} else {
				n.IPv6 = append(n.IPv6, ipnet.IP.String())
			}
		}
		out = append(out, n)
	}
	return out
}
