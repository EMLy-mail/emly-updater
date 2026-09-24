package wsclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestURLFor(t *testing.T) {
	cases := []struct {
		base string
		want string
	}{
		{"https://api.emly.ffois.it", "wss://api.emly.ffois.it/v2/client/ws"},
		{"http://172.16.96.73:8080", "ws://172.16.96.73:8080/v2/client/ws"},
		{"https://mirror.example.test/emly", "wss://mirror.example.test/emly/v2/client/ws"},
	}
	for _, c := range cases {
		got, err := URLFor(c.base)
		if err != nil {
			t.Errorf("URLFor(%q) returned %v", c.base, err)
			continue
		}
		if got != c.want {
			t.Errorf("URLFor(%q) = %q, want %q", c.base, got, c.want)
		}
	}
}

func TestURLForRejectsWhatItCannotSpeak(t *testing.T) {
	for _, base := range []string{"", "ftp://example.test", "://nonsense"} {
		if got, err := URLFor(base); err == nil {
			t.Errorf("URLFor(%q) = %q, want an error", base, got)
		}
	}
}

// Every field of the identity payload is optional in exactly the way the
// matching X-EMLy-* header is today: absent means "not reported", never
// "empty". The API reads a missing key as unknown and keeps what it has,
// while an explicit empty string would erase it.
func TestIdentityOmitsWhatItDoesNotKnow(t *testing.T) {
	raw, err := json.Marshal(Identity{Hostname: "RM095", HWID: "HW-1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, `"hostname":"RM095"`) || !strings.Contains(got, `"hwid":"HW-1"`) {
		t.Errorf("payload dropped a field it was given: %s", got)
	}
	for _, key := range []string{"ad_domain", "logged_user", "logged_user_state",
		"logged_user_disconnected_at", "serial", "product", "os_version", "emly_version"} {
		if strings.Contains(got, key) {
			t.Errorf("payload carries %q for a value it does not have: %s", key, got)
		}
	}
}

func TestIdentityIdentified(t *testing.T) {
	if (Identity{}).Identified() {
		t.Error("an empty identity reports itself as identified")
	}
	if !(Identity{HWID: "HW-1"}).Identified() {
		t.Error("an identity with only a HWID is not identified")
	}
	if !(Identity{Hostname: "RM095"}).Identified() {
		t.Error("an identity with only a hostname is not identified")
	}
}

// A 404 on the upgrade is the documented answer from a site mirror that has
// not been updated yet, exactly like the updater's own manifest endpoint. It
// has to come back distinguishable, because the supervisor stops asking that
// server rather than retrying it forever.
func TestDialReportsAnUnimplementedEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	url, err := URLFor(srv.URL)
	if err != nil {
		t.Fatalf("URLFor: %v", err)
	}
	c := &Client{URL: url, HandshakeTimeout: 5 * time.Second}
	if _, err := c.dial(context.Background()); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("dial against a 404 returned %v, want ErrNotImplemented", err)
	}
}

func TestDialReportsARejectedKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	url, err := URLFor(srv.URL)
	if err != nil {
		t.Fatalf("URLFor: %v", err)
	}
	c := &Client{URL: url, HandshakeTimeout: 5 * time.Second}
	if _, err := c.dial(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("dial against a 401 returned %v, want ErrUnauthorized", err)
	}
}

// The API key and the User-Agent go out on the upgrade request: the key is
// what the route authenticates on, and the User-Agent is where the API reads
// updater_version and contact from - they are deliberately not in the
// identity payload.
// A wss:// dial (over TLS) redirected to a plain http:// endpoint must be
// refused rather than followed: Session.Secure() is derived from the URL
// this Client was configured with, not from the connection the redirect
// actually ends up using, so following the downgrade would leave Secure()
// reporting true - the condition destructive commands require - over a
// connection that was never TLS for the hop that mattered.
func TestDialRefusesATLSToPlainHTTPRedirect(t *testing.T) {
	var plainHit bool
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHit = true
		http.NotFound(w, r)
	}))
	defer plain.Close()

	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	defer secure.Close()

	url, err := URLFor(secure.URL)
	if err != nil {
		t.Fatalf("URLFor: %v", err)
	}
	c := &Client{URL: url, HandshakeTimeout: 5 * time.Second, HTTPClient: secure.Client()}
	if _, err := c.dial(context.Background()); err == nil {
		t.Fatal("dial must fail rather than follow a wss -> ws (https -> http) redirect")
	}
	if plainHit {
		t.Fatal("the plain HTTP endpoint must never actually be reached")
	}
}

func TestDialSendsTheApiKeyAndUserAgent(t *testing.T) {
	seen := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		http.NotFound(w, r)
	}))
	defer srv.Close()

	url, err := URLFor(srv.URL)
	if err != nil {
		t.Fatalf("URLFor: %v", err)
	}
	c := &Client{
		URL:              url,
		APIKey:           "secret-key",
		UserAgent:        "EMLy-Updater/1.6.3 (it@example.test)",
		HandshakeTimeout: 5 * time.Second,
	}
	_, _ = c.dial(context.Background())

	select {
	case h := <-seen:
		if got := h.Get("X-Api-Key"); got != "secret-key" {
			t.Errorf("X-Api-Key = %q, want %q", got, "secret-key")
		}
		if got := h.Get("User-Agent"); got != "EMLy-Updater/1.6.3 (it@example.test)" {
			t.Errorf("User-Agent = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server never saw the upgrade request")
	}
}

// HWID and hostname go out as headers too, duplicated from the identity that
// otherwise only travels in the post-handshake payload - it is what lets the
// API's ban list enforce a HWID/hostname ban on this route, which cannot see
// the payload before the upgrade completes. A field the identity does not
// carry must not appear as an empty header (same "absent means unknown"
// contract the X-EMLy-* headers already have on the manifest path).
func TestDialSendsHWIDAndHostnameHeaders(t *testing.T) {
	seen := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		http.NotFound(w, r)
	}))
	defer srv.Close()

	url, err := URLFor(srv.URL)
	if err != nil {
		t.Fatalf("URLFor: %v", err)
	}
	c := &Client{
		URL:              url,
		Identity:         Identity{HWID: "HW-1", Hostname: "RM095"},
		HandshakeTimeout: 5 * time.Second,
	}
	_, _ = c.dial(context.Background())

	select {
	case h := <-seen:
		if got := h.Get("X-EMLy-HWID"); got != "HW-1" {
			t.Errorf("X-EMLy-HWID = %q, want %q", got, "HW-1")
		}
		if got := h.Get("X-EMLy-Hostname"); got != "RM095" {
			t.Errorf("X-EMLy-Hostname = %q, want %q", got, "RM095")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server never saw the upgrade request")
	}
}

// An identity field this machine does not have must not send an empty
// header - "absent" means "unknown" everywhere else in this codebase.
func TestDialOmitsEmptyIdentityHeaders(t *testing.T) {
	seen := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		http.NotFound(w, r)
	}))
	defer srv.Close()

	url, err := URLFor(srv.URL)
	if err != nil {
		t.Fatalf("URLFor: %v", err)
	}
	c := &Client{URL: url, HandshakeTimeout: 5 * time.Second}
	_, _ = c.dial(context.Background())

	select {
	case h := <-seen:
		if _, ok := h["X-Emly-Hwid"]; ok {
			t.Errorf("X-EMLy-HWID sent with no value: %v", h)
		}
		if _, ok := h["X-Emly-Hostname"]; ok {
			t.Errorf("X-EMLy-Hostname sent with no value: %v", h)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server never saw the upgrade request")
	}
}

// A rejected key's error names the actual status code, so a log line built
// from it can tell a wrong key (401) apart from the rate limiter's ban (403)
// without attaching a debugger.
func TestDialRejectedKeyErrorNamesTheStatusCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	url, err := URLFor(srv.URL)
	if err != nil {
		t.Fatalf("URLFor: %v", err)
	}
	c := &Client{URL: url, HandshakeTimeout: 5 * time.Second}
	_, err = c.dial(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("dial against a 403 = %v, want ErrUnauthorized", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error %q does not name the status code", err.Error())
	}
}

// testServer runs handler as a WebSocket endpoint on a real listener and
// returns the wss:// URL a Client should dial. It is the API's half of the
// protocol, so the tests below exercise the real handshake rather than a
// mock of it.
func testServer(t *testing.T, handler func(ctx context.Context, conn *websocket.Conn)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.CloseNow()
		handler(r.Context(), conn)
	}))
	t.Cleanup(srv.Close)

	url, err := URLFor(srv.URL)
	if err != nil {
		t.Fatalf("URLFor: %v", err)
	}
	return url
}

// The handshake in full: the server speaks first with hello, the client
// answers with its identity, and only then does the connection count as
// established.
func TestRunSendsIdentityAfterHello(t *testing.T) {
	got := make(chan Identity, 1)
	url := testServer(t, func(ctx context.Context, conn *websocket.Conn) {
		if err := wsjson.Write(ctx, conn, Message{Type: TypeHello}); err != nil {
			return
		}
		var msg Message
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}
		if msg.Type != TypeIdentity {
			t.Errorf("first client message was %q, want %q", msg.Type, TypeIdentity)
			return
		}
		var id Identity
		if err := json.Unmarshal(msg.Data, &id); err != nil {
			t.Errorf("identity payload: %v", err)
			return
		}
		got <- id
		conn.Close(websocket.StatusNormalClosure, "done")
	})

	connected := make(chan struct{}, 1)
	c := &Client{
		URL:              url,
		Identity:         Identity{HWID: "HW-1", Hostname: "RM095", OSVersion: "Windows 11"},
		HandshakeTimeout: 5 * time.Second,
		IdleTimeout:      5 * time.Second,
		OnConnected:      func() { connected <- struct{}{} },
	}
	_ = c.Run(context.Background())

	select {
	case id := <-got:
		if id.HWID != "HW-1" || id.Hostname != "RM095" || id.OSVersion != "Windows 11" {
			t.Errorf("identity = %+v", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server never received an identity")
	}
	select {
	case <-connected:
	default:
		t.Error("OnConnected was never called for a completed handshake")
	}
}

// The heartbeat: every server ping is answered with a pong, for as long as
// the connection lives.
func TestRunAnswersPingsWithPongs(t *testing.T) {
	const pings = 3
	pongs := make(chan struct{}, pings)
	url := testServer(t, func(ctx context.Context, conn *websocket.Conn) {
		if err := wsjson.Write(ctx, conn, Message{Type: TypeHello}); err != nil {
			return
		}
		var identity Message
		if err := wsjson.Read(ctx, conn, &identity); err != nil {
			return
		}
		for i := 0; i < pings; i++ {
			if err := wsjson.Write(ctx, conn, Message{Type: TypePing}); err != nil {
				return
			}
			var msg Message
			if err := wsjson.Read(ctx, conn, &msg); err != nil {
				return
			}
			if msg.Type != TypePong {
				t.Errorf("answer to a ping was %q, want %q", msg.Type, TypePong)
				return
			}
			pongs <- struct{}{}
		}
		conn.Close(websocket.StatusNormalClosure, "done")
	})

	c := &Client{URL: url, Identity: Identity{HWID: "HW-1"},
		HandshakeTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second}
	_ = c.Run(context.Background())

	if len(pongs) != pings {
		t.Errorf("answered %d of %d pings", len(pongs), pings)
	}
}

// An unknown type is logged and discarded, never a reason to hang up. This
// is what makes it safe for the API to start sending a message type this
// build has never heard of.
func TestRunIgnoresUnknownMessageTypes(t *testing.T) {
	pongs := make(chan struct{}, 1)
	url := testServer(t, func(ctx context.Context, conn *websocket.Conn) {
		if err := wsjson.Write(ctx, conn, Message{Type: TypeHello}); err != nil {
			return
		}
		var identity Message
		if err := wsjson.Read(ctx, conn, &identity); err != nil {
			return
		}
		if err := wsjson.Write(ctx, conn, Message{Type: "command",
			Data: json.RawMessage(`{"name":"from-the-future"}`)}); err != nil {
			return
		}
		if err := wsjson.Write(ctx, conn, Message{Type: TypePing}); err != nil {
			return
		}
		var msg Message
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}
		if msg.Type == TypePong {
			pongs <- struct{}{}
		}
		conn.Close(websocket.StatusNormalClosure, "done")
	})

	var logged []string
	c := &Client{URL: url, Identity: Identity{HWID: "HW-1"},
		HandshakeTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second,
		Logf: func(format string, args ...any) {
			logged = append(logged, fmt.Sprintf(format, args...))
		}}
	_ = c.Run(context.Background())

	if len(pongs) != 1 {
		t.Error("the connection did not survive an unknown message type")
	}
	var mentioned bool
	for _, line := range logged {
		if strings.Contains(line, "command") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Errorf("the unknown type was discarded without a word: %v", logged)
	}
}

// The server refusing the identity (neither hwid nor hostname) comes back as
// an error naming the code, not as a silent disconnect: this is the
// misconfiguration an operator has to be able to read out of the log.
func TestRunSurfacesAServerError(t *testing.T) {
	url := testServer(t, func(ctx context.Context, conn *websocket.Conn) {
		if err := wsjson.Write(ctx, conn, Message{Type: TypeHello}); err != nil {
			return
		}
		var identity Message
		if err := wsjson.Read(ctx, conn, &identity); err != nil {
			return
		}
		_ = wsjson.Write(ctx, conn, Message{Type: TypeError,
			Data: json.RawMessage(`{"code":"unidentified"}`)})
		conn.Close(websocket.StatusPolicyViolation, "unidentified")
	})

	c := &Client{URL: url, Identity: Identity{HWID: "HW-1"},
		HandshakeTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second}
	err := c.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unidentified") {
		t.Errorf("Run() = %v, want an error naming the server's code", err)
	}
}

// Cancelling the context is how the service stops the channel. Run must come
// back promptly with the context's error rather than sitting in a blocking
// read until the idle timeout.
func TestRunReturnsWhenTheContextIsCancelled(t *testing.T) {
	url := testServer(t, func(ctx context.Context, conn *websocket.Conn) {
		if err := wsjson.Write(ctx, conn, Message{Type: TypeHello}); err != nil {
			return
		}
		var identity Message
		if err := wsjson.Read(ctx, conn, &identity); err != nil {
			return
		}
		<-ctx.Done() // never speaks again
	})

	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{URL: url, Identity: Identity{HWID: "HW-1"},
		HandshakeTimeout: 5 * time.Second, IdleTimeout: time.Minute}

	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run() = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

// Refusing to open a connection the server would close anyway: an identity
// with neither hwid nor hostname is a machine the API cannot track, and
// dialling would just burn a handshake.
func TestRunRefusesAnUnidentifiedClient(t *testing.T) {
	c := &Client{URL: "wss://unused.example.test/v2/client/ws"}
	if err := c.Run(context.Background()); err == nil {
		t.Fatal("Run() with an empty identity returned no error")
	}
}

type recHandler struct {
	welcomes chan *Session
	commands chan Command
	notifies chan Notify
}

func newRecHandler() *recHandler {
	return &recHandler{welcomes: make(chan *Session, 1), commands: make(chan Command, 4), notifies: make(chan Notify, 4)}
}

func (h *recHandler) Welcome(s *Session, w Welcome) { h.welcomes <- s }
func (h *recHandler) Command(ctx context.Context, s *Session, msg Message, cmd Command) {
	_ = s.Ack(ctx, msg.ID, Ack{Accepted: true})
	h.commands <- cmd
}
func (h *recHandler) Notify(s *Session, msg Message, n Notify) { h.notifies <- n }

// v2Server runs a fake API: hello, read identity (handed to gotIdentity),
// then script(c).
func v2Server(t *testing.T, gotIdentity chan<- Identity, script func(ctx context.Context, c *websocket.Conn)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		_ = wsjson.Write(ctx, c, Message{Type: TypeHello})
		var msg Message
		if err := wsjson.Read(ctx, c, &msg); err != nil {
			return
		}
		var id Identity
		_ = json.Unmarshal(msg.Data, &id)
		gotIdentity <- id
		script(ctx, c)
	}))
}

func welcomeFrame(caps ...string) Message {
	data, _ := json.Marshal(Welcome{Protocol: 2, AcceptedCapabilities: caps, Limits: DefaultLimits})
	return Message{Type: TypeWelcome, ID: NewID(), Data: data}
}

func runClient(t *testing.T, srv *httptest.Server, h Handler) (context.CancelFunc, <-chan error) {
	t.Helper()
	url, _ := URLFor(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	c := &Client{URL: url, Identity: Identity{HWID: "HW-1", Protocol: 2, Capabilities: []string{CmdMachineInfo}}, Handler: h}
	go func() { done <- c.Run(ctx) }()
	return cancel, done
}

func TestIdentityCarriesProtocolAndCapabilities(t *testing.T) {
	ids := make(chan Identity, 1)
	srv := v2Server(t, ids, func(ctx context.Context, c *websocket.Conn) { <-ctx.Done() })
	defer srv.Close()
	cancel, _ := runClient(t, srv, newRecHandler())
	defer cancel()
	select {
	case id := <-ids:
		if id.Protocol != 2 || len(id.Capabilities) != 1 || id.Capabilities[0] != CmdMachineInfo {
			t.Fatalf("identity = %+v", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no identity")
	}
}

func TestWelcomeThenCommandIsDispatchedAndAcked(t *testing.T) {
	ids := make(chan Identity, 1)
	acks := make(chan Message, 1)
	srv := v2Server(t, ids, func(ctx context.Context, c *websocket.Conn) {
		_ = wsjson.Write(ctx, c, welcomeFrame(CmdMachineInfo))
		data, _ := json.Marshal(Command{Name: CmdMachineInfo, ExpiresAt: "2099-01-01T00:00:00Z"})
		cmdID := NewID()
		_ = wsjson.Write(ctx, c, Message{Type: TypeCommand, ID: cmdID, Data: data})
		var m Message
		if wsjson.Read(ctx, c, &m) == nil {
			acks <- m
		}
		<-ctx.Done()
	})
	defer srv.Close()
	h := newRecHandler()
	cancel, _ := runClient(t, srv, h)
	defer cancel()

	select {
	case s := <-h.welcomes:
		if s.Secure() || !s.Accepted(CmdMachineInfo) || s.Accepted(CmdMachineReboot) {
			t.Fatalf("session secure=%v", s.Secure())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no welcome")
	}
	select {
	case cmd := <-h.commands:
		if cmd.Name != CmdMachineInfo {
			t.Fatalf("cmd = %+v", cmd)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("command not dispatched")
	}
	select {
	case m := <-acks:
		if m.Type != TypeAck || m.ReplyTo == "" || len(m.ID) != 26 {
			t.Fatalf("ack = %+v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no ack")
	}
}

func TestCommandIgnoredBeforeWelcome(t *testing.T) {
	ids := make(chan Identity, 1)
	srv := v2Server(t, ids, func(ctx context.Context, c *websocket.Conn) {
		data, _ := json.Marshal(Command{Name: CmdMachineInfo})
		_ = wsjson.Write(ctx, c, Message{Type: TypeCommand, ID: NewID(), Data: data})
		_ = wsjson.Write(ctx, c, Message{Type: TypePing})
		var pong Message
		_ = wsjson.Read(ctx, c, &pong)
		<-ctx.Done()
	})
	defer srv.Close()
	h := newRecHandler()
	cancel, _ := runClient(t, srv, h)
	defer cancel()
	select {
	case <-h.commands:
		t.Fatal("command executed on a connection that never received welcome")
	case <-time.After(500 * time.Millisecond):
	}
}

func TestNotifyDispatched(t *testing.T) {
	ids := make(chan Identity, 1)
	srv := v2Server(t, ids, func(ctx context.Context, c *websocket.Conn) {
		_ = wsjson.Write(ctx, c, welcomeFrame(TopicConfigPublished))
		payload, _ := json.Marshal(ConfigPublished{Revision: 44, JitterSeconds: 60})
		data, _ := json.Marshal(Notify{Topic: TopicConfigPublished, Payload: payload})
		_ = wsjson.Write(ctx, c, Message{Type: TypeNotify, ID: NewID(), Data: data})
		<-ctx.Done()
	})
	defer srv.Close()
	h := newRecHandler()
	cancel, _ := runClient(t, srv, h)
	defer cancel()
	select {
	case n := <-h.notifies:
		if n.Topic != TopicConfigPublished {
			t.Fatalf("n = %+v", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("notify not dispatched")
	}
}

func TestSessionRefusesOversizedFrames(t *testing.T) {
	ids := make(chan Identity, 1)
	srv := v2Server(t, ids, func(ctx context.Context, c *websocket.Conn) {
		_ = wsjson.Write(ctx, c, welcomeFrame(EvtMachineInfo))
		<-ctx.Done()
	})
	defer srv.Close()
	h := newRecHandler()
	cancel, _ := runClient(t, srv, h)
	defer cancel()
	s := <-h.welcomes
	big := strings.Repeat("x", DefaultLimits.MaxMessageBytes)
	if err := s.Event(context.Background(), EvtMachineInfo, big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}
