//go:build windows

package tray

import (
	_ "embed"
	"encoding/binary"

	"github.com/rodrigocfd/windigo/co"
	"github.com/rodrigocfd/windigo/win"
	"golang.org/x/sys/windows/registry"
)

// darkIconFile is the agent's icon in white, for dark backgrounds: the
// exe's own icon (installer/appicon.ico, via exeIcons) is the black wolf on
// transparent, which all but vanishes on a dark taskbar or title bar.
//
//go:embed appicon_dark.ico
var darkIconFile []byte

// darkIcons returns the small and large white icons, sized for the current
// DPI. Zero handles when the embedded file cannot be read.
func darkIcons() (small, large win.HICON) {
	return iconFromFile(darkIconFile, int(win.GetSystemMetrics(co.SM_CXSMICON))),
		iconFromFile(darkIconFile, int(win.GetSystemMetrics(co.SM_CXICON)))
}

// iconFromFile makes an HICON of size x size from an .ico file: the smallest
// image at least that big (the largest one otherwise), scaled by Windows.
// The images may be PNG or DIB; CreateIconFromResourceEx takes both.
func iconFromFile(ico []byte, size int) win.HICON {
	if len(ico) < 6 || size <= 0 {
		return 0
	}
	n := int(binary.LittleEndian.Uint16(ico[4:]))
	var best []byte
	bestW := 0
	for i := range n {
		e := 6 + 16*i
		if e+16 > len(ico) {
			return 0
		}
		w := int(ico[e])
		if w == 0 {
			w = 256
		}
		ln := int(binary.LittleEndian.Uint32(ico[e+8:]))
		off := int(binary.LittleEndian.Uint32(ico[e+12:]))
		if off < 0 || ln <= 0 || off+ln > len(ico) {
			continue
		}
		better := best == nil ||
			(w >= size && (bestW < size || w < bestW)) || // smallest that fits
			(w < size && bestW < size && w > bestW) // or the largest below
		if better {
			best, bestW = ico[off:off+ln], w
		}
	}
	if best == nil {
		return 0
	}
	h, err := win.CreateIconFromResourceEx(best, 0x0003_0000,
		win.SIZE{Cx: int32(size), Cy: int32(size)}, co.LR_DEFAULTCOLOR)
	if err != nil {
		return 0
	}
	return h
}

// taskbarDark reports whether Windows' system mode - the taskbar and its
// notification area - is dark (SystemUsesLightTheme = 0). It is not the app
// mode isDark follows: the two can differ, and the tray icon sits on the
// taskbar whatever the agent's own windows use.
func taskbarDark() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("SystemUsesLightTheme")
	return err == nil && v == 0
}
