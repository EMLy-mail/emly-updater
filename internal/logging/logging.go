// Package logging provides one façade over the two sinks the service uses:
// a rolling file log under %ProgramData%\EMLyUpdater\logs (everything) and the
// Windows Event Log (major events only: update found, install ok/failed,
// forced kill, association repair, source fallback).
//
// Level, file rotation and the Event Log mirror can be changed while the
// service runs (SetLevel, Reconfigure): the remote configuration document
// carries a logging section that is applied as soon as it is accepted.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/windows/svc/eventlog"
	"gopkg.in/natefinch/lumberjack.v2"
)

// EventSourceName is the Event Log source registered by the `install`
// subcommand (eventlog.InstallAsEventCreate).
const EventSourceName = "EMLyUpdater"

// FileSettings are the rolling-file parameters that can change at runtime.
type FileSettings struct {
	MaxSizeMB  int
	MaxBackups int
	Compress   bool
}

// DefaultFileSettings is what New starts with: 2 MB × 5 rotated files,
// gzip-compressed once rotated.
var DefaultFileSettings = FileSettings{MaxSizeMB: 2, MaxBackups: 5, Compress: true}

// Logger writes structured lines to the file log and, for the *Event methods,
// mirrors a plain-text copy to the Windows Event Log.
type Logger struct {
	file  *slog.Logger
	level *slog.LevelVar

	// The rolling file sinks sit behind a swappable writer so their size and
	// backup limits can be changed without a restart: lumberjack reads its
	// fields on every write, so mutating a live instance would race with
	// the write goroutine. Reconfigure builds new instances pointing at the
	// same files and swaps them in.
	sink     *swapWriter
	logDir   string
	exePath  string
	console  bool
	settings FileSettings

	ev        *eventlog.Log // nil when the event source is unavailable (run mode without install)
	evEnabled atomic.Bool
}

// New creates the file logger with DefaultFileSettings. When exeLogPath is
// non-empty a second plain log is also written next to the executable (useful
// for on-site diagnostics without accessing ProgramData). With console=true
// (foreground `run` mode) lines are mirrored to stdout at debug level.
func New(logDir string, exeLogPath string, console bool) *Logger {
	l := &Logger{
		level:    new(slog.LevelVar),
		sink:     &swapWriter{},
		logDir:   logDir,
		exePath:  exeLogPath,
		console:  console,
		settings: DefaultFileSettings,
	}
	l.level.Set(slog.LevelInfo)
	if console {
		l.level.Set(slog.LevelDebug)
	}
	l.evEnabled.Store(true)
	l.sink.swap(l.buildFiles(DefaultFileSettings))

	var w io.Writer = l.sink
	if console {
		w = io.MultiWriter(l.sink, os.Stdout)
	}
	l.file = slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: l.level}))
	return l
}

// buildFiles creates the rolling file writers for s.
func (l *Logger) buildFiles(s FileSettings) (io.Writer, []io.Closer) {
	rolling := &lumberjack.Logger{
		Filename:   filepath.Join(l.logDir, "updater.log"),
		MaxSize:    s.MaxSizeMB,
		MaxBackups: s.MaxBackups,
		Compress:   s.Compress, // rotated backups become updater-<timestamp>.log.gz
	}
	closers := []io.Closer{rolling}
	var w io.Writer = rolling
	if l.exePath != "" {
		exeLog := &lumberjack.Logger{
			Filename:   l.exePath,
			MaxSize:    s.MaxSizeMB,
			MaxBackups: min(s.MaxBackups, 3),
			Compress:   s.Compress,
		}
		closers = append(closers, exeLog)
		w = io.MultiWriter(rolling, exeLog)
	}
	return w, closers
}

// Reconfigure applies new rolling-file limits. A no-op when nothing changed;
// otherwise the old writers are closed (lumberjack reopens lazily, so no
// line is lost) and the new limits apply from the next rotation.
func (l *Logger) Reconfigure(s FileSettings) {
	if s.MaxSizeMB < 1 {
		s.MaxSizeMB = DefaultFileSettings.MaxSizeMB
	}
	if s.MaxBackups < 0 {
		s.MaxBackups = 0
	}
	if s == l.settings {
		return
	}
	l.settings = s
	old := l.sink.swap(l.buildFiles(s))
	for _, c := range old {
		_ = c.Close()
	}
}

// SetLevel changes the minimum level written to the file log. Unknown names
// are ignored, so a document can never silence the log by accident.
// The console mirror in `run` mode stays at whatever level is set here too.
func (l *Logger) SetLevel(name string) bool {
	lv, ok := ParseLevel(name)
	if !ok {
		return false
	}
	l.level.Set(lv)
	return true
}

// Level returns the current minimum level name.
func (l *Logger) Level() string {
	return strings.ToLower(l.level.Level().String())
}

// ParseLevel maps "debug" | "info" | "warn" | "error" to a slog.Level.
func ParseLevel(name string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return 0, false
}

// SetEventLog turns the Windows Event Log mirror on or off. The remote
// configuration events (900-904) are written regardless, so a document that
// switches the mirror off is still diagnosable from Event Viewer.
func (l *Logger) SetEventLog(enabled bool) { l.evEnabled.Store(enabled) }

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
	EventGeneric     = 1
	EventUpdateFound = 100
	// EventSourcesUnreachable fires once per outage when a poll cycle cannot
	// reach any configured update source (primary retries plus, when wired
	// in, the fallback) - the console user is toasted at the same time.
	EventSourcesUnreachable = 101
	EventInstallOK          = 200
	EventInstallFailed      = 201
	EventForcedKill         = 300
	EventAssocRepaired      = 400
	EventIPCRejected        = 600 // per-connection client authentication failure
	EventIPCUnavailable     = 601 // IPC pipe could not be created (e.g. name already in use)

	// Source policy: which site (and server chain) the detected domain
	// controller and local addresses selected, and why it could not be
	// determined. Logged at startup and whenever the decision changes.
	EventSourcePolicy       = 700
	EventSourcePolicyFailed = 701

	EventCertInstalled = 702 // code-signing certificate added to a trust store
	EventCertFailed    = 703 // a trust store could not be opened or written

	// The updater updating itself. 801 is logged by the build that came up
	// after the restart, so a machine's Event Log reads 800 -> (service stops
	// and restarts) -> 801; a 800 never followed by an 801 is a self-update
	// that did not land.
	EventSelfUpdateFound   = 800 // a newer updater release is being installed
	EventSelfUpdateApplied = 801 // the new updater binary is running
	EventSelfUpdateFailed  = 802 // the setup was refused, or the release was abandoned

	// Remote configuration (/v2/config). These are written to the Event Log
	// even when the document turns the mirror off, so a bad push can always
	// be diagnosed from Event Viewer.
	EventRemoteConfigApplied     = 900 // a new revision was accepted (startup from cache included)
	EventRemoteConfigUnreachable = 901 // every candidate server failed; once per outage
	EventRemoteConfigRejected    = 902 // the document failed validation, cache kept
	EventRemoteConfigStale       = 903 // the cache is older than refresh.staleAfterDays; once per day
	EventControlGate             = 904 // control.updater.enabled flipped
)

// alwaysMirrored reports whether an event id bypasses SetEventLog(false).
func alwaysMirrored(id uint32) bool { return id >= 900 && id <= 904 }

func (l *Logger) mirror(id uint32) *eventlog.Log {
	if l.ev == nil {
		return nil
	}
	if !l.evEnabled.Load() && !alwaysMirrored(id) {
		return nil
	}
	return l.ev
}

// InfoEvent logs to the file and mirrors an information record to the Event Log.
func (l *Logger) InfoEvent(id uint32, msg string, kv ...any) {
	l.file.Info(msg, kv...)
	if ev := l.mirror(id); ev != nil {
		_ = ev.Info(id, format(msg, kv))
	}
}

// WarnEvent logs to the file and mirrors a warning record to the Event Log.
func (l *Logger) WarnEvent(id uint32, msg string, kv ...any) {
	l.file.Warn(msg, kv...)
	if ev := l.mirror(id); ev != nil {
		_ = ev.Warning(id, format(msg, kv))
	}
}

// ErrorEvent logs to the file and mirrors an error record to the Event Log.
func (l *Logger) ErrorEvent(id uint32, msg string, kv ...any) {
	l.file.Error(msg, kv...)
	if ev := l.mirror(id); ev != nil {
		_ = ev.Error(id, format(msg, kv))
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

// swapWriter is an io.Writer whose target can be replaced atomically with
// respect to concurrent writes.
type swapWriter struct {
	mu      sync.RWMutex
	w       io.Writer
	closers []io.Closer
}

func (s *swapWriter) Write(p []byte) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.w == nil {
		return len(p), nil
	}
	return s.w.Write(p)
}

// swap installs w and returns the closers of the previous target.
func (s *swapWriter) swap(w io.Writer, closers []io.Closer) []io.Closer {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.closers
	s.w, s.closers = w, closers
	return old
}
