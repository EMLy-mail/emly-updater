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

// updateFailedKey identifies one distinct update.failed occurrence.
// Reporting the same (target, target version, attempt, error code) again
// is not new information - it is a cycle restating an outcome the client
// channel already knows.
type updateFailedKey struct {
	target    string
	toVersion string
	attempt   int
	code      string
}

// emitUpdateFailed emits update.failed unless this exact occurrence was
// already reported this process (spec §8.5). Poll goroutine only, like
// announceUpdate.
//
// Without this, reconcileSelfUpdate (selfupdate.go) would repeat
// "version_mismatch" on every cycle a launch stays pending - through its
// cooldown, every retry, and forever once the target is abandoned (GaveUp),
// since Reconcile keeps reporting OutcomeMissed long after the give-up was
// already reported once - and a mirror stuck serving a bad file would
// repeat download_failed/signature_invalid every cycle, since neither
// advances the attempt counter recorded in state.json.
func (u *Updater) emitUpdateFailed(ev updateEvent) {
	code := ""
	if ev.Error != nil {
		code = ev.Error.Code
	}
	if !u.markUpdateFailed(ev.Target, ev.ToVersion, ev.Attempt, code) {
		return
	}
	u.emit(wsclient.EvtUpdateFailed, ev)
}

// markUpdateFailed records (target, toVersion, attempt, code) as reported
// and returns true the first time it is called for that combination, false
// on every later call - the signal emitUpdateFailed uses to decide whether
// to actually emit.
func (u *Updater) markUpdateFailed(target, toVersion string, attempt int, code string) bool {
	if u.updateFailed == nil {
		u.updateFailed = map[updateFailedKey]bool{}
	}
	key := updateFailedKey{target, toVersion, attempt, code}
	if u.updateFailed[key] {
		return false
	}
	u.updateFailed[key] = true
	return true
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
