// Package wsclient is the client half of the presence channel described in
// docs/superpowers/specs/2026-09-17-client-presence-ws-design.md: one
// WebSocket connection to the API's GET /v2/client/ws, held open for the
// life of the service, whose only job is to let the API answer "is this
// machine on right now" without guessing from the last poll.
//
// It is deliberately pure Go with no Windows API and no dependency on
// internal/service, so the whole handshake and heartbeat can be exercised
// against a real listener in an ordinary test - the same reason
// internal/policy does not import internal/service either. What decides
// *when* to run a connection (which server, whether the remote document
// enables the channel at all, what to do when one drops) lives in
// internal/service/clientws.go.
//
// The wire protocol's normative reference is the API-side design document,
// emly-go-api/docs/superpowers/specs/2026-09-17-client-presence-ws-api-design.md §3.
// A change to the envelope, the handshake or the heartbeat touches both.
package wsclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Path is the presence endpoint, appended to a server's base URL.
const Path = "/v2/client/ws"

// The envelope's message types. Both sides log and discard a type they do
// not recognise instead of closing the connection, which is what lets a new
// one (a server-to-client command, say) be added without a synchronised
// rollout across ~400 updaters in the field.
const (
	TypeHello    = "hello"
	TypeIdentity = "identity"
	TypePing     = "ping"
	TypePong     = "pong"
	TypeError    = "error"
)

// Message is the envelope every frame on this channel carries. Data is
// omitted for the messages that have no payload (hello, ping, pong). ID,
// ReplyTo and TS are v2 additions (spec §4) and stay empty - so omitted from
// the wire - on a v1 connection, which is what keeps the handshake and
// heartbeat byte-identical to v1 against a v1 server.
type Message struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	ReplyTo string          `json:"reply_to,omitempty"`
	TS      string          `json:"ts,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Identity is the payload of the first message the client sends: the same
// machine facts that travel as X-EMLy-* headers on a manifest check, in JSON
// instead of headers.
//
// Every field is omitempty for the same reason the headers are omitted when
// empty: the API reads an absent value as "not reported" and keeps whatever
// it already has, while an explicit empty string would erase it. There is
// deliberately no equivalent of X-EMLy-IntIP - that one is specific to the
// manifest/download path - and no updater_version or contact, which the API
// reads from the upgrade request's User-Agent.
type Identity struct {
	HWID                     string `json:"hwid,omitempty"`
	Hostname                 string `json:"hostname,omitempty"`
	ADDomain                 string `json:"ad_domain,omitempty"`
	LoggedUser               string `json:"logged_user,omitempty"`
	LoggedUserState          string `json:"logged_user_state,omitempty"`
	LoggedUserDisconnectedAt string `json:"logged_user_disconnected_at,omitempty"`
	Serial                   string `json:"serial,omitempty"`
	Product                  string `json:"product,omitempty"`
	OSVersion                string `json:"os_version,omitempty"`
	EMLyVersion              string `json:"emly_version,omitempty"`

	// Protocol and Capabilities are the v2 negotiation (spec §4). Zero
	// Protocol keeps the payload byte-identical to v1.
	Protocol     int      `json:"protocol,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// Identified reports whether the payload carries enough for the API to know
// which machine this is. The server closes a connection whose identity has
// neither, so there is no point opening one.
func (i Identity) Identified() bool { return i.HWID != "" || i.Hostname != "" }

// Errors the supervisor reacts to differently from an ordinary failure.
var (
	// ErrNotImplemented is a 404 on the upgrade: this server does not serve
	// the endpoint. Same convention as the updater's own manifest - an
	// internal mirror that has not been updated yet answers this, and
	// retrying it on a backoff forever would put an error in every machine
	// of that site's log, every cycle, for as long as the mirror lags.
	ErrNotImplemented = errors.New("server does not implement the presence endpoint")
	// ErrUnauthorized is a 401/403 on the upgrade. The API's rate limiter
	// also answers 403 on the request that trips a ban (see Backoff's doc
	// comment for why that happens to a fleet of reconnecting clients), so
	// this sentinel covers two different causes - a wrong API key and a
	// rate-limit ban - that a caller cannot tell apart from the error alone.
	// dial folds the actual status code into the wrapped message so a log
	// line can still tell them apart without attaching a debugger.
	ErrUnauthorized = errors.New("presence endpoint rejected the connection")
)

// URLFor turns a server's base URL into this endpoint's WebSocket URL:
// https becomes wss, http becomes ws, and Path is appended to whatever path
// the base already has (a mirror served under a prefix keeps it).
func URLFor(baseURL string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return "", errors.New("empty server base URL")
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("server base URL %q is not a URL: %w", baseURL, err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("server base URL %q has scheme %q, want http or https", baseURL, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("server base URL %q has no host", baseURL)
	}
	u.Path += Path
	return u.String(), nil
}

// Default timings. IdleTimeout is three server ping intervals (the API pings
// every 10s and gives the client 20s to answer): if nothing at all arrives
// for that long, the connection is gone whatever the socket still believes.
const (
	defaultIdleTimeout      = 30 * time.Second
	defaultHandshakeTimeout = 30 * time.Second
	writeTimeout            = 10 * time.Second
)

// Handler reacts to the v2 messages a Client's connection receives once the
// server has switched it to v2 with a welcome. Welcome and Notify are called
// from the connection's own goroutine (the same one running heartbeat) and
// must not block for long, or they hold up every other read on the
// connection, including pongs; Command runs on a fresh goroutine per
// command, so a slow one never blocks the next.
type Handler interface {
	// Welcome is called once, when the server's welcome switches the
	// connection to v2.
	Welcome(s *Session, w Welcome)
	// Command is called for every command frame received after welcome, on
	// its own goroutine, with a context that ends when Run returns.
	Command(ctx context.Context, s *Session, msg Message, cmd Command)
	// Notify is called for every notify frame received after welcome.
	Notify(s *Session, msg Message, n Notify)
}

// Client runs one presence connection from dial to close. It is single-use
// in the sense that Run returns when the connection ends; the supervisor
// calls it again for the next attempt.
type Client struct {
	// URL is the full wss:// endpoint, from URLFor.
	URL string
	// APIKey goes out as X-Api-Key on the upgrade request; the route
	// authenticates on it exactly like the manifest endpoints do.
	APIKey string
	// UserAgent goes out as User-Agent. The API parses updater_version and
	// contact out of it, which is why neither is in the identity payload.
	UserAgent string
	// Identity is the payload of the first message sent after the server's
	// hello. Resolved once, by the caller, at the moment of the dial.
	Identity Identity
	// Handler receives v2 commands and notifies once the server switches
	// the connection with a welcome. Nil means v1 behaviour even if the
	// server sends a welcome - it is ignored (logged) instead of dispatched.
	Handler Handler

	// IdleTimeout bounds a single read; zero means defaultIdleTimeout.
	IdleTimeout time.Duration
	// HandshakeTimeout bounds the upgrade and the hello/identity exchange;
	// zero means defaultHandshakeTimeout.
	HandshakeTimeout time.Duration
	// HTTPClient performs the upgrade request; nil means http.DefaultClient.
	HTTPClient *http.Client

	// OnConnected is called, synchronously, once the identity has been sent
	// and the connection is in heartbeat mode. It is how the caller knows a
	// dropped connection was a real one rather than a failed attempt.
	OnConnected func()
	// Logf receives the lines that are not worth an event of their own -
	// above all an unrecognised message type. Optional.
	Logf func(format string, args ...any)
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

func (c *Client) idleTimeout() time.Duration {
	if c.IdleTimeout > 0 {
		return c.IdleTimeout
	}
	return defaultIdleTimeout
}

func (c *Client) handshakeTimeout() time.Duration {
	if c.HandshakeTimeout > 0 {
		return c.HandshakeTimeout
	}
	return defaultHandshakeTimeout
}

// dial performs the upgrade, translating the two status codes the supervisor
// treats specially into sentinel errors.
//
// coder/websocket returns the *http.Response alongside the error when the
// handshake got an answer that was not a 101, which is what makes telling
// "this mirror has no such route" apart from "this mirror is unreachable"
// possible at all.
//
// X-EMLy-HWID and X-EMLy-Hostname go out on the upgrade request too,
// duplicating the same two fields the identity payload sends after the
// handshake. That is a deliberate exception to "identity travels in the
// payload, not headers" (see the Identity doc comment): the API's ban list
// (middleware.BanList) can only enforce a ban by IP on a route it has not
// upgraded yet, because identity does not exist before the handshake
// completes - these two headers are what let an operator's HWID/hostname
// ban reach this connection at all. They also de-anonymise the API's own
// auth-failure log on a rejected key. Exactly like updater_version already
// travels in the User-Agent instead of the JSON body, this is duplication
// for enforcement, not an oversight - do not "clean it up" into payload-only.
func (c *Client) dial(ctx context.Context) (*websocket.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, c.handshakeTimeout())
	defer cancel()

	c.logf("presence channel dialing %s", c.URL)

	header := http.Header{}
	if c.APIKey != "" {
		header.Set("X-Api-Key", c.APIKey)
	}
	if c.UserAgent != "" {
		header.Set("User-Agent", c.UserAgent)
	}
	if c.Identity.HWID != "" {
		header.Set("X-EMLy-HWID", c.Identity.HWID)
	}
	if c.Identity.Hostname != "" {
		header.Set("X-EMLy-Hostname", c.Identity.Hostname)
	}

	conn, resp, err := websocket.Dial(ctx, c.URL, &websocket.DialOptions{
		HTTPClient: c.HTTPClient,
		HTTPHeader: header,
	})
	if err != nil {
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusNotFound:
				return nil, fmt.Errorf("%s: %w", c.URL, ErrNotImplemented)
			case http.StatusUnauthorized, http.StatusForbidden:
				// The status code travels in the message text (not just the
				// sentinel) so a log line built from this error can tell a
				// wrong key (401) apart from a rate-limit ban (403) without
				// the caller doing anything special - see ErrUnauthorized.
				return nil, fmt.Errorf("%s: HTTP %d: %w", c.URL, resp.StatusCode, ErrUnauthorized)
			}
		}
		return nil, fmt.Errorf("presence channel dial failed: %w", err)
	}
	c.logf("presence channel upgrade succeeded, waiting for hello")
	return conn, nil
}

// Run holds one presence connection open: dial, wait for the server's hello,
// send this machine's identity, then answer every ping with a pong until the
// connection ends or ctx is cancelled.
//
// It returns ctx.Err() for a clean shutdown, ErrNotImplemented or
// ErrUnauthorized from the dial, and a descriptive error otherwise. The
// caller decides what any of that means for the retry schedule - this
// function has no opinion about reconnecting.
func (c *Client) Run(ctx context.Context) error {
	if !c.Identity.Identified() {
		// The server closes an unidentified connection on sight (API spec
		// §3.1), so opening one only costs a handshake to be told so.
		return errors.New("refusing to open the presence channel: this machine reports neither a HWID nor a hostname")
	}

	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(int64(DefaultLimits.MaxMessageBytes) + 1024)

	if err := c.handshake(ctx, conn); err != nil {
		return err
	}
	if c.OnConnected != nil {
		c.OnConnected()
	}

	err = c.heartbeat(ctx, conn)
	if ctx.Err() != nil {
		// A stop request, not a failure: say goodbye properly so the API
		// drops this machine's presence immediately instead of waiting for
		// its own read deadline to expire.
		_ = conn.Close(websocket.StatusNormalClosure, "service stopping")
	}
	return err
}

// handshake waits for the server's hello and answers with the identity.
//
// Messages of any other type are discarded while waiting, for the same
// reason the heartbeat loop discards them: a type this build does not know
// must never be a reason to hang up. The whole exchange is bounded by
// HandshakeTimeout, so a server that accepts the upgrade and then says
// nothing does not hold the connection open forever.
func (c *Client) handshake(ctx context.Context, conn *websocket.Conn) error {
	ctx, cancel := context.WithTimeout(ctx, c.handshakeTimeout())
	defer cancel()

	for {
		var msg Message
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return fmt.Errorf("presence channel handshake failed waiting for %q: %w", TypeHello, err)
		}
		switch msg.Type {
		case TypeHello:
			c.logf("presence channel received hello, sending identity (hwid=%q hostname=%q)",
				c.Identity.HWID, c.Identity.Hostname)
			payload, err := json.Marshal(c.Identity)
			if err != nil {
				return fmt.Errorf("could not serialise this machine's identity: %w", err)
			}
			if err := wsjson.Write(ctx, conn, Message{Type: TypeIdentity, Data: payload}); err != nil {
				return fmt.Errorf("presence channel could not send its identity: %w", err)
			}
			c.logf("presence channel identity sent")
			return nil
		case TypeError:
			return fmt.Errorf("presence endpoint refused the connection: %s", errorCode(msg.Data))
		default:
			c.logf("presence channel ignoring unknown message type %q during the handshake", msg.Type)
		}
	}
}

// heartbeat answers the server's pings until the connection ends.
//
// Each read is bounded by IdleTimeout rather than left open indefinitely:
// a connection through a firewall that silently dropped the flow looks
// perfectly healthy from this side, and only the absence of the pings the
// server promised every 10 seconds gives it away.
func (c *Client) heartbeat(ctx context.Context, conn *websocket.Conn) error {
	var sess *Session
	cmdCtx, cancelCmds := context.WithCancel(ctx)
	defer cancelCmds()
	for {
		readCtx, cancel := context.WithTimeout(ctx, c.idleTimeout())
		var msg Message
		err := wsjson.Read(readCtx, conn, &msg)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("presence channel read failed: %w", err)
		}

		switch msg.Type {
		case TypePing:
			writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := wsjson.Write(writeCtx, conn, Message{Type: TypePong})
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("presence channel could not answer a ping: %w", err)
			}
			c.logf("presence channel answered a ping")
		case TypeWelcome:
			if c.Handler == nil || sess != nil {
				continue
			}
			var w Welcome
			if err := json.Unmarshal(msg.Data, &w); err != nil || w.Protocol < ProtocolV2 {
				c.logf("presence channel ignoring an unusable welcome: %s", msg.Data)
				continue
			}
			sess = newSession(conn, c.URL, w)
			c.logf("presence channel switched to protocol v2 (%d capabilities)", len(w.AcceptedCapabilities))
			c.Handler.Welcome(sess, w)
		case TypeCommand:
			if sess == nil {
				c.logf("presence channel ignoring a command received before welcome")
				continue
			}
			var cmd Command
			if err := json.Unmarshal(msg.Data, &cmd); err != nil {
				c.logf("presence channel ignoring a malformed command: %v", err)
				continue
			}
			go c.Handler.Command(cmdCtx, sess, msg, cmd)
		case TypeNotify:
			if sess == nil {
				continue
			}
			var n Notify
			if err := json.Unmarshal(msg.Data, &n); err == nil {
				c.Handler.Notify(sess, msg, n)
			}
		case TypeError:
			return fmt.Errorf("presence endpoint refused the connection: %s", errorCode(msg.Data))
		default:
			c.logf("presence channel ignoring unknown message type %q", msg.Type)
		}
	}
}

// errorCode pulls the code out of an error message's payload, falling back
// to the raw payload when it is not the shape we expect - a server that
// says something unexpected is still saying something worth logging.
func errorCode(data json.RawMessage) string {
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(data, &payload); err == nil && payload.Code != "" {
		return payload.Code
	}
	if len(data) == 0 {
		return "(no code)"
	}
	return string(data)
}
