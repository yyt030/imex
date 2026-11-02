package common

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Seconds renders a wall-clock duration the way every timing record in the
// command log is meant to read: plain seconds with three decimal places, fixed
// unit, so "0.842s" and "12.350s" sort and grep alike instead of the mixed
// "45s" / "1.234s" / "2.5ms" that time.Duration.String() produces when the value
// crosses unit boundaries. Used for both the periodic progress "elapsed" field
// and the completion "duration" field, per the operator request that the final
// total time read in seconds with three decimals.
func Seconds(d time.Duration) string {
	return fmt.Sprintf("%.3fs", d.Seconds())
}

// InfoEnabled reports whether the global logger will emit an info record.
// Callers that must assemble extra attributes (a worker id and shard index next
// to the statement, for instance) guard on this so they do no work — and log no
// line — unless --log-level is at info or below.
func InfoEnabled() bool {
	return Logger.Enabled(context.Background(), slog.LevelInfo)
}

// LogInteractionSQL records, at the default info level, one statement the tool
// sends to the server as part of the export/import *conversation* — schema and
// metadata lookups (SHOW CREATE, INFORMATION_SCHEMA probes, MIN/MAX bounds,
// shard boundary probes), the per-shard SELECT the parallel export runs,
// structural DDL a SQL dump carries, the consistent-snapshot statements, and the
// metadata query a list subcommand runs.
//
// These are few in number: one record per statement, so the count grows with
// tables, shards and batches, never with rows. Measured against a 200,000-row SQL
// dump import the whole trace is 4 records; a 4-thread, 8-shard export of the
// same table is 32, because each shard connection opens its own snapshot. The
// team asked to read the statements without raising the log level first, because
// "what did the tool actually send" is the first thing to check when an import or
// export misbehaves on a real server. --log-level warn, or error, is the way to
// silence them again.
//
// Bulk data statements are deliberately NOT routed here. A parallel import's
// batched INSERTs run into the millions, and the operator asked specifically to
// keep those out. Callers handle data-path diagnostics through FailingSQL on
// error instead.
//
// The statement is rendered through SQLInteractionPreview: identifiers and the
// statement text are echoed as written, but any bound boundary/arg values stay
// redacted to type/length tags, and the text is clipped to
// MaxSQLInteractionPreview so a caller-supplied multi-kilobyte query repeated
// across shard SELECTs cannot turn the default log into the biggest file of the
// run. That is the same disclosure rule that governs failing-SQL echoes, and the
// reason the trace is safe to carry at the level that is on by default and safe
// to write to a --messages file.
func LogInteractionSQL(query string, args ...interface{}) {
	if !Logger.Enabled(context.Background(), slog.LevelInfo) {
		return
	}
	Logger.Info("db interaction", "sql", SQLInteractionPreview(query, args))
}
