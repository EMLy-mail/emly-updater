package wsclient

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

type fillReader byte

func (f fillReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(f)
	}
	return len(p), nil
}

// The two ends of the ULID space are fixed by the spec
// (github.com/ulid/spec): all-zero bits, and a 48-bit time plus 80 bits
// of entropy all set, which is the largest valid ULID.
func TestNewIDBounds(t *testing.T) {
	if got := newIDAt(time.UnixMilli(0), fillReader(0)); got != strings.Repeat("0", 26) {
		t.Errorf("zero ULID = %q", got)
	}
	if got := newIDAt(time.UnixMilli(1<<48-1), fillReader(0xFF)); got != "7ZZZZZZZZZZZZZZZZZZZZZZZZZ" {
		t.Errorf("max ULID = %q", got)
	}
}

// The first 10 characters are the millisecond timestamp.
func TestNewIDTimePrefix(t *testing.T) {
	if got := newIDAt(time.UnixMilli(1), fillReader(0))[:10]; got != "0000000001" {
		t.Errorf("time prefix of ms=1 = %q", got)
	}
}

func TestNewIDSortsByTime(t *testing.T) {
	a := newIDAt(time.UnixMilli(1_700_000_000_000), fillReader(0xFF))
	b := newIDAt(time.UnixMilli(1_700_000_000_001), fillReader(0))
	if !(a < b) {
		t.Errorf("%q should sort before %q", a, b)
	}
}

func TestNewIDShapeAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewID()
		if len(id) != 26 {
			t.Fatalf("len(%q) = %d", id, len(id))
		}
		for _, c := range []byte(id) {
			if !bytes.ContainsRune([]byte(crockford), rune(c)) {
				t.Fatalf("%q contains %q, not Crockford base32", id, c)
			}
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
