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

// DebugEnabled reports whether the global logger will emit a debug record.
// Callers that must assemble extra attributes (a worker id and shard index next
// to the statement, for instance) guard on this so they do no work — and log no
// line — unless --log-level debug is actually in effect.
func DebugEnabled() bool {
	return Logger.Enabled(context.Background(), slog.LevelDebug)
}

// LogInteractionSQL records, at debug level only, one statement the tool sends
// to the server as part of the export/import *conversation* — schema and
// metadata lookups (SHOW CREATE, INFORMATION_SCHEMA probes, MIN/MAX bounds,
// shard boundary probes), the per-shard SELECT the parallel export runs,
// structural DDL a SQL dump carries, and the session-setup statements. These
// are few in number, so seeing the exact SQL that crossed the wire is worth a
// record each under --log-level debug; a default (info) run stays byte-for-byte
// as quiet as before.
//
// Bulk data statements are deliberately NOT routed here. A parallel import's
// batched INSERTs run into the millions, and the operator asked specifically to
// keep those out. Callers handle data-path diagnostics through FailingSQL on
// error instead.
//
// The statement is rendered through SQLPreview, so any bound boundary/arg values
// stay redacted to type/length tags — the same rule that governs failing-SQL
// echoes, and the reason debug output is safe to write to a --messages file.
func LogInteractionSQL(query string, args ...interface{}) {
	if !Logger.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	Logger.Debug("db interaction", "sql", SQLPreview(query, args))
}
