package common

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/gomydump/internal/config"
)

var (
	// Logger is the global structured logger instance.
	Logger *slog.Logger

	// Version is injected at build time via ldflags.
	Version = "dev"

	// BuildTime is injected at build time via ldflags.
	BuildTime = "unknown"

	// GitCommit is injected at build time via ldflags.
	GitCommit = "unknown"

	// GoVersion is the Go runtime version.
	GoVersion = runtime.Version()

	// ErrorStack, when true, includes the full error call stack in the ERROR
	// record emitted at the end of a failed run. Controlled by --error-stack /
	// GOMYDUMP_ERROR_STACK.
	ErrorStack bool

	// Quiet, when true, suppresses the periodic (5s) progress logs. Completion
	// summaries, configuration and ERROR records are still printed. Controlled
	// by --quiet / GOMYDUMP_QUIET.
	Quiet bool

	// LogFormat selects the record layout: "java" (the default, a fixed-width
	// `time level detail` line resembling a JUL formatter, which greps and
	// tail-friendly) or "text" (slog's own key=value handler). Controlled by
	// --log-format / GOMYDUMP_LOG_FORMAT.
	LogFormat = "java"
)

// logSink is the single destination every log record goes through.
var logSink = &sink{}

// sink is the one destination every log record goes through: the --messages file
// while one is open, stderr otherwise. The file takes the run over instead of
// copying it, so a run with -m prints nothing on the terminal — the way DB2's
// MESSAGES keeps its report to itself.
//
// Both layouts hold this one value instead of an os.Stderr, so --messages can
// take the log over without rebuilding handlers that slog's text handler would
// pin to a writer captured at construction. stderr itself is resolved inside
// Write rather than stored, so reassigning os.Stderr — as the logger tests do —
// still takes effect, exactly as it did when the handlers wrote os.Stderr
// directly.
//
// The mutex is needed in its own right: the progress monitor and the shard
// producers log from different goroutines, and a record split across two
// writers is a record lost.
type sink struct {
	mu sync.Mutex
	f  *os.File // the --messages file, nil when the option was not given
}

func (s *sink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f != nil {
		return s.f.Write(p)
	}
	return os.Stderr.Write(p)
}

// OpenLogFile hands the command log over to path: every subsequent record goes
// to the file instead of to stderr, until CloseLogFile gives it back. The file is
// created and truncated, so the first record written after this call is its first
// line — which is why the root command opens it before printing the version
// banner. Opening a second file closes the first.
func OpenLogFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return errors.Wrapf(err, "failed to create message file %s", path)
	}
	logSink.mu.Lock()
	defer logSink.mu.Unlock()
	if logSink.f != nil {
		logSink.f.Close()
	}
	logSink.f = f
	return nil
}

// CloseLogFile detaches and closes the message file, returning the log to stderr.
// Detaching happens before the close, so a record about a failed close still
// reaches the operator on stderr instead of going into a handle that is already
// being torn down — with the file attached, stderr would otherwise be silent.
func CloseLogFile() error {
	logSink.mu.Lock()
	f := logSink.f
	logSink.f = nil
	logSink.mu.Unlock()
	if f == nil {
		return nil
	}
	if err := f.Close(); err != nil {
		return errors.Wrapf(err, "failed to close message file %s", f.Name())
	}
	return nil
}

func init() {
	// Provide a usable default logger so early failures (before InitLogger runs
	// inside a subcommand) can still be reported at ERROR level, in the default
	// layout.
	Logger = newLogger(config.LogLevelInfo, LogFormatJava)
}

// LogVersion prints the version banner. It must be the first log line of a run.
func LogVersion(command string) {
	Logger.Info("gomydump starting",
		"version", Version,
		"build_time", BuildTime,
		"git_commit", GitCommit,
		"go_version", GoVersion,
		"command", command,
	)
}

// InitLogger initializes the global logger with the given log level.
func InitLogger(level config.LogLevel) {
	Logger = newLogger(level, LogFormat)
}

// ApplyLogFormat re-applies the selected layout without changing the level. It
// runs in the root command's PersistentPreRunE so that even a usage error
// raised before InitLogger is already printed in the chosen style.
func ApplyLogFormat() {
	Logger = newLogger(currentLevel(), LogFormat)
}

// newLogger builds the global logger for a level and layout.
func newLogger(level config.LogLevel, format string) *slog.Logger {
	var slogLevel slog.Level
	switch level {
	case config.LogLevelDebug:
		slogLevel = slog.LevelDebug
	case config.LogLevelWarn:
		slogLevel = slog.LevelWarn
	case config.LogLevelError:
		slogLevel = slog.LevelError
	default:
		slogLevel = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: slogLevel}
	if format == LogFormatText {
		return slog.New(slog.NewTextHandler(logSink, opts))
	}
	return slog.New(&javaHandler{level: slogLevel, pid: os.Getpid()})
}

// Layout names accepted by --log-format.
const (
	LogFormatJava = "java"
	LogFormatText = "text"
)

// validLogFormat is the set --log-format accepts; cmd/validate.go's
// validateLogFormat is called from the root command's PersistentPreRunE so a
// typo fails with exit code 2 instead of silently falling back.
func ValidLogFormat(s string) bool {
	return s == LogFormatJava || s == LogFormatText
}

// currentLevel reads the level back out of the active handler so ApplyLogFormat
// can restyle without resetting it. The probes must walk down from Debug, one
// step at a time: skipping Info made an info handler look like "warn" (Info is
// enabled, so Enabled(Warn) is trivially true as well), and every restyle then
// raised the level one more step until only ERROR survived — which silently
// dropped the version banner and any pre-InitLogger INFO record.
func currentLevel() config.LogLevel {
	switch {
	case Logger.Enabled(context.Background(), slog.LevelDebug):
		return config.LogLevelDebug
	case Logger.Enabled(context.Background(), slog.LevelInfo):
		return config.LogLevelInfo
	case Logger.Enabled(context.Background(), slog.LevelWarn):
		return config.LogLevelWarn
	case Logger.Enabled(context.Background(), slog.LevelError):
		return config.LogLevelError
	default:
		return config.LogLevelInfo
	}
}

// javaHandler renders one record per line as
//
//	2026-01-02 15:04:05.123 [pid] LEVEL message key=value ...
//
// The fixed-width timestamp, the process id and the uppercase level make the
// output directly greppable and familiar to operators coming from a JUL-style
// MySQL/OceanBase log. It is the default layout; "text" keeps slog's own
// key=value handler for machine parsing.
type javaHandler struct {
	level slog.Level
	pid   int
	attrs []slog.Attr
}

func (h *javaHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *javaHandler) Handle(_ context.Context, r slog.Record) error {
	buf := bufferPool.Get().(*bytes.Buffer)
	defer bufferPool.Put(buf)

	t := r.Time
	if t.IsZero() {
		t = time.Now()
	}
	buf.WriteString(t.Format("2006-01-02 15:04:05.000"))
	buf.WriteByte(' ')
	buf.WriteByte('[')
	buf.WriteString(strconv.Itoa(h.pid))
	buf.WriteString("] ")

	levelName := strings.ToUpper(r.Level.String())
	buf.WriteString(levelName)
	buf.WriteByte(' ')
	buf.WriteString(r.Message)

	for _, a := range h.attrs {
		writeAttr(buf, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		writeAttr(buf, a)
		return true
	})

	buf.WriteByte('\n')
	_, err := logSink.Write(buf.Bytes())
	buf.Reset()
	return err
}

func (h *javaHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := *h
	nh.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &nh
}

func (h *javaHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.WithAttrs([]slog.Attr{{Key: name}})
}

func writeAttr(buf *bytes.Buffer, a slog.Attr) {
	if a.Equal(slog.Attr{}) {
		return
	}
	buf.WriteByte(' ')
	buf.WriteString(a.Key)
	buf.WriteByte('=')
	v := a.Value.Any()
	s, ok := v.(string)
	if !ok {
		s = fmt.Sprint(v)
	}
	// A record must occupy exactly one physical line: delimiters and quote
	// characters are legitimate values here (char_del=" row_del="\n"), and an
	// unescaped newline splits the record so grep/tail lose everything after it.
	// Quote like slog's own text handler does, so both layouts render the same
	// value as the same token.
	if needsQuoting(s) {
		buf.WriteString(strconv.Quote(s))
		return
	}
	buf.WriteString(s)
}

// needsQuoting reports whether a value has to be quoted to stay one token on
// one line: empty, or containing whitespace, a control character, a double
// quote, a backslash, or '=' (which would otherwise read as a new key).
func needsQuoting(s string) bool {
	if s == "" {
		return true
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c == 0x7f || c == '"' || c == '\\' || c == '=' {
			return true
		}
	}
	return false
}

var bufferPool = sync.Pool{New: func() interface{} { return new(bytes.Buffer) }}
