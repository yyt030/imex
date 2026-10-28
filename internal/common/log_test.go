package common

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/imex/internal/config"
)

// captureStderr redirects os.Stderr into a temp file for the duration of fn and
// returns what was written. The console handler resolves os.Stderr at write time,
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
func TestConsoleHandlerKeepsOneRecordPerLine(t *testing.T) {
	out := captureStderr(t, func() {
		logger := newLogger(config.LogLevelInfo, LogFormatConsole)
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
		LogFormat = LogFormatConsole
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
	LogFormat = LogFormatConsole
	InitLogger(config.LogLevelInfo)
	ApplyLogFormat()

	out := captureStderr(t, func() { LogVersion("export") })
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("banner spanned %d lines: %q", strings.Count(out, "\n"), out)
	}
	for _, w := range []string{"INFO imex starting", "command=export", "version="} {
		if !strings.Contains(out, w) {
			t.Errorf("banner %q missing %q", out, w)
		}
	}
}

// --messages is the command log's file: the sink is what makes that possible, so
// its contract lives here. While the file is open it receives every record and
// stderr receives none — the terminal is silent for the whole run — and closing
// it hands the log back.
func TestLogFileTakesOverFromStderr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	logger := newLogger(config.LogLevelInfo, LogFormatConsole)

	out := captureStderr(t, func() {
		if err := OpenLogFile(path); err != nil {
			t.Fatalf("open log file: %v", err)
		}
		defer func() {
			if err := CloseLogFile(); err != nil {
				t.Fatalf("close log file: %v", err)
			}
		}()
		logger.Info("export config", "output", "/tmp/a.del")
		logger.Warn("single-row INSERT exceeds --statement-size", "statement_bytes", 4096)
		logger.Error("command failed", "error", "no such table")
	})

	if out != "" {
		t.Errorf("a run with --messages must print nothing to stderr: %q", out)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if n := strings.Count(string(data), "\n"); n != 3 {
		t.Fatalf("log file holds %d records, want all 3:\n%s", n, string(data))
	}
	for _, want := range []string{"export config", "single-row INSERT", "command failed"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("log file missing %q:\n%s", want, string(data))
		}
	}

	// Once closed, the file is detached: further records go back to stderr and the
	// file stays as it was.
	size := len(data)
	after := captureStderr(t, func() { logger.Info("after close") })
	if !strings.Contains(after, "after close") {
		t.Errorf("record lost after CloseLogFile: %q", after)
	}
	data2, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read log file: %v", err)
	}
	if len(data2) != size {
		t.Errorf("file grew after it was closed: %q", string(data2[size:]))
	}
	// Closing twice (a run without --messages goes through the same path) is a
	// no-op, not an error.
	if err := CloseLogFile(); err != nil {
		t.Errorf("second CloseLogFile: %v", err)
	}
}

// The handlers are built once, when the process starts, and stay in place for the
// whole run — slog's text handler would otherwise have pinned the writer it was
// given. Opening --messages later must therefore still reach both layouts.
func TestLogFileFollowsHandlersBuiltEarlier(t *testing.T) {
	path := filepath.Join(t.TempDir(), "text.log")
	logger := newLogger(config.LogLevelInfo, LogFormatText)

	out := captureStderr(t, func() {
		if err := OpenLogFile(path); err != nil {
			t.Fatalf("open log file: %v", err)
		}
		defer func() { _ = CloseLogFile() }()
		logger.Info("import config", "table", "users")
	})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if out != "" {
		t.Errorf("a handler built before the open must hand its records to the file, not to stderr: %q", out)
	}
	if !strings.Contains(string(data), `msg="import config"`) || !strings.Contains(string(data), "table=users") {
		t.Errorf("unexpected text layout in the file: %q", string(data))
	}
}

// The sink is shared by the progress monitor and the shard producers, so it also
// owns serialization: concurrent writers must never interleave two records.
func TestLogFileSerializesConcurrentRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.log")
	logger := newLogger(config.LogLevelInfo, LogFormatConsole)

	if err := OpenLogFile(path); err != nil {
		t.Fatalf("open log file: %v", err)
	}
	t.Cleanup(func() { _ = CloseLogFile() })

	const shards, perShard = 8, 25
	var wg sync.WaitGroup
	for i := 0; i < shards; i++ {
		wg.Add(1)
		go func(shard int) {
			defer wg.Done()
			for j := 0; j < perShard; j++ {
				logger.Info("shard export completed", "shard", shard, "seq", j)
			}
		}(i)
	}
	wg.Wait()

	if err := CloseLogFile(); err != nil {
		t.Fatalf("close log file: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != shards*perShard {
		t.Fatalf("file holds %d lines, want %d records", len(lines), shards*perShard)
	}
	// Every record must be one complete line: an interleaved write would show up
	// here as a missing "shard=<i> seq=<j>" pair or a broken token.
	seen := map[string]bool{}
	for _, line := range lines {
		i := strings.Index(line, "shard=")
		if i < 0 || strings.Count(line, "shard=") != 1 {
			t.Errorf("record split or merged: %q", line)
			continue
		}
		key := line[i:]
		if seen[key] {
			t.Errorf("duplicated record: %q", line)
		}
		seen[key] = true
	}
	if len(seen) != shards*perShard {
		t.Errorf("%d distinct records, want %d", len(seen), shards*perShard)
	}
	for i := 0; i < shards; i++ {
		for j := 0; j < perShard; j++ {
			if !seen[fmt.Sprintf("shard=%d seq=%d", i, j)] {
				t.Errorf("record shard=%d seq=%d missing or truncated", i, j)
			}
		}
	}
}

// A second --messages path replaces the first rather than silently keeping both
// attached (the live tests re-point the sink per case).
func TestLogFileReplaceKeepsOnlyTheNewest(t *testing.T) {
	first := filepath.Join(t.TempDir(), "first.log")
	second := filepath.Join(t.TempDir(), "second.log")
	logger := newLogger(config.LogLevelInfo, LogFormatConsole)

	captureStderr(t, func() {
		if err := OpenLogFile(first); err != nil {
			t.Fatalf("open first: %v", err)
		}
		logger.Info("record for the first file")
		if err := OpenLogFile(second); err != nil {
			t.Fatalf("open second: %v", err)
		}
		logger.Info("record after the replacement")
		if err := CloseLogFile(); err != nil {
			t.Fatalf("close: %v", err)
		}
	})

	a, err := os.ReadFile(first)
	if err != nil {
		t.Fatalf("read first: %v", err)
	}
	b, err := os.ReadFile(second)
	if err != nil {
		t.Fatalf("read second: %v", err)
	}
	if !strings.Contains(string(a), "record for the first file") {
		t.Errorf("the first file missed its own record: %q", string(a))
	}
	if strings.Contains(string(a), "after the replacement") {
		t.Errorf("the replaced file is still attached: %q", string(a))
	}
	if !strings.Contains(string(b), "record after the replacement") {
		t.Errorf("the current file missed the record: %q", string(b))
	}
	if strings.Contains(string(b), "record for the first file") {
		t.Errorf("opening the second file must not replay history: %q", string(b))
	}
}

// A --messages path that cannot be created must fail with the path named, and
// must leave the sink alone: the log keeps going to stderr by itself.
func TestOpenLogFileRejectsBadPath(t *testing.T) {
	t.Cleanup(func() { _ = CloseLogFile() })

	before := captureStderr(t, func() { Logger.Info("before") })
	err := OpenLogFile(filepath.Join(t.TempDir(), "missing-dir", "run.log"))
	if err == nil || !strings.Contains(err.Error(), "missing-dir") {
		t.Fatalf("want an error naming the path, got %v", err)
	}
	after := captureStderr(t, func() { Logger.Info("after") })
	if !strings.Contains(before, "before") || !strings.Contains(after, "after") {
		t.Errorf("a failed open must leave stderr working: %q / %q", before, after)
	}
}
