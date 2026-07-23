package ipc

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"net"

	"google.golang.org/protobuf/proto"

	"emlyupdater/internal/ipc/ipcpb"
	"emlyupdater/internal/logging"
	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/version"
)

// authNonceSize is the length, in bytes, of the random nonce sent in
// ServerRequestAuthChallenge and HMAC'd (with sharedSecret) by the client
// in ClientAuthResponse.
const authNonceSize = 32

// handleHandshake drives the v2 explicit handshake to completion:
//
//	ClientHello -> ServerAnswHello
//	ClientSemverSend -> ServerSemverOk | ServerSemverReject (closes on reject)
//	ServerRequestAuthChallenge -> ClientAuthResponse (closes on bad HMAC)
//	ClientSystemInfoRequest | ClientADStatusRequest -> Server*Response
//
// conn's deadline and PID/path authentication (verifyClient) have already
// been applied by handleConn. firstTagByte is the tag byte handleConn
// already consumed off the wire to decide this connection is speaking v2 at
// all — it is expected to be FRAME_TYPE_CLIENT_HELLO, the only legal
// opening frame; any other value is rejected like any other out-of-sequence
// frame.
func (s *Server) handleHandshake(conn net.Conn, id clientIdentity, firstTagByte byte) {
	hello := &ipcpb.ClientHello{}
	tag, body, err := readFrameAfterTag(conn, firstTagByte)
	if uErr := unmarshalExpected(tag, body, err, ipcpb.FrameType_FRAME_TYPE_CLIENT_HELLO, hello); uErr != nil {
		s.log.Debug("ipc v2 read failed", "pid", id.PID, "step", "ClientHello", "error", uErr.Error())
		return
	}
	if hello.GetProtocolVersion() != ProtocolVersion {
		s.sendV2Error(conn, id, ipcpb.ErrorCode_ERROR_CODE_UNSUPPORTED_VERSION,
			fmt.Sprintf("unsupported protocol version %d", hello.GetProtocolVersion()))
		return
	}
	if err := writeFrame(conn, ipcpb.FrameType_FRAME_TYPE_SERVER_ANSW_HELLO, &ipcpb.ServerAnswHello{
		ProtocolVersion: ProtocolVersion,
		ServerVersion:   version.Version,
	}); err != nil {
		s.log.Debug("ipc v2 write failed", "pid", id.PID, "step", "ServerAnswHello", "error", err.Error())
		return
	}

	semver := &ipcpb.ClientSemverSend{}
	tag, body, err = readFrame(conn)
	if uErr := unmarshalExpected(tag, body, err, ipcpb.FrameType_FRAME_TYPE_CLIENT_SEMVER_SEND, semver); uErr != nil {
		s.log.Debug("ipc v2 read failed", "pid", id.PID, "step", "ClientSemverSend", "error", uErr.Error())
		return
	}
	if err := s.checkPeerVersionV2(semver.GetClientVersion()); err != nil {
		_ = writeFrame(conn, ipcpb.FrameType_FRAME_TYPE_SERVER_SEMVER_REJECT, &ipcpb.ServerSemverReject{Reason: err.Error()})
		s.log.Debug("ipc v2 client version rejected", "pid", id.PID, "reason", err.Error())
		return
	}
	if err := writeFrame(conn, ipcpb.FrameType_FRAME_TYPE_SERVER_SEMVER_OK, &ipcpb.ServerSemverOk{}); err != nil {
		s.log.Debug("ipc v2 write failed", "pid", id.PID, "step", "ServerSemverOk", "error", err.Error())
		return
	}

	nonce := make([]byte, authNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		s.log.Warn("ipc v2 nonce generation failed", "pid", id.PID, "error", err.Error())
		return
	}
	if err := writeFrame(conn, ipcpb.FrameType_FRAME_TYPE_SERVER_REQUEST_AUTH_CHALLENGE, &ipcpb.ServerRequestAuthChallenge{Nonce: nonce}); err != nil {
		s.log.Debug("ipc v2 write failed", "pid", id.PID, "step", "ServerRequestAuthChallenge", "error", err.Error())
		return
	}

	authResp := &ipcpb.ClientAuthResponse{}
	tag, body, err = readFrame(conn)
	if uErr := unmarshalExpected(tag, body, err, ipcpb.FrameType_FRAME_TYPE_CLIENT_AUTH_RESPONSE, authResp); uErr != nil {
		s.log.Debug("ipc v2 read failed", "pid", id.PID, "step", "ClientAuthResponse", "error", uErr.Error())
		return
	}
	if !validHMAC(nonce, authResp.GetHmac()) {
		s.log.WarnEvent(logging.EventIPCHandshakeFailed, "ipc v2 client failed auth challenge", "pid", id.PID, "path", id.Path)
		s.sendV2Error(conn, id, ipcpb.ErrorCode_ERROR_CODE_UNAUTHORIZED, "unauthorized")
		return
	}

	reqTag, reqBody, err := readFrame(conn)
	if err != nil {
		s.log.Debug("ipc v2 read failed", "pid", id.PID, "step", "payload request", "error", err.Error())
		return
	}
	respTag, resp := s.dispatchV2(reqTag, reqBody)
	if err := writeFrame(conn, respTag, resp); err != nil {
		s.log.Debug("ipc v2 write failed", "pid", id.PID, "step", "payload response", "error", err.Error())
		return
	}
	s.log.Debug("ipc v2 request served", "pid", id.PID)
}

// dispatchV2 handles the payload phase's request tag, mirroring dispatch's
// Envelope-oneof switch but over bare FrameType-tagged messages. Malformed
// request bodies (reqBody fails to unmarshal into the type reqTag implies)
// are reported as ERROR_CODE_BAD_REQUEST alongside an unrecognized tag,
// rather than as a distinct internal error — a client sending a tag it
// isn't prepared to fill in correctly is a protocol violation either way.
func (s *Server) dispatchV2(reqTag ipcpb.FrameType, reqBody []byte) (ipcpb.FrameType, proto.Message) {
	switch reqTag {
	case ipcpb.FrameType_FRAME_TYPE_CLIENT_SYSTEM_INFO_REQUEST:
		if err := proto.Unmarshal(reqBody, &ipcpb.SystemInfoRequest{}); err != nil {
			return ipcpb.FrameType_FRAME_TYPE_SERVER_ERROR, &ipcpb.ErrorResponse{
				Code: ipcpb.ErrorCode_ERROR_CODE_BAD_REQUEST, Message: "malformed request",
			}
		}
		m := s.machine()
		return ipcpb.FrameType_FRAME_TYPE_SERVER_SYSTEM_INFO_RESPONSE, &ipcpb.SystemInfoResponse{
			Hostname:   m.Hostname,
			Hwid:       m.HWID,
			InternalIp: m.InternalIP,
			OsVersion:  m.OSVersion,
		}
	case ipcpb.FrameType_FRAME_TYPE_CLIENT_AD_STATUS_REQUEST:
		if err := proto.Unmarshal(reqBody, &ipcpb.ADStatusRequest{}); err != nil {
			return ipcpb.FrameType_FRAME_TYPE_SERVER_ERROR, &ipcpb.ErrorResponse{
				Code: ipcpb.ErrorCode_ERROR_CODE_BAD_REQUEST, Message: "malformed request",
			}
		}
		m := s.machine()
		return ipcpb.FrameType_FRAME_TYPE_SERVER_AD_STATUS_RESPONSE, &ipcpb.ADStatusResponse{
			AdDomain:     m.ADDomain,
			DomainJoined: machineinfo.DomainJoined(m.ADDomain, m.Hostname),
		}
	default:
		return ipcpb.FrameType_FRAME_TYPE_SERVER_ERROR, &ipcpb.ErrorResponse{
			Code: ipcpb.ErrorCode_ERROR_CODE_BAD_REQUEST, Message: "unrecognized request",
		}
	}
}

// sendV2Error writes a tagged ErrorResponse. Never echoes id.Path/id.PID —
// only into the local log, matching the legacy path's posture (see
// errorEnvelope in server.go).
func (s *Server) sendV2Error(conn net.Conn, id clientIdentity, code ipcpb.ErrorCode, msg string) {
	_ = writeFrame(conn, ipcpb.FrameType_FRAME_TYPE_SERVER_ERROR, &ipcpb.ErrorResponse{Code: code, Message: msg})
}

// validHMAC reports whether got is the correct HMAC-SHA256(sharedSecret,
// nonce), compared in constant time via hmac.Equal.
func validHMAC(nonce, got []byte) bool {
	mac := hmac.New(sha256.New, sharedSecret)
	mac.Write(nonce)
	want := mac.Sum(nil)
	return hmac.Equal(want, got)
}
