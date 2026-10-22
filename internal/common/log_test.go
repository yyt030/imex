package common

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gomydump/internal/config"
)

// captureStderr redirects os.Stderr into a temp file for the duration of fn and
// returns what was written. The java handler resolves os.Stderr at write time,
// so replacing the package variable is enough.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stderr")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create capture file: %v", err)
	}
	orig := os.Stderr
	os.Stderr = f
	fn()
	os.Stderr = orig
	if err := f.Close(); err != nil {
		t.Fatalf("close capture file: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	return string(data)
}

// A log record must occupy exactly one physical line, whatever the values are.
// The configuration line logs the DEL delimiters themselves, so a raw newline
// (row_del="\n") used to split one record into several lines and an unquoted
// double quote (char_del="") made the following tokens unreadable.
func TestJavaHandlerKeepsOneRecordPerLine(t *testing.T) {
	out := captureStderr(t, func() {
		logger := newLogger(config.LogLevelInfo, LogFormatJava)
		logger.Info("export configuration",
			"char_del", `"`,
			"col_del", ",",
			"row_del", "\n",
			"escape", `\`,
			"shard_key", "",
			"timestamp_fmt", "yyyy-MM-dd HH:mm:ss",
			"no_col_names", false,
			"statement_size", 8388608,
		)
	})

	if strings.Count(out, "\n") != 1 {
		t.Fatalf("record spanned %d lines, want exactly 1: %q", strings.Count(out, "\n"), out)
	}
	want := []string{
		`char_del="\""`,
		`col_del=,`,
		`row_del="\n"`,
		`escape="\\"`,
		`shard_key=""`,
		`timestamp_fmt="yyyy-MM-dd HH:mm:ss"`,
		`no_col_names=false`,
		`statement_size=8388608`,
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output %q missing token %q", out, w)
		}
	}
}

// needsQuoting drives the single-line guarantee; '=' and '\' must be quoted as
// well, otherwise a value reads as an extra key or breaks the escaping.
func TestNeedsQuoting(t *testing.T) {
	quoted := []string{"", "a b", "a\tb", "a\nb", `"`, `\`, "a=b", "\x7f", `C:\path`}
	plain := []string{"utf8mb4", "yyyy-MM-dd", "8388608", "false", "0.1.0", "a,b"}
	for _, s := range quoted {
		if !needsQuoting(s) {
			t.Errorf("needsQuoting(%q) = false, want true", s)
		}
	}
	for _, s := range plain {
		if needsQuoting(s) {
			t.Errorf("needsQuoting(%q) = true, want false", s)
		}
	}
}

// ApplyLogFormat restyles without changing the level. Reading the level back by
// probing only debug/warn/error made an info handler look like "warn", so every
// restyle raised the threshold one step and INFO records (including the version
// banner) disappeared while the layout stayed valid.
func TestApplyLogFormatPreservesLevel(t *testing.T) {
	levels := []config.LogLevel{config.LogLevelDebug, config.LogLevelInfo, config.LogLevelWarn, config.LogLevelError}
	want := []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError}

	defer func(orig string) { LogFormat = orig; InitLogger(config.LogLevelInfo) }(LogFormat)
	for i, l := range levels {
		LogFormat = LogFormatJava
		InitLogger(l)
		before := enabledLevels()
		ApplyLogFormat()
		after := enabledLevels()
		if before != after {
			t.Errorf("level %q: enabled mask before=%s after=%s, restyle must not change it", l, before, after)
		}
		if !Logger.Enabled(context.Background(), want[i]) || (want[i] > slog.LevelDebug && Logger.Enabled(context.Background(), want[i]-1)) {
			t.Errorf("level %q: handler threshold is not %q (mask=%s)", l, want[i], after)
		}
	}
}

// enabledLevels renders a fixed-width mask of which levels pass, so a threshold
// drift is visible in the failure message.
func enabledLevels() string {
	var b bytes.Buffer
	for _, l := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		if Logger.Enabled(context.Background(), l) {
			b.WriteByte('1')
			continue
		}
		b.WriteByte('0')
	}
	return b.String()
}

// LogVersion must actually emit the banner in the current layout: it is the
// documented first line of every run.
func TestLogVersionEmitsBanner(t *testing.T) {
	LogFormat = LogFormatJava
	InitLogger(config.LogLevelInfo)
	ApplyLogFormat()

	out := captureStderr(t, func() { LogVersion("export") })
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("banner spanned %d lines: %q", strings.Count(out, "\n"), out)
	}
	for _, w := range []string{"INFO gomydump starting", "command=export", "version="} {
		if !strings.Contains(out, w) {
			t.Errorf("banner %q missing %q", out, w)
		}
	}
}
