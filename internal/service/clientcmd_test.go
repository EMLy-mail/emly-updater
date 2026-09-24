package service

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"emlyupdater/internal/winget"
	"emlyupdater/internal/wsclient"
)

type fakeSession struct {
	secure  bool
	mu      sync.Mutex
	acks    []wsclient.Ack
	results []wsclient.Result
	closed  websocket.StatusCode
}

func (f *fakeSession) Secure() bool { return f.secure }
func (f *fakeSession) Ack(_ context.Context, _ string, a wsclient.Ack) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acks = append(f.acks, a)
	return nil
}
func (f *fakeSession) Result(_ context.Context, _ string, r wsclient.Result) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results = append(f.results, r)
	return nil
}
func (f *fakeSession) Close(code websocket.StatusCode, _ string) { f.closed = code }

func cmdMsg(name string, args string) (wsclient.Message, wsclient.Command) {
	now := time.Now().UTC()
	msg := wsclient.Message{Type: wsclient.TypeCommand, ID: wsclient.NewID(), TS: now.Format(time.RFC3339)}
	return msg, wsclient.Command{Name: name, Args: json.RawMessage(args), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}
}

// withPolicy installs a fresh cycle whose clientWs allows cmds.
func withPolicy(t *testing.T, u *Updater, cmds ...string) {
	t.Helper()
	cyc := u.beginCycle(context.Background(), false)
	cyc.eff.Doc.ClientWS.Enabled = true
	cyc.eff.Doc.ClientWS.Commands = cmds
	u.cur.Store(cyc)
}

func TestExecuteCommandMachineInfo(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdMachineInfo)
	s := &fakeSession{}
	msg, cmd := cmdMsg(wsclient.CmdMachineInfo, `{"sections":["network"]}`)
	u.executeCommand(context.Background(), s, msg, cmd)
	if len(s.acks) != 1 || !s.acks[0].Accepted || len(s.results) != 1 || s.results[0].Status != wsclient.ResultOK {
		t.Fatalf("acks=%+v results=%+v", s.acks, s.results)
	}
	if !strings.Contains(string(s.results[0].Payload), `"Ethernet"`) {
		t.Fatalf("payload = %s", s.results[0].Payload)
	}
}

func TestExecuteCommandRefusals(t *testing.T) {
	cases := []struct {
		name, args string
		allowed    []string
		secure     bool
		expired    bool
		want       string
	}{
		{wsclient.CmdMachineInfo, `{"sections":["gpu"]}`, []string{wsclient.CmdMachineInfo}, false, false, wsclient.ErrInvalidArgs},
		{"machine.format_disk", `{}`, nil, false, false, wsclient.ErrUnsupportedCommand},
		{wsclient.CmdMachineInfo, ``, nil, false, false, wsclient.ErrDisabledByPolicy},
		{wsclient.CmdMachineInfo, ``, []string{wsclient.CmdMachineInfo}, false, true, wsclient.ErrExpired},
		{wsclient.CmdServiceRestart, ``, []string{wsclient.CmdServiceRestart}, false, false, wsclient.ErrInsecureTransport},
	}
	for _, c := range cases {
		u := newClientTestUpdater(t)
		withPolicy(t, u, c.allowed...)
		s := &fakeSession{secure: c.secure}
		msg, cmd := cmdMsg(c.name, c.args)
		if c.expired {
			cmd.ExpiresAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
		}
		u.executeCommand(context.Background(), s, msg, cmd)
		if len(s.acks) != 1 || s.acks[0].Accepted || s.acks[0].Error.Code != c.want || len(s.results) != 0 {
			t.Errorf("%s: acks=%+v results=%+v, want refusal %s", c.name, s.acks, s.results, c.want)
		}
	}
}

// Clocks that disagree by more than 5 minutes cannot judge expiry.
func TestExpiryIgnoredWhenClocksDisagree(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdMachineInfo)
	s := &fakeSession{}
	msg, cmd := cmdMsg(wsclient.CmdMachineInfo, ``)
	msg.TS = time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	cmd.ExpiresAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	u.executeCommand(context.Background(), s, msg, cmd)
	if len(s.acks) != 1 || !s.acks[0].Accepted {
		t.Fatalf("acks = %+v", s.acks)
	}
}

func TestExecuteCommandDedupes(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdMachineInfo)
	s := &fakeSession{}
	msg, cmd := cmdMsg(wsclient.CmdMachineInfo, ``)
	u.executeCommand(context.Background(), s, msg, cmd)
	u.executeCommand(context.Background(), s, msg, cmd)
	if len(s.acks) != 2 || !s.acks[1].Duplicate || len(s.results) != 2 || string(s.results[0].Payload) != string(s.results[1].Payload) {
		t.Fatalf("acks=%+v results=%d", s.acks, len(s.results))
	}
}

func TestCommandRingEvictsOldest(t *testing.T) {
	var r commandRing
	for i := 0; i < commandRingSize+1; i++ {
		r.add(string(rune('A'+i%26)) + time.Duration(i).String())
	}
	if _, seen := r.lookup("A0s"); seen {
		t.Fatal("oldest id survived")
	}
}

func TestBusyWhileSameCommandRuns(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdAppsListUpgradable)
	release := make(chan struct{})
	u.listUpgradableFn = func(ctx context.Context) ([]winget.Package, error) { <-release; return nil, nil }
	s1, s2 := &fakeSession{}, &fakeSession{}
	m1, c1 := cmdMsg(wsclient.CmdAppsListUpgradable, ``)
	m2, c2 := cmdMsg(wsclient.CmdAppsListUpgradable, ``)
	done := make(chan struct{})
	go func() { u.executeCommand(context.Background(), s1, m1, c1); close(done) }()
	time.Sleep(50 * time.Millisecond)
	u.executeCommand(context.Background(), s2, m2, c2)
	close(release)
	<-done
	if s2.acks[0].Accepted || s2.acks[0].Error.Code != wsclient.ErrBusy {
		t.Fatalf("second = %+v", s2.acks)
	}
}

func TestListUpgradableErrorsAndTruncation(t *testing.T) {
	u := newClientTestUpdater(t)
	u.listUpgradableFn = func(context.Context) ([]winget.Package, error) { return nil, winget.ErrModuleNotInstalled }
	if _, e, _ := u.listUpgradable(context.Background()); e == nil || e.Code != wsclient.ErrWingetModuleMissing {
		t.Fatalf("module missing -> %+v", e)
	}
	u.listUpgradableFn = func(context.Context) ([]winget.Package, error) { return nil, winget.ErrPowerShellNotFound }
	if _, e, _ := u.listUpgradable(context.Background()); e == nil || e.Code != wsclient.ErrPowerShellNotFound {
		t.Fatalf("powershell -> %+v", e)
	}
	many := make([]winget.Package, 2000)
	for i := range many {
		many[i] = winget.Package{Name: strings.Repeat("n", 40), ID: "Vendor.App", InstalledVersion: "1.0", Available: "2.0", Source: "winget"}
	}
	u.listUpgradableFn = func(context.Context) ([]winget.Package, error) { return many, nil }
	payload, e, truncated := u.listUpgradable(context.Background())
	b, _ := json.Marshal(payload)
	if e != nil || !truncated || len(b) > resultPayloadBudget {
		t.Fatalf("e=%v truncated=%v size=%d", e, truncated, len(b))
	}
}
