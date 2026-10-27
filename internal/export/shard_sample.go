package export

import (
	"database/sql"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/pkg/errors"

	"github.com/imex/internal/common"
)

// Parallel export splits the key space into more ranges than there are worker
// slots, and the workers claim those ranges in order.
//
// Two things motivate the oversubscription. Sampling answers "what is the value
// of the rowsPerRange-th row", so any error in the row estimate lands entirely
// in the range after the last boundary; with two ranges per worker that residual
// is spread over two claims instead of one. And a range is the unit a worker
// picks up, so a distribution that defeats sampling still gets drained as
// several small ranges rather than one stuck worker plus idle ones.
//
// The cap is a memory cap, not a speed cap: every range owns a pipe whose
// look-ahead comes from a fixed budget divided across the ranges
// (shardStreamQueueBlocks), so the range count is what divides that budget.
const (
	shardRangeOversample = 2
	maxShardRanges       = 64

	// samplingMinScanBytes is the dump size from which reading the key index
	// once more counts as free. Below it an integer key that is already dense
	// keeps the split it has, because paying a pass to balance a two-second
	// export is not a trade the user asked for.
	samplingMinScanBytes = int64(256) << 20
)

// denseSpanLimit says how much value space one row may occupy before an
// equal-width split stops being provably decent: if the whole span holds at most
// 3/2 rows per unit of space, no equal-width range can be more than 1.5x fuller
// than an ideal one, empty stretches and duplicate clusters included.
var denseSpanLimit = big.NewRat(3, 2)

// shardRangeCount returns how many ranges a run with threads workers reads.
func shardRangeCount(threads int) int {
	if threads < 1 {
		threads = 1
	}
	n := threads * shardRangeOversample
	if n > maxShardRanges {
		n = maxShardRanges
	}
	return n
}

// planShardRanges produces the ranges a parallel export run reads, in ascending
// key order, because the stream writer concatenates them in that order and that
// is what makes a merged dump equivalent to a single-threaded one.
//
// Splitting MIN()/MAX() into equal widths is only as good as the value domain. A
// BIGINT key holding 40,000 rows at 1..40,000 and 160,000 rows at
// 9,960,000..10,000,000 gets four ranges, two of which read nothing and one of
// which reads everything; a CHAR or VARCHAR key cannot be split numerically at
// all and always collapsed to a single range. Where the value domain cannot be
// trusted, boundaries come from the stored key distribution instead: ask the
// index what the rowsPerRange-th, 2*rowsPerRange-th ... value is.
//
// Every fallback here is the behaviour this function had before sampling
// existed, so sampling can speed a run up and cannot change what it writes for
// any table that used to get good boundaries.
func planShardRanges(db *sql.DB, tableName string, info *ShardInfo, numRanges int) []ShardRange {
	if numRanges < 2 {
		return GenerateShardRanges(info, numRanges)
	}

	rows, avgRowLen, ok := tableStats(db, tableName)
	if !ok {
		common.Logger.Debug("no row estimate for the table, splitting the key range by value width",
			"table", tableName)
		return GenerateShardRanges(info, numRanges)
	}

	if !samplingWorthwhile(info, rows, avgRowLen) {
		return GenerateShardRanges(info, numRanges)
	}

	ranges, err := sampleShardRanges(db, tableName, info, numRanges, rows)
	if err != nil {
		common.Logger.Warn("shard boundary sampling unavailable, splitting the key range by value width",
			"table", tableName, "shard_key", info.Columns[0], "reason", err.Error())
		return GenerateShardRanges(info, numRanges)
	}
	return ranges
}

// samplingWorthwhile reports whether reading the real key distribution can beat
// an equal-width split of the value domain.
func samplingWorthwhile(info *ShardInfo, rows, avgRowLen int64) bool {
	// A non-numeric key has no equal-width split: sampling is the only way to
	// shard it at all, so its cost is worth paying whatever the table size is.
	if !info.IsNumeric[0] {
		return true
	}
	// Continuous types (DECIMAL, FLOAT, DOUBLE) can be dense in value space and
	// still cluster in a fraction of it, so the density shortcut below does not
	// apply to them.
	if !isIntegerKeyType(info.ColumnTypes[0]) {
		return true
	}
	// A sparse value domain is exactly what equal-width ranges cut wrong.
	if !denseEnoughForEqualWidth(info, rows) {
		return true
	}
	// Dense integer key: every range is provably within 1.5x of the ideal row
	// count already. Sampling still pays for itself once one index pass is a
	// rounding error against the dump it is balancing.
	return avgRowLen > 0 && avgRowLen*rows >= samplingMinScanBytes
}

// isIntegerKeyType reports whether DATA_TYPE is a fixed-point integer type, the
// only family whose value span reads as a density.
func isIntegerKeyType(dataType string) bool {
	switch strings.ToLower(dataType) {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint":
		return true
	}
	return false
}

// denseEnoughForEqualWidth bounds the worst-case imbalance of an equal-width
// split without counting anything: it compares the key's value span against the
// row estimate.
func denseEnoughForEqualWidth(info *ShardInfo, rows int64) bool {
	if rows <= 0 {
		return false
	}
	min, okMin := numericRat(info.Mins[0])
	max, okMax := numericRat(info.Maxs[0])
	if !okMin || !okMax {
		return false
	}
	span := new(big.Rat).Sub(max, min)
	span.Add(span, new(big.Rat).SetInt64(1))
	limit := new(big.Rat).SetInt64(rows).Mul(new(big.Rat).SetInt64(rows), denseSpanLimit)
	return span.Cmp(limit) <= 0
}

// tableStats reads the engine's row estimate and average row length for one
// table. Both are statistics rather than counted rows — InnoDB and OceanBase
// derive them from index samples and they are good to a few percent — which is
// enough to place boundaries, because a boundary is always a value the probe
// actually found, never an interpolated one.
func tableStats(db *sql.DB, tableName string) (rows, avgRowLen int64, ok bool) {
	query := `
		SELECT TABLE_ROWS, AVG_ROW_LENGTH
		FROM INFORMATION_SCHEMA.TABLES
		WHERE TABLE_SCHEMA = DATABASE()
			AND TABLE_NAME = ?
	`
	common.LogInteractionSQL(query, tableName)
	if err := db.QueryRow(common.TagSQL(query), tableName).Scan(&rows, &avgRowLen); err != nil {
		return 0, 0, false
	}
	if rows <= 0 {
		return 0, 0, false
	}
	return rows, avgRowLen, true
}

// probeArg binds a boundary value back to the server. Integer keys go as
// integers so the optimiser sees a numeric range on a numeric column; a value
// that does not fit signed 64-bit (BIGINT UNSIGNED) goes as its exact text.
func probeArg(v interface{}, integer bool) interface{} {
	var text string
	switch x := v.(type) {
	case []byte:
		text = string(x)
	case string:
		text = x
	case shardBoundary:
		text = string(x)
	default:
		return v
	}
	if !integer {
		return text
	}
	if n, err := strconv.ParseInt(text, 10, 64); err == nil {
		return n
	}
	return text
}

// sampleShardRanges places the internal boundaries of a run by reading the key
// distribution.
//
// Each probe reads at most rowsPerRange index entries, so numRanges probes
// together cost one pass over the key index however many ranges there are.
// Because every probe seeks from the boundary it just found, an error in the row
// estimate moves all later boundaries together instead of opening a hole: the
// ranges stay a contiguous, duplicate-free cover of the key space, which is what
// the merged dump's correctness rests on.
func sampleShardRanges(db *sql.DB, tableName string, info *ShardInfo, numRanges int, rows int64) ([]ShardRange, error) {
	col := info.Columns[0]
	quoted := fmt.Sprintf("`%s`", col)
	integer := isIntegerKeyType(info.ColumnTypes[0])

	rowsPerRange := rows / int64(numRanges)
	if rowsPerRange < 1 {
		return nil, errors.Errorf("estimated %d rows cannot be split into %d ranges", rows, numRanges)
	}

	bounds := []interface{}{exactValue(info.Mins[0], info.IsNumeric[0])}

	for i := 1; i < numRanges; i++ {
		// The offset is one less than the step: the boundary sought is the
		// rowsPerRange-th row after the previous one.
		probeSQL := fmt.Sprintf(
			"SELECT %s FROM `%s` WHERE %s > ? ORDER BY %s LIMIT 1 OFFSET %d",
			quoted, tableName, quoted, quoted, rowsPerRange-1)
		probeBound := probeArg(bounds[len(bounds)-1], integer)
		common.LogInteractionSQL(probeSQL, probeBound)
		query := common.TagSQL(probeSQL)

		var raw interface{}
		if err := db.QueryRow(query, probeBound).Scan(&raw); err != nil {
			if err == sql.ErrNoRows {
				// The estimate was too high, or everything left duplicates of the
				// last boundary. Fewer ranges is not a failure: whatever has been
				// collected still covers the key space.
				break
			}
			return nil, errors.Wrapf(err, "failed to sample boundary %d of shard key %s", i, col)
		}
		if raw == nil {
			return nil, errors.Errorf("sampling shard key %s returned NULL", col)
		}

		bound := exactValue(raw, info.IsNumeric[0])
		if sameShardBound(bound, bounds[len(bounds)-1]) {
			// All rows carrying this value land in one range wherever the
			// boundary goes, so a second copy would only buy an empty range.
			continue
		}
		bounds = append(bounds, bound)
	}

	if len(bounds) < 2 {
		return nil, errors.Errorf("shard key %s yielded only its lowest value", col)
	}
	if common.Logger != nil {
		common.Logger.Debug("shard boundaries sampled from the key distribution",
			"table", tableName, "shard_key", col, "ranges", len(bounds),
			"rows_per_range", rowsPerRange)
	}
	return rangesFromBounds(info, bounds), nil
}

// sameShardBound compares two boundaries by their SQL rendering, the only form
// numeric and text keys share.
func sameShardBound(a, b interface{}) bool {
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

// rangesFromBounds turns ascending boundary values into ranges covering
// [bounds[0], MAX()] exactly once, plus the IS NULL range a nullable
// single-column key always needs.
func rangesFromBounds(info *ShardInfo, bounds []interface{}) []ShardRange {
	n := len(bounds)
	ranges := make([]ShardRange, 0, n+1)
	for i := 0; i < n; i++ {
		mins := make([]interface{}, len(info.Columns))
		maxs := make([]interface{}, len(info.Columns))
		for j := range info.Columns {
			mins[j] = exactValue(info.Mins[j], info.IsNumeric[j])
			maxs[j] = exactValue(info.Maxs[j], info.IsNumeric[j])
		}

		// The first column carries the varying boundary; the rest of a composite
		// key keeps its full range so every tuple is covered exactly once.
		mins[0] = bounds[i]
		if i == n-1 {
			maxs[0] = exactValue(info.Maxs[0], info.IsNumeric[0])
		} else {
			maxs[0] = bounds[i+1]
		}

		ranges = append(ranges, ShardRange{Mins: mins, Maxs: maxs, IsLast: i == n-1})
	}

	if anyNullable(info.Nullable) {
		if len(info.Columns) > 1 {
			return nil
		}
		ranges = append(ranges, ShardRange{NullOnly: true})
	}
	return ranges
}
