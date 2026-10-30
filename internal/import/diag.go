package _import

import "time"

const (
	// slowBatchThreshold is how long one batched INSERT may take before the run
	// says so. Five seconds is several times a healthy round-trip even for a
	// multi-megabyte batch on a cloud link, so a record here means the server
	// (or the network in front of it) is the bottleneck for that batch.
	slowBatchThreshold = 5 * time.Second

	// slowCommitThreshold is how long one COMMIT may take before the run says
	// so. A commit is a round-trip plus a redo flush, so it is normally far
	// shorter than an INSERT; when it is not, the durable-log side is the
	// bottleneck and no amount of --threads helps.
	slowCommitThreshold = 5 * time.Second
)

// batchWireBytes estimates the payload one batch retains, using the same
// accounting the replay buffer uses (statement text plus the string/byte
// payloads). It exists only to give a slow-batch record a size; the name avoids
// the local batchSize variable in the import paths.
func batchWireBytes(query string, values []interface{}) int {
	return batchSize(query, values)
}

// rowsPerSecond is rows/d, 0 when d is not positive: a segment that measured as
// zero still gets a rate field rather than a division by zero.
func rowsPerSecond(rows int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(rows) / d.Seconds()
}
