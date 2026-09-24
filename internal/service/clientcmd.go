package service

import (
	"context"

	"emlyupdater/internal/wsclient"
)

// executeCommand runs a client-channel command (CLIENT_WS_PROTOCOL.md §8).
//
// Temporary stub: Task 7 replaces this with the real ack/result dispatch
// per command name.
func (u *Updater) executeCommand(ctx context.Context, s *wsclient.Session, msg wsclient.Message, cmd wsclient.Command) {
	u.Log.Debug("client command received, not implemented yet", "name", cmd.Name)
}
