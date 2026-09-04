package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// CacheFile is the on-disk last-known-good document plus the facts about
// how it got there. FetchedAt is the local clock, which is what staleness
// is measured against; the document's own generatedAt is never compared
// with this machine's time.
type CacheFile struct {
	FetchedAt   time.Time       `json:"fetchedAt"`
	FetchedFrom string          `json:"fetchedFrom"`
	ETag        string          `json:"etag,omitempty"`
	Document    json.RawMessage `json:"document"`
}

// Revision reads the revision out of the raw document without a full parse,
// for the log line that says what the cache held before it was replaced.
func (c *CacheFile) Revision() int64 {
	var head struct {
		Revision int64 `json:"revision"`
	}
	_ = json.Unmarshal(c.Document, &head)
	return head.Revision
}

// LoadCache reads the cache at path. A missing file is (nil, nil): the
// caller falls back to the default policy. A file that is present but not
// readable as a CacheFile is returned as an error so the caller can move it
// aside and say so; validating the document inside it is the caller's job.
func LoadCache(path string) (*CacheFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var c CacheFile
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("corrupt cache file %s: %w", path, err)
	}
	if len(c.Document) == 0 {
		return nil, fmt.Errorf("corrupt cache file %s: no document", path)
	}
	return &c, nil
}

// SaveCache writes c to path atomically: temp file in the same directory,
// then rename, so a crash or a full disk mid-write leaves the previous file
// intact rather than a truncated one.
func SaveCache(path string, c *CacheFile) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "remote-config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// QuarantineCache moves a cache that failed to load or validate to badPath
// (one copy, overwritten) so the next start does not trip over it again and
// the file is still there for whoever wants to look at it.
func QuarantineCache(path, badPath string) error {
	_ = os.Remove(badPath)
	if err := os.Rename(path, badPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
