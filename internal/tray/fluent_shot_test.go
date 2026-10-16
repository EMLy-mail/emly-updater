//go:build windows

package tray

import (
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"unsafe"

	"github.com/rodrigocfd/windigo/win"
	"golang.org/x/sys/windows"
)

var procPrintWindowShot = user32.NewProc("PrintWindow")

// pwRenderFullContent makes PrintWindow capture DWM-composed content too
// (Windows 8.1+); without it child controls of a layered or DWM-drawn window
// can come out black.
const pwRenderFullContent = 2

// captureWindowPNG renders hwnd, frame and children included, into a PNG at
// path. For the gated Live tests: they cannot assert pixels, so they save a
// screenshot to look at.
func captureWindowPNG(hwnd win.HWND, path string) error {
	var wr windows.Rect
	if r, _, err := procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&wr))); r == 0 {
		return err
	}
	w, h := wr.Right-wr.Left, wr.Bottom-wr.Top
	if w <= 0 || h <= 0 {
		return errors.New("window has no size")
	}
	s, err := newSurface(w, h)
	if err != nil {
		return err
	}
	defer s.free()
	if r, _, err := procPrintWindowShot.Call(uintptr(hwnd), s.dc, pwRenderFullContent); r == 0 {
		return err
	}
	return s.savePNG(w, h, path)
}

// captureScreenPNG copies a screen rectangle into a PNG at path: what the
// user sees there, popups and their shadows included, which PrintWindow on
// a single window cannot show. The area must be on screen and uncovered.
func captureScreenPNG(rc windows.Rect, path string) error {
	w, h := rc.Right-rc.Left, rc.Bottom-rc.Top
	if w <= 0 || h <= 0 {
		return errors.New("empty rectangle")
	}
	s, err := newSurface(w, h)
	if err != nil {
		return err
	}
	defer s.free()
	screen, _, _ := procFdGetDC.Call(0)
	if screen == 0 {
		return errors.New("GetDC failed")
	}
	defer procFdReleaseDC.Call(0, screen)
	const captureBlt = 0x40000000 // include layered windows
	procBitBlt.Call(s.dc, 0, 0, uintptr(w), uintptr(h), screen, uintptr(rc.Left), uintptr(rc.Top), srcCopy|captureBlt)
	return s.savePNG(w, h, path)
}

// savePNG writes the surface's w x h pixels as an opaque PNG at path.
func (s *gdiSurface) savePNG(w, h int32, path string) error {
	img := image.NewNRGBA(image.Rect(0, 0, int(w), int(h)))
	for y := 0; y < int(h); y++ {
		for x := 0; x < int(w); x++ {
			p := s.pix[y*int(w)+x] // BGRA in memory: 0xAARRGGBB as uint32
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(p >> 16), G: uint8(p >> 8), B: uint8(p), A: 255})
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
