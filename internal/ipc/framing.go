package ipc

import (
	"encoding/binary"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"

	"emlyupdater/internal/ipc/ipcpb"
)

// MaxFrameSize bounds a single Envelope's wire size, guarding against a
// hostile or buggy client exhausting memory via a bogus length prefix.
const MaxFrameSize = 64 * 1024

// writeEnvelope writes env to w as [4-byte big-endian length][protobuf bytes].
// This explicit length-prefix framing (rather than relying on named-pipe
// message mode) keeps the codec plain Go, testable with net.Pipe() and no
// Windows API calls.
func writeEnvelope(w io.Writer, env *ipcpb.Envelope) error {
	b, err := proto.Marshal(env)
	if err != nil {
		return fmt.Errorf("ipc: marshal envelope: %w", err)
	}
	if len(b) > MaxFrameSize {
		return fmt.Errorf("ipc: envelope too large (%d bytes)", len(b))
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(b)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return fmt.Errorf("ipc: write length prefix: %w", err)
	}
	if _, err := w.Write(b); err != nil {
		return fmt.Errorf("ipc: write envelope: %w", err)
	}
	return nil
}

// readEnvelope reads one length-prefixed Envelope from r, rejecting frames
// larger than MaxFrameSize before allocating a buffer for them.
func readEnvelope(r io.Reader) (*ipcpb.Envelope, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, fmt.Errorf("ipc: read length prefix: %w", err)
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n > MaxFrameSize {
		return nil, fmt.Errorf("ipc: frame too large (%d bytes)", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("ipc: read envelope: %w", err)
	}
	env := &ipcpb.Envelope{}
	if err := proto.Unmarshal(buf, env); err != nil {
		return nil, fmt.Errorf("ipc: unmarshal envelope: %w", err)
	}
	return env, nil
}
