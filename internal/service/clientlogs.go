package service

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"time"

	"emlyupdater/internal/logging"
	"emlyupdater/internal/wsclient"
)

// logReadWindow is how much of the end of updater.log log.send_latest reads
// at most. Text logs gzip about 10:1, so the ~44 KB of gzip a frame has room
// for holds roughly 400-500 KB of log: 2 MiB leaves ample margin and keeps
// the search below cheap even when logging.maxSizeMB lets the file grow to
// 100 MB.
const logReadWindow = 2 << 20

// sendLatestLog is log.send_latest: the tail of the current updater.log,
// gzipped and base64'd into a single result frame. The newest lines are the
// ones that matter for a diagnosis, so it is the start of the file that is
// dropped when the whole does not fit - never the end - and the cut is always
// at a line boundary.
//
// Only the current file is sent, not the rotated updater-*.log.gz backups: a
// frame cannot hold more than the current file's tail anyway.
func (u *Updater) sendLatestLog() (any, *wsclient.ErrorBody, bool) {
	path := u.Log.FilePath()
	if u.logFilePathFn != nil {
		path = u.logFilePathFn()
	}
	t, err := logging.ReadTail(path, logReadWindow)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, refuse(wsclient.ErrNotFound, "no log file at "+path), false
	case err != nil:
		return nil, refuse(wsclient.ErrInternal, err.Error()), false
	}

	p := wsclient.LogFile{
		Name:        filepath.Base(path),
		SizeBytes:   t.Size,
		Level:       u.Log.Level(),
		Encoding:    wsclient.LogEncodingGzipBase64,
		CollectedAt: u.clock().UTC().Format(time.RFC3339),
	}
	if !t.ModTime.IsZero() {
		p.ModifiedAt = t.ModTime.UTC().Format(time.RFC3339)
	}
	start := fitLogTail(t.Data, t.Offset > 0, func(content string, from int) bool {
		p.Offset, p.IncludedBytes, p.Content = t.Offset+int64(from), int64(len(t.Data)-from), content
		b, _ := json.Marshal(p)
		return len(b) <= resultPayloadBudget
	})
	// fitLogTail leaves p set from its last attempt, which is not always the
	// one it settled on: set it again from the answer.
	p.Offset, p.IncludedBytes, p.Content = t.Offset+int64(start), int64(len(t.Data)-start), gzipBase64(t.Data[start:])
	return p, nil, p.Offset > 0
}

// fitLogTail returns the earliest line start in data from which the
// gzip+base64 of the rest satisfies fits, or len(data) when not even the last
// line does. midLine says data[0] is not a line start (the read began inside
// the file), so the partial first line is never a candidate.
//
// gzip's output only roughly grows with its input, so this binary search
// finds a start that fits and is close to the earliest one, not provably the
// earliest - a line or two of difference in a 400 KB tail.
func fitLogTail(data []byte, midLine bool, fits func(content string, from int) bool) int {
	var starts []int
	if !midLine {
		starts = append(starts, 0)
	}
	for i, c := range data {
		if c == '\n' && i+1 < len(data) {
			starts = append(starts, i+1)
		}
	}
	i := sort.Search(len(starts), func(i int) bool {
		return fits(gzipBase64(data[starts[i]:]), starts[i])
	})
	if i == len(starts) {
		return len(data)
	}
	return starts[i]
}

func gzipBase64(b []byte) string {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}
