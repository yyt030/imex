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
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(&javaHandler{level: slogLevel, pid: os.Getpid()})
}

// Layout names accepted by --log-format.
const (
	LogFormatJava = "java"
	LogFormatText = "text"
)

// validLogFormat is the set --log-format accepts; cmd/validate.go checks
// membership so a typo fails with exit code 2 instead of silently falling back.
func ValidLogFormat(s string) bool {
	return s == LogFormatJava || s == LogFormatText
}

// currentLevel reads the level back out of the active handler so ApplyLogFormat
// can restyle without resetting it.
func currentLevel() config.LogLevel {
	switch {
	case Logger.Enabled(context.Background(), slog.LevelDebug):
		return config.LogLevelDebug
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
	_, err := os.Stderr.Write(buf.Bytes())
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
	s := fmt.Sprint(v)
	// Quote values containing spaces so key=value pairs stay individually
	// tokenisable, mirroring what slog's handlers do.
	if strings.ContainsAny(s, " \t\n") {
		buf.WriteByte('"')
		buf.WriteString(strings.ReplaceAll(s, `"`, `\"`))
		buf.WriteByte('"')
	} else {
		buf.WriteString(s)
	}
}

var bufferPool = sync.Pool{New: func() interface{} { return new(bytes.Buffer) }}
