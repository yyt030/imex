package common

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/imex/internal/config"
)

// The operator asked that every timing in the command log read in plain seconds
// with three decimals. Seconds must render sub-second, whole-second, and
// multi-second values the same way, unlike time.Duration.String() which switches
// units ("2.5ms", "45s", "1m0s").
func TestSecondsAlwaysPlainSeconds(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{842 * time.Millisecond, "0.842s"},
		{2500 * time.Millisecond, "2.500s"},
		{45 * time.Second, "45.000s"},
		{90 * time.Second, "90.000s"},
		{5 * time.Millisecond, "0.005s"},
		{0, "0.000s"},
	}
	for _, c := range cases {
		if got := Seconds(c.d); got != c.want {
			t.Errorf("Seconds(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// The team asked to see the statements the tool sends without raising the log
// level first, so the trace carries at the default info level and disappears
// only above it. A warn run must not emit a single interaction line, and
// InfoEnabled has to agree with what the logger actually writes.
func TestLogInteractionSQLCarriesAtDefaultLevel(t *testing.T) {
	defer InitLogger(config.LogLevelInfo)
	InitLogger(config.LogLevelInfo)
	if !InfoEnabled() {
		t.Fatal("InfoEnabled() false at the default level")
	}

	out := captureStderr(t, func() {
		LogInteractionSQL("SHOW CREATE TABLE `t`")
	})
	if !strings.Contains(out, "db interaction") {
		t.Errorf("default-level run emitted no interaction line: %q", out)
	}
	if !strings.Contains(out, "SHOW CREATE TABLE") {
		t.Errorf("statement text not echoed: %q", out)
	}

	InitLogger(config.LogLevelWarn)
	if InfoEnabled() {
		t.Error("InfoEnabled() true at warn level")
	}
	out = captureStderr(t, func() {
		LogInteractionSQL("SHOW CREATE TABLE `t`")
	})
	if out != "" {
		t.Errorf("warn-level run leaked an interaction line: %q", out)
	}
}

// The statement text is echoed as written — up to MaxSQLInteractionPreview — but
// bound boundary/arg values stay redacted to type tags through
// SQLInteractionPreview, the same disclosure rule that governs failing-SQL
// echoes, and the reason the trace is safe to carry at the level that is on by
// default. A real value must never reach the sink.
func TestLogInteractionSQLRedactsArgs(t *testing.T) {
	defer InitLogger(config.LogLevelInfo)
	InitLogger(config.LogLevelInfo)

	const secret = "hunter2secret"
	out := captureStderr(t, func() {
		LogInteractionSQL("SELECT k FROM `t` WHERE k > ? ORDER BY k LIMIT 1 OFFSET 9", secret)
	})

	if !strings.Contains(out, "db interaction") {
		t.Fatalf("missing info record: %q", out)
	}
	if !strings.Contains(out, "SELECT k FROM") {
		t.Errorf("statement text not echoed: %q", out)
	}
	if strings.Contains(out, secret) {
		t.Errorf("bound value leaked into the interaction echo: %q", out)
	}
	wantTag := "str(" + strconv.Itoa(len(secret)) + ")"
	if !strings.Contains(out, wantTag) {
		t.Errorf("expected redacted %s tag for the bound value: %q", wantTag, out)
	}
}

// A parallel export embeds the caller's whole query into every shard SELECT, so
// the trace cannot afford the error echo's 2048-byte preview: 64 shard ranges of
// a multi-kilobyte IN list used to write more log than product (measured
// 165,731 log bytes against a 36,550-byte dump). The trace clips at
// MaxSQLInteractionPreview; the failing-SQL echo still gets the full error cap,
// because an error prints one statement per run and the reader needs all of it.
func TestLogInteractionSQLClipsLongerThanTheErrorPreview(t *testing.T) {
	defer InitLogger(config.LogLevelInfo)
	InitLogger(config.LogLevelInfo)

	var b strings.Builder
	b.WriteString("SELECT * FROM big WHERE id IN (")
	for i := 1; i <= 900; i++ {
		if i > 1 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.Itoa(i))
	}
	b.WriteString(")")
	query := b.String()
	if len(query) <= MaxSQLErrorPreview {
		t.Fatalf("fixture is %d bytes, too short to separate the two caps", len(query))
	}

	out := captureStderr(t, func() { LogInteractionSQL(query) })
	mark := strings.Index(out, "sql=\"")
	if mark < 0 {
		t.Fatalf("no sql attribute in %q", out)
	}
	got := out[mark+len("sql=\"") : strings.LastIndex(out, "\"")]
	if len(got) > MaxSQLInteractionPreview+len(" /* ... truncated */") {
		t.Errorf("trace echo is %d bytes, above the %d-byte cap: %q", len(got), MaxSQLInteractionPreview, got)
	}
	if !strings.HasSuffix(got, "/* ... truncated */") {
		t.Errorf("long trace echo was not clipped with the marker: %q", got)
	}
	if !strings.HasPrefix(got, "SELECT * FROM big WHERE id IN") {
		t.Errorf("clipped trace echo lost its statement head: %q", got)
	}

	// The same statement on the failure path keeps the wider cap.
	errPreview := SQLPreview(query, nil)
	if len(errPreview) <= MaxSQLInteractionPreview {
		t.Errorf("error preview clipped to the trace cap (%d bytes), want room up to %d", len(errPreview), MaxSQLErrorPreview)
	}
	if len(errPreview) > MaxSQLErrorPreview+len(" /* ... truncated */") {
		t.Errorf("error preview is %d bytes, above its own cap", len(errPreview))
	}
}
