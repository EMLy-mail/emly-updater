package service

import (
	"context"

	"emlyupdater/internal/wsclient"
)

// admitDestructive and runDestructive are stubs until Task 8 lands
// (service.restart / machine.reboot admission and execution).
func (u *Updater) admitDestructive(cmd wsclient.Command) *wsclient.ErrorBody { return nil }

func (u *Updater) runDestructive(ctx context.Context, s commandSession, msg wsclient.Message, cmd wsclient.Command) {
	_ = s.Result(ctx, msg.ID, wsclient.Result{Status: wsclient.ResultError, Error: refuse(wsclient.ErrUnsupportedCommand, "not implemented")})
}
