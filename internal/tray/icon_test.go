//go:build windows

package tray

import "testing"

func TestIconFromFile(t *testing.T) {
	for _, size := range []int{16, 20, 24, 32, 48, 300} {
		h := iconFromFile(darkIconFile, size)
		if h == 0 {
			t.Fatalf("iconFromFile(appicon_dark.ico, %d) = 0", size)
		}
		_ = h.DestroyIcon()
	}
	for name, ico := range map[string][]byte{
		"empty":     nil,
		"truncated": darkIconFile[:20],
		"no images": {0, 0, 1, 0, 0, 0},
	} {
		if h := iconFromFile(ico, 16); h != 0 {
			_ = h.DestroyIcon()
			t.Errorf("%s: iconFromFile = %v, want 0", name, h)
		}
	}
}
