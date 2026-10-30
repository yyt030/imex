package common

import "time"

// WarnIfSlow emits one WARN record when d reaches threshold, and nothing at all
// below it. The threshold is what keeps these diagnostics out of a healthy run:
// a slow statement is worth naming only once it is slow enough to explain a
// stall, so the record carries the same fields a failure record would (what the
// operation was, how big it was) plus how long it took.
//
// It is a plain conditional rather than a sampler: callers already sit on the
// per-batch / per-commit path, where one comparison costs nothing next to the
// round-trip being measured.
func WarnIfSlow(threshold time.Duration, msg string, d time.Duration, attrs ...any) {
	if threshold <= 0 || d < threshold {
		return
	}
	a := make([]any, 0, len(attrs)+2)
	a = append(a, attrs...)
	a = append(a, "duration", Seconds(d))
	Logger.Warn(msg, a...)
}

// ScanStallCheckEvery is how many rows are read between two clock reads of a
// StallWatch. Reading the clock once per row would put a timer call on the
// hottest loop of an export; once per 64 rows bounds a reported gap to the time
// it took to read those rows (microseconds at any healthy rate) while cutting
// the clock reads by 64x. Checking every 1024 rows instead made a steady but
// slow scan (1024 rows per 10s) look like a stall, which is a different
// diagnosis: the progress record already carries that rate.
const ScanStallCheckEvery = 64

// StallWatch reports a scan that stopped producing rows for longer than
// threshold. It is meant to answer "is the export stuck in the server, or is it
// writing?" from a single log line: a SELECT that stalls (a filesort, a lock
// wait, a network pause) shows up as a gap between rows, not as a failed query.
//
// A gap is only observable when a row finally arrives — rows.Next() blocks for
// the whole stall — so the record says how long the gap was, at the checkpoint
// where the scan resumed. Checkpoints every ScanStallCheckEvery rows bound the
// clock reads; the gaps they see are measured between checkpoints.
type StallWatch struct {
	threshold time.Duration
	every     int
	n         int
	last      time.Time
}

// NewStallWatch returns a watcher for gaps longer than threshold.
func NewStallWatch(threshold time.Duration) *StallWatch {
	return &StallWatch{threshold: threshold, every: ScanStallCheckEvery, last: time.Now()}
}

// Observe records one row and reports, at every checkpoint, the time since the
// previous checkpoint: gap is the measured interval and stalled says whether it
// exceeded the threshold.
func (w *StallWatch) Observe() (gap time.Duration, stalled bool) {
	return w.observe(time.Now())
}

// observe is Observe with the clock injected, so tests can drive the
// checkpoints without sleeping.
func (w *StallWatch) observe(now time.Time) (gap time.Duration, stalled bool) {
	if w == nil || w.threshold <= 0 || w.every <= 0 {
		return 0, false
	}
	w.n++
	if w.n < w.every {
		return 0, false
	}
	w.n = 0
	gap = now.Sub(w.last)
	w.last = now
	return gap, gap > w.threshold
}
