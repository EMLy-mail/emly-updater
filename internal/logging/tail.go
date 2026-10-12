package logging

import (
	"io"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// Tail is the end of a log file as ReadTail found it.
type Tail struct {
	// Data is the last bytes of the file, at most the maxBytes ReadTail was given.
	Data []byte
	// Offset is where Data starts in the file.
	Offset int64
	// Size is the file's size when it was read.
	Size    int64
	ModTime time.Time
}

// ReadTail reads at most maxBytes bytes from the end of path.
//
// The handle is opened with FILE_SHARE_DELETE as well as read/write sharing:
// the file is the live updater.log, and lumberjack rotates it by renaming it
// away while it is open - a plain os.Open would make that rename fail for as
// long as the read lasts, and lumberjack drops the line it was writing.
func ReadTail(path string, maxBytes int64) (Tail, error) {
	f, err := openShared(path)
	if err != nil {
		return Tail{}, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return Tail{}, err
	}
	t := Tail{Size: fi.Size(), ModTime: fi.ModTime()}
	t.Offset = max(t.Size-maxBytes, 0)
	// Read up to Size as of the Stat, not to EOF: the service keeps writing
	// while this runs, and a tail that grows past maxBytes would break the
	// caller's budget.
	t.Data = make([]byte, t.Size-t.Offset)
	n, err := f.ReadAt(t.Data, t.Offset)
	if err != nil && err != io.EOF {
		return Tail{}, err
	}
	t.Data = t.Data[:n]
	return t, nil
}

// openShared opens path for reading without denying anyone else read, write
// or delete/rename access.
func openShared(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
