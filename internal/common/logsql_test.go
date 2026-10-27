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

// LogInteractionSQL is a debug-only view of the statements the tool exchanges
// with the server. It must be completely silent unless the level is debug, so a
// default run keeps the byte-for-byte quiet log it had before this existed.
func TestLogInteractionSQLSilentAboveDebug(t *testing.T) {
	defer InitLogger(config.LogLevelInfo)
	InitLogger(config.LogLevelInfo)

	out := captureStderr(t, func() {
		LogInteractionSQL("SHOW CREATE TABLE `t`")
	})
	if out != "" {
		t.Errorf("info-level run leaked an interaction line: %q", out)
	}
	if DebugEnabled() {
		t.Error("DebugEnabled() true at info level")
	}
}

// At debug the statement is emitted, and bound boundary/arg values stay redacted
// to type tags via SQLPreview — the same disclosure rule that governs failing-SQL
// echoes. A real value must never reach the sink even under debug.
func TestLogInteractionSQLDebugRedactsArgs(t *testing.T) {
	defer InitLogger(config.LogLevelInfo)
	InitLogger(config.LogLevelInfo)
	InitLogger(config.LogLevelDebug)
	if !DebugEnabled() {
		t.Fatal("DebugEnabled() false at debug level")
	}

	const secret = "hunter2secret"
	out := captureStderr(t, func() {
		LogInteractionSQL("SELECT k FROM `t` WHERE k > ? ORDER BY k LIMIT 1 OFFSET 9", secret)
	})

	if !strings.Contains(out, "db interaction") {
		t.Fatalf("missing debug record: %q", out)
	}
	if !strings.Contains(out, "SELECT k FROM") {
		t.Errorf("statement text not echoed: %q", out)
	}
	if strings.Contains(out, secret) {
		t.Errorf("bound value leaked into the debug echo: %q", out)
	}
	wantTag := "str(" + strconv.Itoa(len(secret)) + ")"
	if !strings.Contains(out, wantTag) {
		t.Errorf("expected redacted %s tag for the bound value: %q", wantTag, out)
	}
}
