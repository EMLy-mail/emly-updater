package machineinfo

import (
	"testing"
	"time"
	"unsafe"
)

func TestParseSessionChange(t *testing.T) {
	at := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	n := wtsSessionNotification{Size: uint32(unsafe.Sizeof(wtsSessionNotification{})), SessionID: 3}

	cases := []struct {
		name      string
		eventType uint32
		data      uintptr
		want      SessionChange
		ok        bool
	}{
		{"console connect", 0x1, uintptr(unsafe.Pointer(&n)), SessionChange{Kind: SessionConsoleConnect, SessionID: 3, At: at}, true},
		{"remote disconnect", 0x4, uintptr(unsafe.Pointer(&n)), SessionChange{Kind: SessionRemoteDisconnect, SessionID: 3, At: at}, true},
		{"terminate", 0xB, uintptr(unsafe.Pointer(&n)), SessionChange{Kind: SessionTerminate, SessionID: 3, At: at}, true},
		{"nil data keeps the event", 0x5, 0, SessionChange{Kind: SessionLogon, At: at}, true},
		{"unknown type", 0x0, uintptr(unsafe.Pointer(&n)), SessionChange{}, false},
		{"unknown type past the table", 0xC, uintptr(unsafe.Pointer(&n)), SessionChange{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseSessionChange(c.eventType, c.data, at)
			if ok != c.ok || got != c.want {
				t.Errorf("ParseSessionChange(%#x) = %+v, %v; want %+v, %v", c.eventType, got, ok, c.want, c.ok)
			}
		})
	}
}
