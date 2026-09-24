package service

import "emlyupdater/internal/wsclient"

// handleNotify reacts to a server-pushed notify (CLIENT_WS_PROTOCOL.md §9).
//
// Temporary stub: Task 10 replaces this with the real per-topic dispatch.
func (u *Updater) handleNotify(n wsclient.Notify) {
	u.Log.Debug("client notify received", "topic", n.Topic)
}
