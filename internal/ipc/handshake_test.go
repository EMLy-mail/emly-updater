package ipc

import (
	"crypto/hmac"
	"crypto/sha256"
	"io"
	"net"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"emlyupdater/internal/ipc/ipcpb"
)

// serveHandshake spins up s.handleHandshake on c1 in a goroutine, mimicking
// the first-byte peek handleConn performs before dispatching to it, and
// returns a channel closed once handleHandshake returns.
func serveHandshake(t *testing.T, s *Server, c1 net.Conn) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var first [1]byte
		if _, err := io.ReadFull(c1, first[:]); err != nil {
			return
		}
		s.handleHandshake(c1, clientIdentity{PID: 1, Path: "test"}, first[0])
	}()
	return done
}

func TestHandshakeHappyPath(t *testing.T) {
	s := testServerWithLog(t)
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	done := serveHandshake(t, s, c1)

	if err := writeFrame(c2, ipcpb.FrameType_FRAME_TYPE_CLIENT_HELLO, &ipcpb.ClientHello{ProtocolVersion: ProtocolVersion}); err != nil {
		t.Fatalf("write ClientHello: %v", err)
	}
	tag, body, err := readFrame(c2)
	if err != nil {
		t.Fatalf("read ServerAnswHello: %v", err)
	}
	hello := &ipcpb.ServerAnswHello{}
	if err := unmarshalExpected(tag, body, nil, ipcpb.FrameType_FRAME_TYPE_SERVER_ANSW_HELLO, hello); err != nil {
		t.Fatalf("ServerAnswHello: %v", err)
	}
	if hello.GetProtocolVersion() != ProtocolVersion {
		t.Errorf("ServerAnswHello.protocol_version = %d, want %d", hello.GetProtocolVersion(), ProtocolVersion)
	}

	if err := writeFrame(c2, ipcpb.FrameType_FRAME_TYPE_CLIENT_SEMVER_SEND, &ipcpb.ClientSemverSend{ClientVersion: MinCompatibleEMLyVersionV2}); err != nil {
		t.Fatalf("write ClientSemverSend: %v", err)
	}
	tag, _, err = readFrame(c2)
	if err != nil {
		t.Fatalf("read ServerSemverOk: %v", err)
	}
	if tag != ipcpb.FrameType_FRAME_TYPE_SERVER_SEMVER_OK {
		t.Fatalf("tag = %v, want FRAME_TYPE_SERVER_SEMVER_OK", tag)
	}

	tag, body, err = readFrame(c2)
	if err != nil {
		t.Fatalf("read ServerRequestAuthChallenge: %v", err)
	}
	challenge := &ipcpb.ServerRequestAuthChallenge{}
	if err := unmarshalExpected(tag, body, nil, ipcpb.FrameType_FRAME_TYPE_SERVER_REQUEST_AUTH_CHALLENGE, challenge); err != nil {
		t.Fatalf("ServerRequestAuthChallenge: %v", err)
	}
	if len(challenge.GetNonce()) != authNonceSize {
		t.Fatalf("nonce length = %d, want %d", len(challenge.GetNonce()), authNonceSize)
	}

	mac := hmac.New(sha256.New, sharedSecret)
	mac.Write(challenge.GetNonce())
	if err := writeFrame(c2, ipcpb.FrameType_FRAME_TYPE_CLIENT_AUTH_RESPONSE, &ipcpb.ClientAuthResponse{Hmac: mac.Sum(nil)}); err != nil {
		t.Fatalf("write ClientAuthResponse: %v", err)
	}

	if err := writeFrame(c2, ipcpb.FrameType_FRAME_TYPE_CLIENT_SYSTEM_INFO_REQUEST, &ipcpb.SystemInfoRequest{}); err != nil {
		t.Fatalf("write ClientSystemInfoRequest: %v", err)
	}
	tag, body, err = readFrame(c2)
	if err != nil {
		t.Fatalf("read SystemInfoResponse: %v", err)
	}
	resp := &ipcpb.SystemInfoResponse{}
	if err := unmarshalExpected(tag, body, nil, ipcpb.FrameType_FRAME_TYPE_SERVER_SYSTEM_INFO_RESPONSE, resp); err != nil {
		t.Fatalf("SystemInfoResponse: %v", err)
	}
	if resp.Hostname != "WKS01" || resp.Hwid != "hwid-1" {
		t.Errorf("unexpected SystemInfoResponse: %+v", resp)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleHandshake did not return")
	}
}

func TestHandshakeRejectsBadHMAC(t *testing.T) {
	s := testServerWithLog(t)
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	done := serveHandshake(t, s, c1)

	if err := writeFrame(c2, ipcpb.FrameType_FRAME_TYPE_CLIENT_HELLO, &ipcpb.ClientHello{ProtocolVersion: ProtocolVersion}); err != nil {
		t.Fatalf("write ClientHello: %v", err)
	}
	if _, _, err := readFrame(c2); err != nil {
		t.Fatalf("read ServerAnswHello: %v", err)
	}
	if err := writeFrame(c2, ipcpb.FrameType_FRAME_TYPE_CLIENT_SEMVER_SEND, &ipcpb.ClientSemverSend{ClientVersion: MinCompatibleEMLyVersionV2}); err != nil {
		t.Fatalf("write ClientSemverSend: %v", err)
	}
	if _, _, err := readFrame(c2); err != nil {
		t.Fatalf("read ServerSemverOk: %v", err)
	}
	if _, _, err := readFrame(c2); err != nil {
		t.Fatalf("read ServerRequestAuthChallenge: %v", err)
	}

	if err := writeFrame(c2, ipcpb.FrameType_FRAME_TYPE_CLIENT_AUTH_RESPONSE, &ipcpb.ClientAuthResponse{Hmac: []byte("wrong-hmac-wrong-hmac-wrong-hmac")}); err != nil {
		t.Fatalf("write ClientAuthResponse: %v", err)
	}

	tag, body, err := readFrame(c2)
	if err != nil {
		t.Fatalf("read error response: %v", err)
	}
	if tag != ipcpb.FrameType_FRAME_TYPE_SERVER_ERROR {
		t.Fatalf("tag = %v, want FRAME_TYPE_SERVER_ERROR", tag)
	}
	errResp := &ipcpb.ErrorResponse{}
	if err := unmarshalExpected(tag, body, nil, ipcpb.FrameType_FRAME_TYPE_SERVER_ERROR, errResp); err != nil {
		t.Fatalf("ErrorResponse: %v", err)
	}
	if errResp.Code != ipcpb.ErrorCode_ERROR_CODE_UNAUTHORIZED {
		t.Errorf("error code = %v, want ERROR_CODE_UNAUTHORIZED", errResp.Code)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleHandshake did not return")
	}
}

func TestHandshakeRejectsOldSemver(t *testing.T) {
	s := testServerWithLog(t)
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	done := serveHandshake(t, s, c1)

	if err := writeFrame(c2, ipcpb.FrameType_FRAME_TYPE_CLIENT_HELLO, &ipcpb.ClientHello{ProtocolVersion: ProtocolVersion}); err != nil {
		t.Fatalf("write ClientHello: %v", err)
	}
	if _, _, err := readFrame(c2); err != nil {
		t.Fatalf("read ServerAnswHello: %v", err)
	}
	if err := writeFrame(c2, ipcpb.FrameType_FRAME_TYPE_CLIENT_SEMVER_SEND, &ipcpb.ClientSemverSend{ClientVersion: "2.0.0"}); err != nil {
		t.Fatalf("write ClientSemverSend: %v", err)
	}

	tag, body, err := readFrame(c2)
	if err != nil {
		t.Fatalf("read ServerSemverReject: %v", err)
	}
	if tag != ipcpb.FrameType_FRAME_TYPE_SERVER_SEMVER_REJECT {
		t.Fatalf("tag = %v, want FRAME_TYPE_SERVER_SEMVER_REJECT", tag)
	}
	reject := &ipcpb.ServerSemverReject{}
	if err := unmarshalExpected(tag, body, nil, ipcpb.FrameType_FRAME_TYPE_SERVER_SEMVER_REJECT, reject); err != nil {
		t.Fatalf("ServerSemverReject: %v", err)
	}
	if reject.GetReason() == "" {
		t.Error("expected a non-empty rejection reason")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleHandshake did not return")
	}
}

func TestHandshakeRejectsUnexpectedFirstFrame(t *testing.T) {
	s := testServerWithLog(t)
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	done := serveHandshake(t, s, c1)

	// Client sends ClientSemverSend as its very first frame instead of
	// ClientHello — an out-of-sequence frame the server must reject
	// without hanging, rather than misinterpreting it.
	writeErrCh := make(chan error, 1)
	go func() {
		writeErrCh <- writeFrame(c2, ipcpb.FrameType_FRAME_TYPE_CLIENT_SEMVER_SEND, &ipcpb.ClientSemverSend{ClientVersion: "2.1.0"})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleHandshake did not return after an out-of-sequence first frame")
	}
	if err := <-writeErrCh; err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
}

func TestDispatchV2SystemInfo(t *testing.T) {
	s := testServer()
	body, err := proto.Marshal(&ipcpb.SystemInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	tag, resp := s.dispatchV2(ipcpb.FrameType_FRAME_TYPE_CLIENT_SYSTEM_INFO_REQUEST, body)
	if tag != ipcpb.FrameType_FRAME_TYPE_SERVER_SYSTEM_INFO_RESPONSE {
		t.Fatalf("tag = %v, want FRAME_TYPE_SERVER_SYSTEM_INFO_RESPONSE", tag)
	}
	info, ok := resp.(*ipcpb.SystemInfoResponse)
	if !ok {
		t.Fatalf("resp type = %T, want *SystemInfoResponse", resp)
	}
	if info.Hostname != "WKS01" || info.Hwid != "hwid-1" || info.InternalIp != "10.0.0.5" || info.OsVersion != "Windows 11 Pro" {
		t.Errorf("unexpected SystemInfoResponse: %+v", info)
	}
}

func TestDispatchV2ADStatus(t *testing.T) {
	s := testServer()
	body, err := proto.Marshal(&ipcpb.ADStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	tag, resp := s.dispatchV2(ipcpb.FrameType_FRAME_TYPE_CLIENT_AD_STATUS_REQUEST, body)
	if tag != ipcpb.FrameType_FRAME_TYPE_SERVER_AD_STATUS_RESPONSE {
		t.Fatalf("tag = %v, want FRAME_TYPE_SERVER_AD_STATUS_RESPONSE", tag)
	}
	status, ok := resp.(*ipcpb.ADStatusResponse)
	if !ok {
		t.Fatalf("resp type = %T, want *ADStatusResponse", resp)
	}
	if status.AdDomain != "corp.example.com" || !status.DomainJoined {
		t.Errorf("unexpected ADStatusResponse: %+v", status)
	}
}

func TestDispatchV2UnrecognizedTagReturnsError(t *testing.T) {
	s := testServer()
	tag, resp := s.dispatchV2(ipcpb.FrameType_FRAME_TYPE_UNSPECIFIED, nil)
	if tag != ipcpb.FrameType_FRAME_TYPE_SERVER_ERROR {
		t.Fatalf("tag = %v, want FRAME_TYPE_SERVER_ERROR", tag)
	}
	errResp, ok := resp.(*ipcpb.ErrorResponse)
	if !ok {
		t.Fatalf("resp type = %T, want *ErrorResponse", resp)
	}
	if errResp.Code != ipcpb.ErrorCode_ERROR_CODE_BAD_REQUEST {
		t.Errorf("error code = %v, want ERROR_CODE_BAD_REQUEST", errResp.Code)
	}
}
