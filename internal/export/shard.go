package export

import (
	"database/sql"
	"fmt"
	"github.com/pkg/errors"
	"strings"

	"github.com/gomydump/internal/common"
)

// ShardInfo holds information about how to shard a table
type ShardInfo struct {
	Columns     []string      // column names to shard on (supports composite keys)
	ColumnTypes []string      // data types of the columns
	Mins        []interface{} // minimum values for each column
	Maxs        []interface{} // maximum values for each column
	IsNumeric   []bool        // whether each column is numeric
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
	}

	// Get column types
	for i, colName := range columnNames {
		colQuery := common.TagSQL(`
			SELECT DATA_TYPE 
			FROM INFORMATION_SCHEMA.COLUMNS 
			WHERE TABLE_SCHEMA = DATABASE() 
				AND TABLE_NAME = ? 
				AND COLUMN_NAME = ?
		`)
		var colType string
		err := db.QueryRow(colQuery, tableName, colName).Scan(&colType)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to get column type for %s", colName)
		}
		info.ColumnTypes[i] = colType

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

// GenerateShardRanges generates shard ranges for parallel export
// For composite keys, we primarily shard on the first column,
// but use row constructor comparison for precise range boundaries
func GenerateShardRanges(info *ShardInfo, numShards int) []ShardRange {
	if numShards <= 1 {
		return []ShardRange{{Mins: info.Mins, Maxs: info.Maxs}}
	}

	// For composite keys, we shard primarily on the first column
	// This works well with B-tree indexes (leftmost prefix principle)
	firstColMin := toFloat64(info.Mins[0])
	firstColMax := toFloat64(info.Maxs[0])

	// Check if first column is numeric
	if !info.IsNumeric[0] {
		// For non-numeric first column, fall back to single shard
		return []ShardRange{{Mins: info.Mins, Maxs: info.Maxs}}
	}

	rangeSize := (firstColMax - firstColMin) / float64(numShards)
	ranges := make([]ShardRange, 0, numShards)

	for i := 0; i < numShards; i++ {
		rangeMin := firstColMin + float64(i)*rangeSize
		rangeMax := firstColMin + float64(i+1)*rangeSize

		// Build min/max arrays for this shard
		mins := make([]interface{}, len(info.Columns))
		maxs := make([]interface{}, len(info.Columns))

		// First column uses calculated range
		mins[0] = rangeMin
		if i == numShards-1 {
			maxs[0] = firstColMax // Last shard uses actual max
		} else {
			maxs[0] = rangeMax
		}

		// For subsequent columns, use actual min/max
		// This ensures we cover all combinations within the first column's range
		for j := 1; j < len(info.Columns); j++ {
			mins[j] = info.Mins[j]
			maxs[j] = info.Maxs[j]
		}

		ranges = append(ranges, ShardRange{
			Mins:   mins,
			Maxs:   maxs,
			IsLast: i == numShards-1,
		})
	}

	return ranges
}

// ShardRange represents a range of values for a shard (supports composite keys)
type ShardRange struct {
	Mins   []interface{} // minimum values for each column
	Maxs   []interface{} // maximum values for each column
	IsLast bool          // whether this is the last shard (inclusive max)
}

// toFloat64 converts interface{} to float64
func toFloat64(v interface{}) float64 {
	switch val := v.(type) {
	case int:
		return float64(val)
	case int8:
		return float64(val)
	case int16:
		return float64(val)
	case int32:
		return float64(val)
	case int64:
		return float64(val)
	case uint:
		return float64(val)
	case uint8:
		return float64(val)
	case uint16:
		return float64(val)
	case uint32:
		return float64(val)
	case uint64:
		return float64(val)
	case float32:
		return float64(val)
	case float64:
		return val
	case []byte:
		// Try to parse as number
		var f float64
		fmt.Sscanf(string(val), "%f", &f)
		return f
	default:
		return 0
	}
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
