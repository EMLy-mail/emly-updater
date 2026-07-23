// Package ipc implements a secure Windows named-pipe server that exposes
// SystemInfo and AD status to the local EMLy client. The service runs as
// LocalSystem; the pipe DACL and per-connection client verification (see
// auth.go) are what make tampering with this channel require Administrator
// rather than merely being logged in.
package ipc

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	winio "github.com/Microsoft/go-winio"

	"emlyupdater/internal/config"
	"emlyupdater/internal/ipc/ipcpb"
	"emlyupdater/internal/logging"
	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/version"
)

// ProtocolVersion is bumped on any wire-incompatible change to
// proto/updateripc.proto. Requests carrying a different version are
// rejected rather than guessed at. This server also still understands
// ProtocolVersionV1's frozen one-shot Envelope exchange — see handleConn's
// dual-protocol dispatch — so already-deployed EMLy builds older than 2.1.0
// keep working unmodified against a 1.3.0+ EMLyUpdater.
const ProtocolVersion = 2

// ProtocolVersionV1 is the frozen wire version of the legacy one-shot
// Envelope exchange (proto/updateripc.proto's "v1" section). Never bump
// this — it identifies already-shipped EMLy clients, not this server.
const ProtocolVersionV1 = 1

// connDeadline bounds how long a single accepted connection may take to
// authenticate and complete its exchange, so a slow or hostile client
// cannot tie up a server goroutine indefinitely. The v2 handshake is four
// sequential round trips instead of v1's one; on a local named pipe each
// round trip is sub-millisecond, so 5s was already ~1000x headroom for a
// single round trip. 10s preserves that same proportionate headroom for
// the full v2 handshake without opening an unbounded hostile-hold window.
const connDeadline = 10 * time.Second

// Server serves SystemInfo/ADStatus over a named pipe to the EMLy client.
type Server struct {
	cfg     *config.Config
	log     *logging.Logger
	machine func() machineinfo.Info
	exePath string
}

// New builds a Server. machine supplies the SystemInfo/ADStatus payload for
// every request — pass a function returning an already-collected snapshot
// (machineinfo.Collect() shells out to PowerShell for the AD domain; it must
// run once at service startup, not per IPC request). exePath is the
// canonical expected EMLy.exe path (assoc.ExePath(cfg.EMLyInstallDir,
// cfg.EMLyExeName)) that a connecting client's own image path must match.
func New(cfg *config.Config, log *logging.Logger, machine func() machineinfo.Info, exePath string) *Server {
	return &Server{cfg: cfg, log: log, machine: machine, exePath: exePath}
}

// Serve accepts connections until ctx is cancelled or the listener fails
// fatally. It returns immediately as a no-op if IPC is disabled.
func (s *Server) Serve(ctx context.Context) {
	if !s.cfg.IPCEnabled {
		s.log.Debug("ipc disabled by config, not starting")
		return
	}

	path := `\\.\pipe\` + s.cfg.IPCPipeName
	ln, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: pipeSDDL})
	if err != nil {
		// ListenPipe uses FILE_CREATE disposition on the first instance, so
		// this fails loudly (rather than silently becoming a second
		// instance) if the pipe name is already taken — e.g. squatted by
		// another process before this service started.
		s.log.ErrorEvent(logging.EventIPCUnavailable,
			"failed to create IPC pipe, possibly already in use", "pipe", path, "error", err.Error())
		return
	}
	defer ln.Close()

	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			ln.Close()
		case <-stopped:
		}
	}()
	defer close(stopped)

	s.log.Info("ipc server listening", "pipe", path)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) || errors.Is(err, winio.ErrPipeListenerClosed) {
				s.log.Info("ipc server stopped")
				return
			}
			s.log.Warn("ipc accept failed", "error", err.Error())
			continue
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(connDeadline))

	s.log.Debug("ipc client connected", "exePath", s.exePath)

	id, err := verifyClient(conn, s.exePath)
	if err != nil {
		s.log.WarnEvent(logging.EventIPCRejected, "ipc client rejected",
			"pid", id.PID, "path", id.Path, "reason", err.Error())
		// Never echo the path/PID back to the client — only into the local
		// log. No wire read has happened yet, so the server doesn't know
		// which protocol dialect the peer speaks; the legacy Envelope shape
		// is the one shape every client (v1 and v2) can be made to decode
		// — see emly's readFirstFrame.
		_ = writeEnvelope(conn, errorEnvelope(ipcpb.ErrorCode_ERROR_CODE_UNAUTHORIZED, "unauthorized"))
		return
	}

	// Dual-protocol dispatch: a legacy (protocol_version 1) client's first
	// wire byte is always 0x00 (the most-significant byte of its 4-byte
	// length prefix, which is always <= MaxFrameSize = 65536). Any other
	// first byte is a v2 FrameType tag. See the FrameType doc comment in
	// proto/updateripc.proto.
	var first [1]byte
	if _, err := io.ReadFull(conn, first[:]); err != nil {
		s.log.Debug("ipc read failed", "pid", id.PID, "error", err.Error())
		return
	}
	if first[0] == 0 {
		s.handleLegacyConn(conn, id, first[0])
		return
	}
	s.handleHandshake(conn, id, first[0])
}

// handleLegacyConn serves the frozen v1 one-shot exchange for a connection
// whose first length-prefix byte (firstLenByte, always 0x00) handleConn has
// already read. Behavior is otherwise identical to the pre-v2 handleConn.
func (s *Server) handleLegacyConn(conn net.Conn, id clientIdentity, firstLenByte byte) {
	var lenBuf [4]byte
	lenBuf[0] = firstLenByte
	if _, err := io.ReadFull(conn, lenBuf[1:]); err != nil {
		s.log.Debug("ipc read failed", "pid", id.PID, "error", err.Error())
		return
	}
	req, err := readEnvelopeAfterLengthPrefix(conn, lenBuf)
	if err != nil {
		s.log.Debug("ipc read failed", "pid", id.PID, "error", err.Error())
		return
	}
	if req.GetProtocolVersion() != ProtocolVersionV1 {
		_ = writeEnvelope(conn, errorEnvelope(ipcpb.ErrorCode_ERROR_CODE_UNSUPPORTED_VERSION, "unsupported protocol version"))
		return
	}
	if err := s.checkPeerVersionV1(req.GetSenderVersion()); err != nil {
		s.log.Debug("ipc client version rejected", "pid", id.PID, "reason", err.Error())
		_ = writeEnvelope(conn, errorEnvelope(ipcpb.ErrorCode_ERROR_CODE_UNSUPPORTED_VERSION, err.Error()))
		return
	}

	resp := s.dispatch(req)
	if err := writeEnvelope(conn, resp); err != nil {
		s.log.Debug("ipc write failed", "pid", id.PID, "error", err.Error())
		return
	}
	s.log.Debug("ipc request served", "pid", id.PID)
}

func (s *Server) dispatch(req *ipcpb.Envelope) *ipcpb.Envelope {
	switch req.GetBody().(type) {
	case *ipcpb.Envelope_SystemInfoRequest:
		m := s.machine()
		return &ipcpb.Envelope{
			ProtocolVersion: ProtocolVersion,
			SenderVersion:   version.Version,
			Body: &ipcpb.Envelope_SystemInfoResponse{
				SystemInfoResponse: &ipcpb.SystemInfoResponse{
					Hostname:   m.Hostname,
					Hwid:       m.HWID,
					InternalIp: m.InternalIP,
					OsVersion:  m.OSVersion,
				},
			},
		}
	case *ipcpb.Envelope_AdStatusRequest:
		m := s.machine()
		return &ipcpb.Envelope{
			ProtocolVersion: ProtocolVersion,
			SenderVersion:   version.Version,
			Body: &ipcpb.Envelope_AdStatusResponse{
				AdStatusResponse: &ipcpb.ADStatusResponse{
					AdDomain:     m.ADDomain,
					DomainJoined: machineinfo.DomainJoined(m.ADDomain, m.Hostname),
				},
			},
		}
	default:
		return errorEnvelope(ipcpb.ErrorCode_ERROR_CODE_BAD_REQUEST, "unrecognized request")
	}
}

// errorEnvelope always builds a legacy (v1) Envelope, since every caller
// sends it either before the peer's dialect is known (the pre-handshake
// UNAUTHORIZED case) or from the legacy path itself.
func errorEnvelope(code ipcpb.ErrorCode, msg string) *ipcpb.Envelope {
	return &ipcpb.Envelope{
		ProtocolVersion: ProtocolVersionV1,
		SenderVersion:   version.Version,
		Body: &ipcpb.Envelope_Error{
			Error: &ipcpb.ErrorResponse{Code: code, Message: msg},
		},
	}
}
