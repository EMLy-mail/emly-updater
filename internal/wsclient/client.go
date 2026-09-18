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
// omitted for the messages that have no payload (hello, ping, pong).
type Message struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
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
	// ErrUnauthorized is a 401/403: the API key is wrong or not accepted.
	// Retrying does not help either, but unlike a 404 it is a
	// misconfiguration worth shouting about rather than a state to accept.
	ErrUnauthorized = errors.New("presence endpoint rejected the API key")
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
func (c *Client) dial(ctx context.Context) (*websocket.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, c.handshakeTimeout())
	defer cancel()

	header := http.Header{}
	if c.APIKey != "" {
		header.Set("X-Api-Key", c.APIKey)
	}
	if c.UserAgent != "" {
		header.Set("User-Agent", c.UserAgent)
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
				return nil, fmt.Errorf("%s: %w", c.URL, ErrUnauthorized)
			}
		}
		return nil, fmt.Errorf("presence channel dial failed: %w", err)
	}
	return conn, nil
}
