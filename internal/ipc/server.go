// Package ipc implements a secure Windows named-pipe server that exposes
// SystemInfo and AD status to the local EMLy client. The service runs as
// LocalSystem; the pipe DACL and per-connection client verification (see
// auth.go) are what make tampering with this channel require Administrator
// rather than merely being logged in.
package ipc

import (
	"context"
	"errors"
	"net"
	"time"

	winio "github.com/Microsoft/go-winio"

	"emlyupdater/internal/config"
	"emlyupdater/internal/ipc/ipcpb"
	"emlyupdater/internal/logging"
	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/policy"
	"emlyupdater/internal/version"
)

// PolicyView is what the IPC server needs from the remote configuration: the
// effective document for this host, ready to serialise, plus the
// compatibility section it enforces on connecting clients. The service
// rebuilds it once per poll cycle; the server only reads it.
type PolicyView struct {
	DocumentJSON    []byte
	Revision        int64
	GeneratedAt     string
	FetchedAt       time.Time
	Source          ipcpb.ConfigResponse_Source
	Stale           bool
	HostWhitelisted bool
	IPC             policy.IPCProtocol
}

// ProtocolVersion is bumped on any wire-incompatible change to
// proto/updateripc.proto. Requests carrying a different version are
// rejected rather than guessed at.
const ProtocolVersion = 1

// connDeadline bounds how long a single accepted connection may take to
// authenticate, send its request and receive its response, so a slow or
// hostile client cannot tie up a server goroutine indefinitely.
const connDeadline = 5 * time.Second

// Server serves SystemInfo/ADStatus/Config over a named pipe to the EMLy client.
type Server struct {
	cfg     *config.Config
	log     *logging.Logger
	machine func() machineinfo.Info
	exePath string
	// policy supplies the current PolicyView; nil (or a nil result) means
	// "no remote configuration wired in" and the compiled-in behaviour
	// applies. It is called on the connection goroutine, so it must be
	// cheap and safe for concurrent use.
	policy func() *PolicyView
}

// New builds a Server. machine supplies the SystemInfo/ADStatus payload for
// every request — pass a function returning an already-collected snapshot
// (machineinfo.Collect() shells out to PowerShell for the AD domain and,
// where WMIC is unavailable, for the HWID; it must run once at service
// startup, not per IPC request). exePath is the
// canonical expected EMLy.exe path (assoc.ExePath(cfg.EMLyInstallDir,
// cfg.EMLyExeName)) that a connecting client's own image path must match.
func New(cfg *config.Config, log *logging.Logger, machine func() machineinfo.Info, exePath string) *Server {
	return &Server{cfg: cfg, log: log, machine: machine, exePath: exePath}
}

// SetPolicyProvider wires in the remote configuration. Must be called
// before Serve.
func (s *Server) SetPolicyProvider(p func() *PolicyView) { s.policy = p }

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
		// Never echo the path/PID back to the client — only into the local log.
		_ = writeEnvelope(conn, errorEnvelope(ipcpb.ErrorCode_ERROR_CODE_UNAUTHORIZED, "unauthorized"))
		return
	}

	req, err := readEnvelope(conn)
	if err != nil {
		s.log.Debug("ipc read failed", "pid", id.PID, "error", err.Error())
		return
	}
	if req.GetProtocolVersion() != ProtocolVersion {
		_ = writeEnvelope(conn, errorEnvelope(ipcpb.ErrorCode_ERROR_CODE_UNSUPPORTED_VERSION, "unsupported protocol version"))
		return
	}
	if err := s.checkPeerVersion(req.GetSenderVersion()); err != nil {
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
	case *ipcpb.Envelope_ConfigRequest:
		if s.policy == nil {
			return errorEnvelope(ipcpb.ErrorCode_ERROR_CODE_INTERNAL, "remote configuration not available")
		}
		view := s.policy()
		if view == nil {
			return errorEnvelope(ipcpb.ErrorCode_ERROR_CODE_INTERNAL, "remote configuration not available")
		}
		fetched := ""
		if !view.FetchedAt.IsZero() {
			fetched = view.FetchedAt.Format(time.RFC3339)
		}
		return &ipcpb.Envelope{
			ProtocolVersion: ProtocolVersion,
			SenderVersion:   version.Version,
			Body: &ipcpb.Envelope_ConfigResponse{
				ConfigResponse: &ipcpb.ConfigResponse{
					DocumentJson:    view.DocumentJSON,
					Revision:        view.Revision,
					GeneratedAt:     view.GeneratedAt,
					FetchedAt:       fetched,
					Source:          view.Source,
					Stale:           view.Stale,
					HostWhitelisted: view.HostWhitelisted,
				},
			},
		}
	default:
		return errorEnvelope(ipcpb.ErrorCode_ERROR_CODE_BAD_REQUEST, "unrecognized request")
	}
}

func errorEnvelope(code ipcpb.ErrorCode, msg string) *ipcpb.Envelope {
	return &ipcpb.Envelope{
		ProtocolVersion: ProtocolVersion,
		SenderVersion:   version.Version,
		Body: &ipcpb.Envelope_Error{
			Error: &ipcpb.ErrorResponse{Code: code, Message: msg},
		},
	}
}
