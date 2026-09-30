package product

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// readExeVersion returns the fixed VERSIONINFO of the executable at path:
// ProductVersion, or FileVersion when ProductVersion is all zeros, as
// "a.b.c.d". A missing file wraps fs.ErrNotExist; an exe without VERSIONINFO
// is an error (the file exists, so the chain reports Unknown).
func readExeVersion(path string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	size, err := windows.GetFileVersionInfoSize(path, nil)
	if err != nil || size == 0 {
		return "", fmt.Errorf("%s has no VERSIONINFO: %v", path, err)
	}
	buf := make([]byte, size)
	if err := windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&buf[0])); err != nil {
		return "", fmt.Errorf("read VERSIONINFO of %s: %w", path, err)
	}
	var fixed *windows.VS_FIXEDFILEINFO
	var fixedLen uint32
	if err := windows.VerQueryValue(unsafe.Pointer(&buf[0]), `\`, unsafe.Pointer(&fixed), &fixedLen); err != nil || fixed == nil {
		return "", fmt.Errorf("%s has no fixed VERSIONINFO: %v", path, err)
	}
	ms, ls := fixed.ProductVersionMS, fixed.ProductVersionLS
	if ms == 0 && ls == 0 {
		ms, ls = fixed.FileVersionMS, fixed.FileVersionLS
	}
	if ms == 0 && ls == 0 {
		return "", fmt.Errorf("%s reports version 0.0.0.0", path)
	}
	return fmt.Sprintf("%d.%d.%d.%d", ms>>16, ms&0xffff, ls>>16, ls&0xffff), nil
}
