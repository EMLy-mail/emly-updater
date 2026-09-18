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
