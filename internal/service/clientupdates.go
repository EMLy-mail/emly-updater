package service

import (
	"strings"

	"emlyupdater/internal/wsclient"
)

type updateEvent struct {
	Target      string              `json:"target"`
	FromVersion string              `json:"from_version,omitempty"`
	ToVersion   string              `json:"to_version"`
	Forced      bool                `json:"forced,omitempty"`
	Attempt     int                 `json:"attempt,omitempty"`
	Trigger     string              `json:"trigger,omitempty"`
	DurationMS  int64               `json:"duration_ms,omitempty"`
	Reinstalled bool                `json:"reinstalled,omitempty"`
	WillRetry   bool                `json:"will_retry,omitempty"`
	Error       *wsclient.ErrorBody `json:"error,omitempty"`
}

// announceUpdate emits update.available once per target and version for
// the life of the process (spec §8.2): a machine waiting three days for
// EMLy to close does not repeat it every cycle. Poll goroutine only.
func (u *Updater) announceUpdate(mc wsclient.ManifestCheck) {
	if u.announced == nil {
		u.announced = map[string]string{}
	}
	if u.announced[mc.Target] == mc.AvailableVersion {
		return
	}
	u.announced[mc.Target] = mc.AvailableVersion
	u.emit(wsclient.EvtUpdateAvailable, mc)
}

// installFailureCode maps installer errors onto spec §8.5's codes. The
// installer package reports by message, not by sentinel; this is the one
// place that reads it.
func installFailureCode(err error) string {
	if strings.Contains(err.Error(), "post-install verification failed") {
		return "version_mismatch"
	}
	return "setup_exit_code"
}
