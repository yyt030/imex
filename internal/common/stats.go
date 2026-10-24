package common

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// StatsSnapshot is a point-in-time view of shared run counters.
type StatsSnapshot struct {
	Processed int64         // rows written (export) / rows read from file (import)
	Inserted  int64         // import only: rows the server took as new inserts
	Updated   int64         // import only: rows an existing row was changed by (insert_update)
	Unchanged int64         // import only: rows written but identical to what was already stored
	Failed    int64         // import only: rows in batches that failed to apply
	Elapsed   time.Duration // wall clock since the run started
}

// Stats is a set of atomic counters shared by worker goroutines and the
// periodic monitor. Increments are lock-free so parallel import/export
// threads can update concurrently.
type Stats struct {
	processed int64
	inserted  int64
	updated   int64
	unchanged int64
	failed    int64
	start     time.Time
}

func NewStats() *Stats { return &Stats{start: time.Now()} }

func (s *Stats) AddProcessed(n int64) { atomic.AddInt64(&s.processed, n) }
func (s *Stats) AddInserted(n int64)  { atomic.AddInt64(&s.inserted, n) }
func (s *Stats) AddUpdated(n int64)   { atomic.AddInt64(&s.updated, n) }
func (s *Stats) AddUnchanged(n int64) { atomic.AddInt64(&s.unchanged, n) }
func (s *Stats) AddFailed(n int64)    { atomic.AddInt64(&s.failed, n) }

// Snapshot reads all counters. Individual counters are atomic; the tuple may
// be slightly torn across concurrent updates, which is acceptable for a
// progress ticker.
func (s *Stats) Snapshot() StatsSnapshot {
	return StatsSnapshot{
		Processed: atomic.LoadInt64(&s.processed),
		Inserted:  atomic.LoadInt64(&s.inserted),
		Updated:   atomic.LoadInt64(&s.updated),
		Unchanged: atomic.LoadInt64(&s.unchanged),
		Failed:    atomic.LoadInt64(&s.failed),
		Elapsed:   time.Since(s.start),
	}
}

// RatePerSec is processed/elapsed; 0 when elapsed <= 0.
func (snap StatsSnapshot) RatePerSec() float64 {
	sec := snap.Elapsed.Seconds()
	if sec <= 0 {
		return 0
	}
	return float64(snap.Processed) / sec
}

// IntervalRate is (curr.Processed - prev.Processed) / (curr.Elapsed - prev.Elapsed).
func (curr StatsSnapshot) IntervalRate(prev StatsSnapshot) float64 {
	dt := (curr.Elapsed - prev.Elapsed).Seconds()
	if dt <= 0 {
		return 0
	}
	return float64(curr.Processed-prev.Processed) / dt
}

// MonitorInterval is how often StartMonitor reports a running job. The commands
// use it for the periodic progress record; it is a variable so a test can make
// the ticker fire on its own clock instead of waiting five seconds.
var MonitorInterval = 5 * time.Second

// StartMonitor spawns a goroutine that invokes report every `interval` with
// the previous and current snapshots, until ctx is canceled or the returned
// stop func is called. The first tick fires after `interval`; a final
// report-on-complete is the caller's responsibility (via its completion log).
//
// stop cancels the monitor and waits for the goroutine to leave report, so
// once it returns no progress record can still be written. Without that drain a
// run that finished while a report was in flight (the writer was busy, so the
// record took milliseconds to land) emitted "… progress" after "… completed" —
// a log that ends by saying the run is still going. stop is idempotent and safe
// to call from several places, so a caller can stop before its summary and keep
// a deferred stop for the error paths.
func StartMonitor(ctx context.Context, interval time.Duration, stats *Stats, report func(prev, curr StatsSnapshot)) (stop func()) {
	monCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		prev := stats.Snapshot()
		for {
			select {
			case <-monCtx.Done():
				return
			case <-ticker.C:
				// Both cases can be ready at once, and select then picks at
				// random: re-check cancellation so a tick that was already queued
				// when stop() ran does not produce one more record.
				if monCtx.Err() != nil {
					return
				}
				curr := stats.Snapshot()
				report(prev, curr)
				prev = curr
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(cancel)
		<-done
	}
}
