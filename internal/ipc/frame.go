package ipc

import (
	"encoding/binary"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"

	"emlyupdater/internal/ipc/ipcpb"
)

// writeFrame writes a v2 frame to w as
// [1-byte FrameType tag][4-byte big-endian length][protobuf bytes]. See the
// FrameType doc comment in proto/updateripc.proto for the wire format and
// why FRAME_TYPE_UNSPECIFIED (tag 0) must never be written.
func writeFrame(w io.Writer, tag ipcpb.FrameType, msg proto.Message) error {
	if tag == ipcpb.FrameType_FRAME_TYPE_UNSPECIFIED {
		return fmt.Errorf("ipc: refusing to write reserved FrameType_FRAME_TYPE_UNSPECIFIED")
	}
	b, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("ipc: marshal %T: %w", msg, err)
	}
	if len(b) > MaxFrameSize {
		return fmt.Errorf("ipc: frame too large (%d bytes)", len(b))
	}
	var head [5]byte
	head[0] = byte(tag)
	binary.BigEndian.PutUint32(head[1:], uint32(len(b)))
	if _, err := w.Write(head[:]); err != nil {
		return fmt.Errorf("ipc: write frame header: %w", err)
	}
	if len(b) == 0 {
		// Several v2 messages (ServerSemverOk, SystemInfoRequest, ...)
		// marshal to zero bytes. Skip the body write entirely rather than
		// calling w.Write(nil): a zero-length Write still requires a
		// rendezvous with a matching Read on some io.ReadWriter
		// implementations (notably net.Pipe, used by this package's
		// tests), while io.ReadFull on a zero-length destination buffer
		// never issues that Read — the two sides would deadlock waiting
		// on each other over zero bytes neither needs to transfer.
		return nil
	}
	if _, err := w.Write(b); err != nil {
		return fmt.Errorf("ipc: write frame body: %w", err)
	}
	return nil
}

// readFrame reads one tag-prefixed v2 frame from r, returning its FrameType
// and raw (still-marshaled) body.
func readFrame(r io.Reader) (ipcpb.FrameType, []byte, error) {
	var tagBuf [1]byte
	if _, err := io.ReadFull(r, tagBuf[:]); err != nil {
		return 0, nil, fmt.Errorf("ipc: read frame tag: %w", err)
	}
	return readFrameAfterTag(r, tagBuf[0])
}

// readFrameAfterTag reads the remainder of a v2 frame whose tag byte
// (tagByte) has already been read off the wire. handleConn uses this to
// resume a v2 read after consuming the connection's first byte to decide,
// versus a legacy v1 client, which protocol dialect it is speaking — see
// the FrameType doc comment in proto/updateripc.proto.
func readFrameAfterTag(r io.Reader, tagByte byte) (ipcpb.FrameType, []byte, error) {
	tag := ipcpb.FrameType(tagByte)
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return tag, nil, fmt.Errorf("ipc: read frame length: %w", err)
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n > MaxFrameSize {
		return tag, nil, fmt.Errorf("ipc: frame too large (%d bytes)", n)
	}
	if n == 0 {
		// Mirror writeFrame's skip of the body write for an empty message —
		// see its comment for why io.ReadFull must not be called here.
		return tag, []byte{}, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return tag, nil, fmt.Errorf("ipc: read frame body: %w", err)
	}
	return tag, buf, nil
}

// unmarshalExpected checks a (tag, body, err) triple as returned by
// readFrame/readFrameAfterTag against the FrameType expected at the current
// handshake step, then unmarshals body into out. Centralizing this check
// means every step in handleHandshake rejects an out-of-sequence frame the
// same way, regardless of which read produced it.
func unmarshalExpected(tag ipcpb.FrameType, body []byte, readErr error, want ipcpb.FrameType, out proto.Message) error {
	if readErr != nil {
		return readErr
	}
	if tag != want {
		return fmt.Errorf("ipc: expected frame type %s, got %s", want, tag)
	}
	return proto.Unmarshal(body, out)
}
