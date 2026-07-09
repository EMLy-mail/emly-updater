package ipc

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"

	"emlyupdater/internal/ipc/ipcpb"
)

func TestFramingRoundTrip(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	want := &ipcpb.Envelope{
		ProtocolVersion: ProtocolVersion,
		Body: &ipcpb.Envelope_SystemInfoResponse{
			SystemInfoResponse: &ipcpb.SystemInfoResponse{
				Hostname:   "WKS01",
				Hwid:       "abc-123",
				InternalIp: "10.0.0.5",
				OsVersion:  "Windows 11 Pro",
			},
		},
	}

	errCh := make(chan error, 1)
	go func() { errCh <- writeEnvelope(c1, want) }()

	got, err := readEnvelope(c2)
	if err != nil {
		t.Fatalf("readEnvelope: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("writeEnvelope: %v", err)
	}

	resp := got.GetSystemInfoResponse()
	if resp == nil {
		t.Fatal("expected a SystemInfoResponse body")
	}
	if resp.Hostname != "WKS01" || resp.Hwid != "abc-123" || resp.InternalIp != "10.0.0.5" || resp.OsVersion != "Windows 11 Pro" {
		t.Errorf("round-tripped envelope mismatch: %+v", resp)
	}
	if got.GetProtocolVersion() != ProtocolVersion {
		t.Errorf("protocol version = %d, want %d", got.GetProtocolVersion(), ProtocolVersion)
	}
}

func TestReadEnvelopeRejectsOversizedFrame(t *testing.T) {
	var buf bytes.Buffer
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], MaxFrameSize+1)
	buf.Write(lenBuf[:])

	if _, err := readEnvelope(&buf); err == nil {
		t.Fatal("expected error for a frame larger than MaxFrameSize")
	}
}

func TestReadEnvelopeRejectsTruncatedFrame(t *testing.T) {
	var buf bytes.Buffer
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], 10)
	buf.Write(lenBuf[:])
	buf.WriteString("short")

	if _, err := readEnvelope(&buf); err == nil {
		t.Fatal("expected error for a truncated frame body")
	}
}

func TestWriteEnvelopeRejectsOversizedMessage(t *testing.T) {
	env := &ipcpb.Envelope{
		ProtocolVersion: ProtocolVersion,
		Body: &ipcpb.Envelope_Error{
			Error: &ipcpb.ErrorResponse{Message: string(make([]byte, MaxFrameSize+1))},
		},
	}
	if err := writeEnvelope(new(bytes.Buffer), env); err == nil {
		t.Fatal("expected error for an envelope larger than MaxFrameSize")
	}
}
