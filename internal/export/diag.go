package export

import "time"

// scanStallThreshold is how long a scan may go without handing over a row
// before the run says so. Ten seconds is far above a healthy gap on a local or
// LAN link and well below the point where an operator starts guessing: a
// filesort, a lock wait or a stalled socket all show up here first, because
// rows.Next() simply blocks while they happen.
const scanStallThreshold = 10 * time.Second

// rowsPerSecond is rows/d, 0 when d is not positive: a segment so short it
// measured as zero still gets a rate field rather than a division by zero.
func rowsPerSecond(rows int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(rows) / d.Seconds()
}
