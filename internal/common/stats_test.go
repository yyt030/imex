package common

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A run must not end with a progress record: "export progress" written after
// "export completed" tells whoever reads the log that the job is still going.
// The window is real — a report that is already in flight (a slow writer holds
// the record for tens of milliseconds) cannot be recalled — so stop() has to
// drain the monitor goroutine, and the caller has to stop the monitor before it
// writes its summary.
func TestMonitorStopDrainsInFlightReport(t *testing.T) {
	const slow = 60 * time.Millisecond

	var mu sync.Mutex
	var records []string
	slowed := false
	inFlight := make(chan struct{})

	stats := NewStats()
	stats.AddProcessed(10)

	stop := StartMonitor(context.Background(), 2*time.Millisecond, stats, func(prev, curr StatsSnapshot) {
		mu.Lock()
		hold := !slowed
		slowed = true
		mu.Unlock()
		if hold {
			// First report only: it stands in for a record stuck in the writer.
			close(inFlight)
			time.Sleep(slow)
		}
		mu.Lock()
		records = append(records, "progress")
		mu.Unlock()
	})

	<-inFlight // a report is executing and has not written its record yet

	// The caller stops the monitor first, then writes the summary — the order the
	// exporter and importer now use.
	stop()
	mu.Lock()
	records = append(records, "completed")
	untilSummary := len(records)
	mu.Unlock()

	// Calling stop twice is the deferred-stop pattern (stop before the summary,
	// stop again on the way out); it must return, not deadlock on the drain. It is
	// checked before the final assertions so it cannot be holding the mutex the
	// drained report would need.
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("second stop() blocked")
	}

	// Give any report that stop() failed to drain, and any later tick, the time
	// to land.
	time.Sleep(4 * slow)

	mu.Lock()
	defer mu.Unlock()
	got := append([]string{}, records...)
	// A pending tick may still be reported while the monitor is being drained
	// (that is what the drain is for), so the count is not fixed — the summary is
	// what must be last, and nothing may be added after it.
	if got[len(got)-1] != "completed" {
		t.Fatalf("the run ended on %q, want the completion record to be last: %v", got[len(got)-1], got)
	}
	if len(got) != untilSummary {
		t.Errorf("monitor kept reporting after stop returned: %v", got)
	}
	if untilSummary < 2 {
		t.Fatalf("the in-flight report never landed before the summary, so the drain was not exercised: %v", got)
	}
}

// MonitorInterval is what the commands pass to StartMonitor; the default is the
// documented 5 s progress cadence.
func TestMonitorIntervalDefaultIsFiveSeconds(t *testing.T) {
	if MonitorInterval != 5*time.Second {
		t.Errorf("MonitorInterval = %v, want 5s", MonitorInterval)
	}
}
