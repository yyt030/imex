package export

import (
	"database/sql"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/pkg/errors"

	"github.com/gomydump/internal/common"
)

// ShardInfo holds information about how to shard a table
type ShardInfo struct {
	Columns     []string      // column names to shard on (supports composite keys)
	ColumnTypes []string      // data types of the columns
	Mins        []interface{} // minimum values for each column
	Maxs        []interface{} // maximum values for each column
	IsNumeric   []bool        // whether each column is numeric
	Nullable    []bool        // whether each column accepts NULL
}

// DetectShardKey detects the best column(s) for sharding
// Priority: PRIMARY KEY > UNIQUE KEY > first indexed column
func DetectShardKey(db *sql.DB, tableName string, preferKey string) (*ShardInfo, error) {
	// If user specified a shard key, use it (single column)
	if preferKey != "" {
		return getShardInfo(db, tableName, []string{preferKey})
	}

	// Try to find PRIMARY KEY (may be composite)
	pkCols, err := findPrimaryKey(db, tableName)
	if err == nil && len(pkCols) > 0 {
		common.Logger.Debug("Using primary key for sharding", "columns", pkCols)
		return getShardInfo(db, tableName, pkCols)
	}

	// Try to find any UNIQUE KEY (may be composite)
	ukCols, err := findUniqueKey(db, tableName)
	if err == nil && len(ukCols) > 0 {
		common.Logger.Debug("Using unique key for sharding", "columns", ukCols)
		return getShardInfo(db, tableName, ukCols)
	}

	return nil, errors.Errorf("no suitable shard key found for table %s", tableName)
}

// findPrimaryKey finds all primary key columns of a table (supports composite keys)
func findPrimaryKey(db *sql.DB, tableName string) ([]string, error) {
	query := common.TagSQL(`
		SELECT COLUMN_NAME 
		FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE 
		WHERE TABLE_SCHEMA = DATABASE() 
			AND TABLE_NAME = ? 
			AND CONSTRAINT_NAME = 'PRIMARY'
		ORDER BY ORDINAL_POSITION
	`)
	rows, err := db.Query(query, tableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var columns []string
	for rows.Next() {
		var colName string
		if err := rows.Scan(&colName); err != nil {
			return nil, err
		}
		columns = append(columns, colName)
	}

	return columns, rows.Err()
}

// findUniqueKey finds the first unique key columns (supports composite keys)
func findUniqueKey(db *sql.DB, tableName string) ([]string, error) {
	query := common.TagSQL(`
		SELECT COLUMN_NAME, CONSTRAINT_NAME
		FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE 
		WHERE TABLE_SCHEMA = DATABASE() 
			AND TABLE_NAME = ? 
			AND CONSTRAINT_NAME != 'PRIMARY'
			AND REFERENCED_TABLE_NAME IS NULL
		ORDER BY CONSTRAINT_NAME, ORDINAL_POSITION
	`)
	rows, err := db.Query(query, tableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var columns []string
	var firstConstraint string
	found := false

	for rows.Next() {
		var colName, constraintName string
		if err := rows.Scan(&colName, &constraintName); err != nil {
			return nil, err
		}

		if !found {
			firstConstraint = constraintName
			found = true
		}

		// Only collect columns from the first unique constraint
		if constraintName == firstConstraint {
			columns = append(columns, colName)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return columns, nil
}

// getShardInfo gets shard information for specific column(s)
func getShardInfo(db *sql.DB, tableName string, columnNames []string) (*ShardInfo, error) {
	if len(columnNames) == 0 {
		return nil, errors.New("no columns provided")
	}

	info := &ShardInfo{
		Columns:     columnNames,
		ColumnTypes: make([]string, len(columnNames)),
		Mins:        make([]interface{}, len(columnNames)),
		Maxs:        make([]interface{}, len(columnNames)),
		IsNumeric:   make([]bool, len(columnNames)),
		Nullable:    make([]bool, len(columnNames)),
	}

	// Get column types
	for i, colName := range columnNames {
		colQuery := common.TagSQL(`
			SELECT DATA_TYPE, IS_NULLABLE
			FROM INFORMATION_SCHEMA.COLUMNS 
			WHERE TABLE_SCHEMA = DATABASE() 
				AND TABLE_NAME = ? 
				AND COLUMN_NAME = ?
		`)
		var colType string
		var isNullable string
		err := db.QueryRow(colQuery, tableName, colName).Scan(&colType, &isNullable)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to get column type for %s", colName)
		}
		info.ColumnTypes[i] = colType
		info.Nullable[i] = strings.EqualFold(isNullable, "YES")

		// Determine if numeric
		numericTypes := map[string]bool{
			"int": true, "integer": true, "tinyint": true, "smallint": true,
			"mediumint": true, "bigint": true, "float": true, "double": true,
			"decimal": true, "numeric": true,
		}
		info.IsNumeric[i] = numericTypes[strings.ToLower(colType)]
	}

	// Get min/max values for all columns
	selectParts := make([]string, len(columnNames)*2)
	for i, colName := range columnNames {
		selectParts[i*2] = fmt.Sprintf("MIN(`%s`)", colName)
		selectParts[i*2+1] = fmt.Sprintf("MAX(`%s`)", colName)
	}
	minMaxQuery := common.TagSQL(fmt.Sprintf("SELECT %s FROM `%s`", strings.Join(selectParts, ", "), tableName))

	// Scan results
	scanArgs := make([]interface{}, len(columnNames)*2)
	for i := range scanArgs {
		scanArgs[i] = new(interface{})
	}

	err := db.QueryRow(minMaxQuery).Scan(scanArgs...)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get min/max values")
	}

	// Extract min/max values
	for i := range columnNames {
		minVal := *(scanArgs[i*2].(*interface{}))
		maxVal := *(scanArgs[i*2+1].(*interface{}))

		if minVal == nil || maxVal == nil {
			return nil, errors.Errorf("table is empty or column %s has no data", columnNames[i])
		}

		info.Mins[i] = minVal
		info.Maxs[i] = maxVal
	}

	return info, nil
}

// shardBoundary is an exact numeric boundary rendered as a SQL numeric literal.
// Keeping it as text avoids routing BIGINT/DECIMAL keys through float64, which
// loses integer precision above 2^53: rounding the last shard's MAX down would
// silently drop the highest rows of the table.
type shardBoundary string

// numericRat parses a MIN()/MAX() value exactly. Values arrive as []byte from
// the text protocol; the remaining cases keep the helper usable from tests.
func numericRat(v interface{}) (*big.Rat, bool) {
	var s string
	switch x := v.(type) {
	case []byte:
		s = string(x)
	case string:
		s = x
	case int:
		s = strconv.Itoa(x)
	case int32:
		s = strconv.FormatInt(int64(x), 10)
	case int64:
		s = strconv.FormatInt(x, 10)
	case uint32:
		s = strconv.FormatUint(uint64(x), 10)
	case uint64:
		s = strconv.FormatUint(x, 10)
	case float32:
		s = strconv.FormatFloat(float64(x), 'g', -1, 64)
	case float64:
		s = strconv.FormatFloat(x, 'g', -1, 64)
	default:
		return nil, false
	}
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok {
		return nil, false
	}
	return r, true
}

// formatShardBoundary renders an exact boundary. Integers print without a
// decimal point; other values print with 30 fractional digits (MySQL DECIMAL's
// maximum scale). Adjacent shards reuse the same rendered string, so rounding
// can shift rows between shards but can never open a gap.
func formatShardBoundary(r *big.Rat) shardBoundary {
	if r.IsInt() {
		return shardBoundary(r.Num().String())
	}
	f := new(big.Float).SetPrec(256).SetRat(r)
	s := strings.TrimRight(f.Text('f', 30), "0")
	s = strings.TrimSuffix(s, ".")
	return shardBoundary(s)
}

// exactValue converts a numeric MIN/MAX value to an exact boundary; non-numeric
// columns are passed through untouched.
func exactValue(v interface{}, numeric bool) interface{} {
	if !numeric {
		return v
	}
	r, ok := numericRat(v)
	if !ok {
		return v
	}
	return formatShardBoundary(r)
}

// GenerateShardRanges generates shard ranges for parallel export.
// For composite keys, we primarily shard on the first column, but the remaining
// columns keep their full range so every tuple is covered exactly once.
func GenerateShardRanges(info *ShardInfo, numShards int) []ShardRange {
	if numShards <= 1 {
		return []ShardRange{{Mins: info.Mins, Maxs: info.Maxs}}
	}

	// For non-numeric first columns (e.g. UUID keys) numeric boundaries cannot
	// be computed, so fall back to a single shard.
	if !info.IsNumeric[0] {
		return []ShardRange{{Mins: info.Mins, Maxs: info.Maxs}}
	}

	min, okMin := numericRat(info.Mins[0])
	max, okMax := numericRat(info.Maxs[0])
	if !okMin || !okMax || max.Cmp(min) <= 0 {
		return []ShardRange{{Mins: info.Mins, Maxs: info.Maxs}}
	}

	// Precompute the numShards+1 boundaries once. The last one is the exact
	// MAX(), so the final shard is inclusive up to the real maximum.
	bounds := make([]*big.Rat, numShards+1)
	bounds[0] = new(big.Rat).Set(min)
	bounds[numShards] = new(big.Rat).Set(max)
	step := new(big.Rat).Quo(new(big.Rat).Sub(max, min), new(big.Rat).SetInt64(int64(numShards)))
	for i := 1; i < numShards; i++ {
		bounds[i] = new(big.Rat).Add(min, new(big.Rat).Mul(step, new(big.Rat).SetInt64(int64(i))))
	}

	ranges := make([]ShardRange, 0, numShards+1)
	for i := 0; i < numShards; i++ {
		mins := make([]interface{}, len(info.Columns))
		maxs := make([]interface{}, len(info.Columns))
		for j := range info.Columns {
			mins[j] = exactValue(info.Mins[j], info.IsNumeric[j])
			maxs[j] = exactValue(info.Maxs[j], info.IsNumeric[j])
		}

		// The first column carries the varying boundary.
		mins[0] = formatShardBoundary(bounds[i])
		maxs[0] = formatShardBoundary(bounds[i+1])

		ranges = append(ranges, ShardRange{
			Mins:   mins,
			Maxs:   maxs,
			IsLast: i == numShards-1,
		})
	}

	// MIN()/MAX() ignore NULLs and no range comparison ever matches a NULL, so
	// rows with a NULL shard key would be dropped from every shard. A
	// single-column key gets one extra shard for exactly those rows; a composite
	// key cannot be expressed that way, so sharding is refused altogether (the
	// caller falls back to a single-threaded export).
	if anyNullable(info.Nullable) {
		if len(info.Columns) > 1 {
			return nil
		}
		ranges = append(ranges, ShardRange{NullOnly: true})
	}

	return ranges
}

// anyNullable reports whether any key column accepts NULL.
func anyNullable(nullable []bool) bool {
	for _, n := range nullable {
		if n {
			return true
		}
	}
	return false
}

// ShardRange represents a range of values for a shard (supports composite keys)
type ShardRange struct {
	Mins     []interface{} // minimum values for each column
	Maxs     []interface{} // maximum values for each column
	IsLast   bool          // whether this is the last shard (inclusive max)
	NullOnly bool          // shard holding only the rows whose key IS NULL
}

// formatValue formats a value for SQL query
func formatValue(v interface{}) string {
	switch val := v.(type) {
	case string:
		return fmt.Sprintf("'%s'", strings.ReplaceAll(val, "'", "''"))
	case []byte:
		return fmt.Sprintf("'%s'", strings.ReplaceAll(string(val), "'", "''"))
	case nil:
		return "NULL"
	default:
		return fmt.Sprintf("%v", val)
	}
}
