// Package logging provides one façade over the two sinks the service uses:
// a rolling file log under %ProgramData%\EMLyUpdater\logs (everything) and the
// Windows Event Log (major events only: update found, install ok/failed,
// forced kill, association repair, source fallback).
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/svc/eventlog"
	"gopkg.in/natefinch/lumberjack.v2"
)

// EventSourceName is the Event Log source registered by the `install`
// subcommand (eventlog.InstallAsEventCreate).
const EventSourceName = "EMLyUpdater"

// Logger writes structured lines to the file log and, for the *Event methods,
// mirrors a plain-text copy to the Windows Event Log.
type Logger struct {
	file *slog.Logger
	ev   *eventlog.Log // nil when the event source is unavailable (run mode without install)
}

// New creates the file logger (5 MB × 5 rotated files). When exeLogPath is
// non-empty a second plain log is also written next to the executable (useful
// for on-site diagnostics without accessing ProgramData). With console=true
// (foreground `run` mode) lines are mirrored to stdout at debug level.
func New(logDir string, exeLogPath string, console bool) *Logger {
	rolling := &lumberjack.Logger{
		Filename:   filepath.Join(logDir, "updater.log"),
		MaxSize:    5, // megabytes
		MaxBackups: 5,
	}

	var w io.Writer = rolling
	if exeLogPath != "" {
		exeLog := &lumberjack.Logger{
			Filename:   exeLogPath,
			MaxSize:    5,
			MaxBackups: 3,
		}
		w = io.MultiWriter(rolling, exeLog)
	}
	level := slog.LevelInfo
	if console {
		w = io.MultiWriter(w, os.Stdout)
		level = slog.LevelDebug
	}

	return &Logger{
		file: slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})),
	}
}

// AttachEventLog opens the Windows Event Log source. Failure is non-fatal
// (e.g. `run` mode before `install` ever registered the source): the file log
// keeps working and event mirroring is silently skipped.
func (l *Logger) AttachEventLog() {
	ev, err := eventlog.Open(EventSourceName)
	if err != nil {
		l.Warn("event log source unavailable, continuing with file log only", "error", err.Error())
		return
	}
	l.ev = ev
}

// Close releases the Event Log handle.
func (l *Logger) Close() {
	if l.ev != nil {
		l.ev.Close()
		l.ev = nil
	}
}

func (l *Logger) Debug(msg string, kv ...any) { l.file.Debug(msg, kv...) }
func (l *Logger) Info(msg string, kv ...any)  { l.file.Info(msg, kv...) }
func (l *Logger) Warn(msg string, kv ...any)  { l.file.Warn(msg, kv...) }
func (l *Logger) Error(msg string, kv ...any) { l.file.Error(msg, kv...) }

// Event IDs grouped per area, so Event Viewer filtering stays meaningful.
const (
	EventGeneric        = 1
	EventUpdateFound    = 100
	EventInstallOK      = 200
	EventInstallFailed  = 201
	EventForcedKill     = 300
	EventAssocRepaired  = 400
	EventSourceFallback = 500
)

// InfoEvent logs to the file and mirrors an information record to the Event Log.
func (l *Logger) InfoEvent(id uint32, msg string, kv ...any) {
	l.file.Info(msg, kv...)
	if l.ev != nil {
		_ = l.ev.Info(id, format(msg, kv))
	}
}

// WarnEvent logs to the file and mirrors a warning record to the Event Log.
func (l *Logger) WarnEvent(id uint32, msg string, kv ...any) {
	l.file.Warn(msg, kv...)
	if l.ev != nil {
		_ = l.ev.Warning(id, format(msg, kv))
	}
}

// ErrorEvent logs to the file and mirrors an error record to the Event Log.
func (l *Logger) ErrorEvent(id uint32, msg string, kv ...any) {
	l.file.Error(msg, kv...)
	if l.ev != nil {
		_ = l.ev.Error(id, format(msg, kv))
	}
}

// format renders "msg (k=v, k=v)" for the Event Log, which has no structured fields.
func format(msg string, kv []any) string {
	if len(kv) == 0 {
		return msg
	}
	out := msg + " ("
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%v=%v", kv[i], kv[i+1])
	}
	return out + ")"
}
