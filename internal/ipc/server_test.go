package ipc

import (
	"os"
	"testing"

	"emlyupdater/internal/ipc/ipcpb"
	"emlyupdater/internal/logging"
	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/version"
)

func testServer() *Server {
	return &Server{
		machine: func() machineinfo.Info {
			return machineinfo.Info{
				Hostname:   "WKS01",
				HWID:       "hwid-1",
				ADDomain:   "corp.example.com",
				InternalIP: "10.0.0.5",
				OSVersion:  "Windows 11 Pro",
			}
		},
	}
}

func TestDispatchSystemInfo(t *testing.T) {
	s := testServer()
	resp := s.dispatch(&ipcpb.Envelope{
		ProtocolVersion: ProtocolVersion,
		Body:            &ipcpb.Envelope_SystemInfoRequest{SystemInfoRequest: &ipcpb.SystemInfoRequest{}},
	})

	info := resp.GetSystemInfoResponse()
	if info == nil {
		t.Fatalf("expected SystemInfoResponse, got %T", resp.GetBody())
	}
	if info.Hostname != "WKS01" || info.Hwid != "hwid-1" || info.InternalIp != "10.0.0.5" || info.OsVersion != "Windows 11 Pro" {
		t.Errorf("unexpected SystemInfoResponse: %+v", info)
	}
}

func TestDispatchADStatus(t *testing.T) {
	s := testServer()
	resp := s.dispatch(&ipcpb.Envelope{
		ProtocolVersion: ProtocolVersion,
		Body:            &ipcpb.Envelope_AdStatusRequest{AdStatusRequest: &ipcpb.ADStatusRequest{}},
	})

	status := resp.GetAdStatusResponse()
	if status == nil {
		t.Fatalf("expected ADStatusResponse, got %T", resp.GetBody())
	}
	if status.AdDomain != "corp.example.com" || !status.DomainJoined {
		t.Errorf("unexpected ADStatusResponse: %+v", status)
	}
}

func TestDispatchUnrecognizedRequestReturnsError(t *testing.T) {
	s := testServer()
	resp := s.dispatch(&ipcpb.Envelope{ProtocolVersion: ProtocolVersion})

	errResp := resp.GetError()
	if errResp == nil {
		t.Fatalf("expected ErrorResponse, got %T", resp.GetBody())
	}
	if errResp.Code != ipcpb.ErrorCode_ERROR_CODE_BAD_REQUEST {
		t.Errorf("error code = %v, want ERROR_CODE_BAD_REQUEST", errResp.Code)
	}
}

func TestErrorEnvelope(t *testing.T) {
	env := errorEnvelope(ipcpb.ErrorCode_ERROR_CODE_UNAUTHORIZED, "unauthorized")
	errResp := env.GetError()
	if errResp == nil || errResp.Code != ipcpb.ErrorCode_ERROR_CODE_UNAUTHORIZED || errResp.Message != "unauthorized" {
		t.Errorf("unexpected error envelope: %+v", env)
	}
	if env.SenderVersion != version.Version {
		t.Errorf("errorEnvelope sender_version = %q, want %q", env.SenderVersion, version.Version)
	}
}

func TestCheckPeerVersion(t *testing.T) {
	s := testServer()

	// A real Logger is needed since checkPeerVersion may log a warning
	// (the aboveMax case); lumberjack opens the log file lazily on first
	// write and keeps the handle open, which races t.TempDir()'s
	// auto-cleanup on Windows (file still in use) — use a manually
	// removed dir with a best-effort, error-ignoring cleanup instead.
	dir, err := os.MkdirTemp("", "ipc-version-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	s.log = logging.New(dir, "", false)

	tests := []struct {
		name    string
		version string
		wantErr bool
	}{
		{"empty is rejected", "", true},
		{"below min is rejected", "1.7.9", true},
		{"unparseable is rejected", "not-a-version", true},
		{"exactly min is accepted", MinCompatibleEMLyVersion, false},
		{"above max is accepted (logged, not enforced)", "9.9.9", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := s.checkPeerVersion(tc.version)
			if (err != nil) != tc.wantErr {
				t.Errorf("checkPeerVersion(%q) error = %v, wantErr %v", tc.version, err, tc.wantErr)
			}
		})
	}
}
