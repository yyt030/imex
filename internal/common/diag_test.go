package common

import (
	"strings"
	"testing"
	"time"

	"github.com/imex/internal/config"
)

// TestStallWatch pins the checkpoint arithmetic: the clock is read once per
// ScanStallCheckEvery rows, gaps are measured between checkpoints, and only a
// gap above the threshold reports as stalled.
func TestStallWatch(t *testing.T) {
	base := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	newWatch := func() *StallWatch {
		w := NewStallWatch(10 * time.Second)
		w.last = base
		return w
	}

	t.Run("below the checkpoint no clock is consulted", func(t *testing.T) {
		w := newWatch()
		for i := 0; i < ScanStallCheckEvery-1; i++ {
			if gap, stalled := w.observe(base.Add(time.Hour)); stalled || gap != 0 {
				t.Fatalf("row %d reported gap=%v stalled=%v, want silence", i, gap, stalled)
			}
		}
	})

	t.Run("a healthy checkpoint is measured but not reported", func(t *testing.T) {
		w := newWatch()
		var gap time.Duration
		var stalled bool
		for i := 0; i < ScanStallCheckEvery; i++ {
			gap, stalled = w.observe(base.Add(2 * time.Second))
		}
		if stalled {
			t.Fatalf("2s gap reported as stalled")
		}
		if gap != 2*time.Second {
			t.Fatalf("gap = %v, want 2s", gap)
		}
	})

	t.Run("a gap above the threshold reports at the checkpoint", func(t *testing.T) {
		w := newWatch()
		var gap time.Duration
		var stalled bool
		for i := 0; i < ScanStallCheckEvery; i++ {
			gap, stalled = w.observe(base.Add(42 * time.Second))
		}
		if !stalled {
			t.Fatal("42s gap not reported as stalled")
		}
		if gap != 42*time.Second {
			t.Fatalf("gap = %v, want 42s", gap)
		}
	})

	t.Run("the checkpoint anchor moves with each report", func(t *testing.T) {
		w := newWatch()
		for i := 0; i < ScanStallCheckEvery; i++ {
			w.observe(base.Add(42 * time.Second))
		}
		// The next checkpoint is measured from the previous one, not from the
		// construction time, so a healthy interval after a stall stays quiet.
		var gap time.Duration
		var stalled bool
		for i := 0; i < ScanStallCheckEvery; i++ {
			gap, stalled = w.observe(base.Add(43 * time.Second))
		}
		if stalled {
			t.Fatalf("1s gap after a reported stall also reported (gap=%v)", gap)
		}
	})

	t.Run("zero values stay silent", func(t *testing.T) {
		var nilWatch *StallWatch
		if _, stalled := nilWatch.observe(base); stalled {
			t.Fatal("nil watch reported a stall")
		}
		noThreshold := NewStallWatch(0)
		for i := 0; i < ScanStallCheckEvery*2; i++ {
			if _, stalled := noThreshold.observe(base.Add(time.Hour)); stalled {
				t.Fatal("watch without a threshold reported a stall")
			}
		}
	})
}

// TestWarnIfSlow pins the threshold gate and the emitted fields: nothing below
// the threshold, exactly one record at or above it, with the caller's
// attributes first and duration last.
func TestWarnIfSlow(t *testing.T) {
	origLevel := Logger
	t.Cleanup(func() { Logger = origLevel })
	Logger = newLogger(config.LogLevelDebug, LogFormatConsole)

	out := captureStderr(t, func() {
		WarnIfSlow(5*time.Second, "slow insert batch", 4999*time.Millisecond, "rows", 1000)
		WarnIfSlow(5*time.Second, "slow insert batch", 5*time.Second, "rows", 1000, "bytes", 4096)
	})

	if strings.Contains(out, "4.999s") {
		t.Fatalf("a duration below the threshold was reported:\n%s", out)
	}
	if got := strings.Count(out, "WARN slow insert batch"); got != 1 {
		t.Fatalf("slow batch records = %d, want 1:\n%s", got, out)
	}
	if !strings.Contains(out, "slow insert batch rows=1000 bytes=4096 duration=5.000s") {
		t.Fatalf("record fields are not in the documented order:\n%s", out)
	}

	quiet := captureStderr(t, func() {
		WarnIfSlow(0, "slow insert batch", time.Hour)
	})
	if quiet != "" {
		t.Fatalf("a zero threshold emitted %q, want nothing", quiet)
	}
}
