package wsclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// ErrTooLarge is a frame over the negotiated max_message_bytes. The caller
// truncates what it sends instead (spec §11).
var ErrTooLarge = errors.New("message exceeds max_message_bytes")

// Session is one v2 connection as seen by the Handler. Its methods are safe
// to call from any goroutine (coder/websocket allows concurrent writes) and
// fail harmlessly once the connection is gone.
type Session struct {
	conn     *websocket.Conn
	secure   bool
	accepted map[string]bool
	limits   Limits
}

func newSession(conn *websocket.Conn, url string, w Welcome) *Session {
	acc := make(map[string]bool, len(w.AcceptedCapabilities))
	for _, c := range w.AcceptedCapabilities {
		acc[c] = true
	}
	limits := w.Limits
	if limits.MaxMessageBytes <= 0 {
		limits = DefaultLimits
	}
	return &Session{conn: conn, secure: strings.HasPrefix(url, "wss://"), accepted: acc, limits: limits}
}

// Secure reports whether the connection is TLS (wss://), the condition for
// destructive commands (spec §12.2).
func (s *Session) Secure() bool { return s.secure }

// Accepted reports whether the server accepted this capability in welcome.
func (s *Session) Accepted(name string) bool { return s.accepted[name] }

// Limits is what the server announced.
func (s *Session) Limits() Limits { return s.limits }

func (s *Session) write(ctx context.Context, typ, replyTo string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	b, err := json.Marshal(Message{Type: typ, ID: NewID(), ReplyTo: replyTo,
		TS: time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"), Data: raw})
	if err != nil {
		return err
	}
	if len(b) > s.limits.MaxMessageBytes {
		return ErrTooLarge
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return s.conn.Write(wctx, websocket.MessageText, b)
}

func (s *Session) Ack(ctx context.Context, replyTo string, a Ack) error {
	return s.write(ctx, TypeAck, replyTo, a)
}

func (s *Session) Result(ctx context.Context, replyTo string, r Result) error {
	return s.write(ctx, TypeResult, replyTo, r)
}

func (s *Session) Event(ctx context.Context, name string, payload any) error {
	return s.write(ctx, TypeEvent, "", Event{Name: name, Payload: payload})
}

// Close ends the connection with code (1001 before a restart or reboot).
func (s *Session) Close(code websocket.StatusCode, reason string) {
	_ = s.conn.Close(code, reason)
}
