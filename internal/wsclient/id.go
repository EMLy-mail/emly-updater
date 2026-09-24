// This file is a verbatim copy of emly-go-api/internal/clientproto/id.go:
// both ends mint message IDs and they must look the same.
package wsclient

import (
	"crypto/rand"
	"io"
	"time"
)

// crockford is the ULID alphabet: base32 without I, L, O, U.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewID returns a ULID (48-bit ms time + 80 random bits, 26 Crockford
// base32 chars). Verbatim copy of emly-go-api/internal/clientproto/id.go:
// both ends mint message IDs and they must look the same.
func NewID() string { return newIDAt(time.Now(), rand.Reader) }

func newIDAt(t time.Time, entropy io.Reader) string {
	var b [16]byte
	ms := uint64(t.UnixMilli())
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (40 - 8*i))
	}
	if _, err := io.ReadFull(entropy, b[6:]); err != nil {
		clear(b[6:])
	}
	var out [26]byte
	for i := range out {
		var v byte
		for j := 0; j < 5; j++ {
			p := i*5 + j - 2
			v <<= 1
			if p >= 0 && b[p/8]&(0x80>>(p%8)) != 0 {
				v |= 1
			}
		}
		out[i] = crockford[v]
	}
	return string(out[:])
}
