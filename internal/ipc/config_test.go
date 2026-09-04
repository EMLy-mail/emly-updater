package ipc

import (
	"os"
	"strconv"
	"testing"
	"time"

	"emlyupdater/internal/ipc/ipcpb"
	"emlyupdater/internal/logging"
	"emlyupdater/internal/policy"
)

// testLogger writes to a manually removed directory: lumberjack keeps the
// log file open, which races t.TempDir()'s cleanup on Windows.
func testLogger(t *testing.T) *logging.Logger {
	t.Helper()
	dir, err := os.MkdirTemp("", "ipc-config-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return logging.New(dir, "", false)
}

func ptr[T any](v T) *T { return &v }

// The remote document may only narrow what this binary compiles in: it can
// disable a protocol version or raise the minimum EMLy version, never enable
// a version the updater does not implement nor lower the minimum below what
// the code requires.
func TestEffectiveCompat(t *testing.T) {
	compiled := Compat{Enabled: true, Min: "2.0.0", Max: "2.1.0"}
	key := strconv.Itoa(ProtocolVersion)

	withEntry := func(e policy.IPCVersion) policy.IPCProtocol {
		return policy.IPCProtocol{Versions: map[string]policy.IPCVersion{key: e}, DefaultVersion: ProtocolVersion}
	}

	cases := []struct {
		name   string
		remote policy.IPCProtocol
		want   Compat
	}{
		{
			name:   "a version the document does not list is left as compiled",
			remote: policy.IPCProtocol{Versions: map[string]policy.IPCVersion{"99": {}}, DefaultVersion: 99},
			want:   compiled,
		},
		{
			name:   "a higher minimum is adopted",
			remote: withEntry(policy.IPCVersion{EMLy: policy.VersionRange{Min: ptr("2.0.5"), Max: ptr("2.1.0")}}),
			want:   Compat{Enabled: true, Min: "2.0.5", Max: "2.1.0"},
		},
		{
			name:   "a lower minimum is ignored: the code requires what it requires",
			remote: withEntry(policy.IPCVersion{EMLy: policy.VersionRange{Min: ptr("1.0.0"), Max: ptr("2.1.0")}}),
			want:   Compat{Enabled: true, Min: "2.0.0", Max: "2.1.0"},
		},
		{
			name:   "the maximum is informational, so the document replaces it either way",
			remote: withEntry(policy.IPCVersion{EMLy: policy.VersionRange{Max: ptr("2.4.0")}}),
			want:   Compat{Enabled: true, Min: "2.0.0", Max: "2.4.0"},
		},
		{
			name:   "a null maximum lifts the ceiling",
			remote: withEntry(policy.IPCVersion{EMLy: policy.VersionRange{Min: ptr("2.0.0")}}),
			want:   Compat{Enabled: true, Min: "2.0.0", Max: ""},
		},
		{
			name:   "the document can disable the version outright",
			remote: withEntry(policy.IPCVersion{Enabled: ptr(false), EMLy: policy.VersionRange{Min: ptr("2.0.0"), Max: ptr("2.1.0")}}),
			want:   Compat{Enabled: false, Min: "2.0.0", Max: "2.1.0"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EffectiveCompat(compiled, c.remote, ProtocolVersion); got != c.want {
				t.Errorf("EffectiveCompat() = %+v, want %+v", got, c.want)
			}
		})
	}
}

// The compiled matrix expressed in the document's shape is what the policy
// defaults use, so a document that says nothing about IPC leaves the
// binary's own values in force.
func TestCompiledIPCProtocolRoundTrips(t *testing.T) {
	got := EffectiveCompat(CompiledCompat(), CompiledIPCProtocol(), ProtocolVersion)
	if got != CompiledCompat() {
		t.Errorf("round trip = %+v, want %+v", got, CompiledCompat())
	}
}

// A peer below the raised minimum is rejected even though it would have
// passed the compiled one.
func TestCheckPeerVersionHonoursTheRemoteMinimum(t *testing.T) {
	s := testServer()
	s.log = testLogger(t)
	if err := s.checkPeerVersion("2.0.1"); err != nil {
		t.Fatalf("2.0.1 must pass the compiled minimum: %v", err)
	}

	s.policy = func() *PolicyView {
		return &PolicyView{IPC: policy.IPCProtocol{
			Versions: map[string]policy.IPCVersion{
				strconv.Itoa(ProtocolVersion): {EMLy: policy.VersionRange{Min: ptr("2.5.0")}},
			},
			DefaultVersion: ProtocolVersion,
		}}
	}
	if err := s.checkPeerVersion("2.0.1"); err == nil {
		t.Error("2.0.1 must be rejected once the document raises the minimum to 2.5.0")
	}
	if err := s.checkPeerVersion("2.5.0"); err != nil {
		t.Errorf("2.5.0 must pass the raised minimum: %v", err)
	}
}

// ConfigRequest hands EMLy the effective document plus the provenance it
// needs to tell a live policy from a cached or default one.
func TestDispatchConfig(t *testing.T) {
	s := testServer()
	fetched := time.Date(2026, 9, 5, 9, 0, 0, 0, time.UTC)
	s.policy = func() *PolicyView {
		return &PolicyView{
			DocumentJSON:    []byte(`{"schemaVersion":1,"revision":42}`),
			Revision:        42,
			GeneratedAt:     "2026-09-01T00:00:00Z",
			FetchedAt:       fetched,
			Source:          ipcpb.ConfigResponse_CACHE,
			Stale:           true,
			HostWhitelisted: true,
		}
	}

	resp := s.dispatch(&ipcpb.Envelope{
		ProtocolVersion: ProtocolVersion,
		Body:            &ipcpb.Envelope_ConfigRequest{ConfigRequest: &ipcpb.ConfigRequest{}},
	})
	cfg := resp.GetConfigResponse()
	if cfg == nil {
		t.Fatalf("expected ConfigResponse, got %T", resp.GetBody())
	}
	if string(cfg.DocumentJson) != `{"schemaVersion":1,"revision":42}` {
		t.Errorf("document = %s", cfg.DocumentJson)
	}
	if cfg.Revision != 42 || cfg.GeneratedAt != "2026-09-01T00:00:00Z" {
		t.Errorf("identity = %+v", cfg)
	}
	if cfg.FetchedAt != fetched.Format(time.RFC3339) {
		t.Errorf("fetchedAt = %q", cfg.FetchedAt)
	}
	if cfg.Source != ipcpb.ConfigResponse_CACHE || !cfg.Stale || !cfg.HostWhitelisted {
		t.Errorf("provenance = %+v", cfg)
	}
}

// Without a policy provider the request is an internal error, not a silent
// empty document EMLy would take at face value.
func TestDispatchConfigWithoutAPolicy(t *testing.T) {
	resp := testServer().dispatch(&ipcpb.Envelope{
		ProtocolVersion: ProtocolVersion,
		Body:            &ipcpb.Envelope_ConfigRequest{ConfigRequest: &ipcpb.ConfigRequest{}},
	})
	if resp.GetError().GetCode() != ipcpb.ErrorCode_ERROR_CODE_INTERNAL {
		t.Errorf("expected an internal error, got %T", resp.GetBody())
	}
}
