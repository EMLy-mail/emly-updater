package machineinfo

import (
	"net"
	"testing"
)

func TestInterfacesFromSkipsLoopbackAndDown(t *testing.T) {
	mac, _ := net.ParseMAC("a4:bb:6d:12:34:56")
	ifs := []net.Interface{
		{Index: 1, Name: "Loopback", Flags: net.FlagUp | net.FlagLoopback},
		{Index: 2, Name: "Ethernet", Flags: net.FlagUp, HardwareAddr: mac},
		{Index: 3, Name: "Wi-Fi", Flags: 0},
	}
	addrs := func(i net.Interface) ([]net.Addr, error) {
		if i.Name != "Ethernet" {
			return nil, nil
		}
		_, v4, _ := net.ParseCIDR("172.16.96.42/24")
		v4.IP = net.ParseIP("172.16.96.42")
		_, v6, _ := net.ParseCIDR("fe80::1/64")
		v6.IP = net.ParseIP("fe80::1")
		return []net.Addr{v4, v6}, nil
	}
	got := interfacesFrom(ifs, addrs)
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	e := got[0]
	if e.Name != "Ethernet" || e.MAC != "A4:BB:6D:12:34:56" || len(e.IPv4) != 1 || e.IPv4[0] != "172.16.96.42" || len(e.IPv6) != 1 {
		t.Fatalf("got %+v", e)
	}
}
