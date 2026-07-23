package ipc

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"

	"emlyupdater/internal/ipc/ipcpb"
)

func TestFrameRoundTrip(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	want := &ipcpb.ServerAnswHello{ProtocolVersion: ProtocolVersion, ServerVersion: "1.3.0"}

	errCh := make(chan error, 1)
	go func() { errCh <- writeFrame(c1, ipcpb.FrameType_FRAME_TYPE_SERVER_ANSW_HELLO, want) }()

	tag, body, err := readFrame(c2)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	if tag != ipcpb.FrameType_FRAME_TYPE_SERVER_ANSW_HELLO {
		t.Errorf("tag = %v, want FRAME_TYPE_SERVER_ANSW_HELLO", tag)
	}

	got := &ipcpb.ServerAnswHello{}
	if err := unmarshalExpected(tag, body, nil, ipcpb.FrameType_FRAME_TYPE_SERVER_ANSW_HELLO, got); err != nil {
		t.Fatalf("unmarshalExpected: %v", err)
	}
	if got.GetProtocolVersion() != ProtocolVersion || got.GetServerVersion() != "1.3.0" {
		t.Errorf("round-tripped frame mismatch: %+v", got)
	}
}

func TestWriteFrameRejectsReservedTag(t *testing.T) {
	if err := writeFrame(new(bytes.Buffer), ipcpb.FrameType_FRAME_TYPE_UNSPECIFIED, &ipcpb.ClientHello{}); err == nil {
		t.Fatal("expected error writing FRAME_TYPE_UNSPECIFIED")
	}
}

func TestWriteFrameRejectsOversizedMessage(t *testing.T) {
	msg := &ipcpb.ServerSemverReject{Reason: string(make([]byte, MaxFrameSize+1))}
	if err := writeFrame(new(bytes.Buffer), ipcpb.FrameType_FRAME_TYPE_SERVER_SEMVER_REJECT, msg); err == nil {
		t.Fatal("expected error for a frame larger than MaxFrameSize")
	}
}

func TestReadFrameRejectsOversizedFrame(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(byte(ipcpb.FrameType_FRAME_TYPE_CLIENT_HELLO))
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], MaxFrameSize+1)
	buf.Write(lenBuf[:])

	if _, _, err := readFrame(&buf); err == nil {
		t.Fatal("expected error for a frame larger than MaxFrameSize")
	}
}

func TestReadFrameRejectsTruncatedFrame(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(byte(ipcpb.FrameType_FRAME_TYPE_CLIENT_HELLO))
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], 10)
	buf.Write(lenBuf[:])
	buf.WriteString("short")

	if _, _, err := readFrame(&buf); err == nil {
		t.Fatal("expected error for a truncated frame body")
	}
}

func TestUnmarshalExpectedRejectsWrongTag(t *testing.T) {
	err := unmarshalExpected(ipcpb.FrameType_FRAME_TYPE_SERVER_SEMVER_REJECT, []byte{}, nil,
		ipcpb.FrameType_FRAME_TYPE_SERVER_SEMVER_OK, &ipcpb.ServerSemverOk{})
	if err == nil {
		t.Fatal("expected error for a frame type mismatch")
	}
}

// TestLegacyLengthPrefixFirstByteAlwaysZero pins down the determinism claim
// the dual-protocol server relies on (see handleConn): for any v1-valid
// Envelope length (0..MaxFrameSize), the big-endian length prefix's first
// (most-significant) byte is always 0x00, since MaxFrameSize (65536) fits
// in the low 17 bits of a uint32. This is what lets handleConn treat a
// first byte of 0x00 as unambiguously "legacy client", and any other byte
// as a v2 FrameType tag.
func TestLegacyLengthPrefixFirstByteAlwaysZero(t *testing.T) {
	for _, n := range []uint32{0, 1, MaxFrameSize - 1, MaxFrameSize} {
		var lenBuf [4]byte
		binary.BigEndian.PutUint32(lenBuf[:], n)
		if lenBuf[0] != 0 {
			t.Errorf("length %d: first byte = 0x%02x, want 0x00", n, lenBuf[0])
		}
	}
}
