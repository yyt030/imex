package export

import (
	"database/sql"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/pkg/errors"

	"github.com/imex/internal/backend"
	"github.com/imex/internal/common"
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

// shardTarget names the table the parallel path inspects: the shard key is
// detected on it, its statistics and key distribution are sampled, and its
// MIN/MAX bound the ranges.
//
// A query may name its table with a schema (`SELECT * FROM otherdb.t`), and that
// schema wins over --database — the connection's own database is only the
// default for a query that says nothing. Everything has to go through the target:
// reading the key space out of the connection's same-named table instead would
// plan shard boundaries that do not cover the rows the query actually returns.
type shardTarget struct {
	schema string // "" means "whatever the connection's current database is"
	table  string

	// be is the dialect the run resolved. It is optional on purpose: every test
	// and library caller that builds a target with just schema/table keeps the
	// historical MySQL behaviour, while exportParallel fills it in so the
	// catalogue reads, identifier quoting and placeholders follow the connected
	// engine.
	be backend.Backend
}

// backend returns the dialect for this target, defaulting to MySQL.
func (t shardTarget) backend() backend.Backend {
	if t.be == nil {
		return backend.Must(backend.EngineMySQL)
	}
	return t.be
}

// source renders the target for a FROM clause, the same way the SHOW CREATE
// lookup does: the schema only when the query named one. The quoting is the
// dialect's (Backend.QuoteIdent), so PostgreSQL gets "schema"."table" and
// MySQL keeps its backticks byte for byte.
func (t shardTarget) source() string {
	be := t.backend()
	if t.schema == "" {
		return be.QuoteIdent(t.table)
	}
	return be.QuoteIdent(t.schema) + "." + be.QuoteIdent(t.table)
}

// display is how the target is named in logs and error messages.
func (t shardTarget) display() string {
	if t.schema == "" {
		return t.table
	}
	return t.schema + "." + t.table
}

// schemaFilter resolves an explicit schema ahead of the connection's database,
// so one predicate serves both the qualified and the unqualified query. It is
// the MySQL spelling; PostgreSQL's equivalent lives in shard_dialect.go.
const schemaFilter = "TABLE_SCHEMA = COALESCE(NULLIF(?, ''), DATABASE())"

// DetectShardKey detects the best column(s) for sharding
// Priority: PRIMARY KEY > UNIQUE KEY > first indexed column
//
// be is the dialect the run resolved: the type vocabulary that decides whether a
// shard key is numeric belongs to the engine, not to this package.
func DetectShardKey(db *sql.DB, be backend.Backend, target shardTarget, preferKey string) (*ShardInfo, error) {
	// A library caller or a test may build the target without the dialect; the
	// backend this function is handed is the same run's, so adopt it. That keeps
	// every catalogue read below on one dialect even when the target was built
	// before --engine was resolved.
	if target.be == nil {
		target.be = be
	}

	// If user specified a shard key, use it (single column)
	if preferKey != "" {
		return getShardInfo(db, be, target, []string{preferKey})
	}

	// Try to find PRIMARY KEY (may be composite)
	pkCols, err := findPrimaryKey(db, target)
	if err == nil && len(pkCols) > 0 {
		common.Logger.Debug("using primary key for sharding", "columns", pkCols)
		return getShardInfo(db, be, target, pkCols)
	}

	// Try to find any UNIQUE KEY (may be composite)
	ukCols, err := findUniqueKey(db, target)
	if err == nil && len(ukCols) > 0 {
		common.Logger.Debug("using unique key for sharding", "columns", ukCols)
		return getShardInfo(db, be, target, ukCols)
	}

	return nil, errors.Errorf("no suitable shard key found for table %s", target.display())
}

// findPrimaryKey finds all primary key columns of a table (supports composite keys)
func findPrimaryKey(db *sql.DB, target shardTarget) ([]string, error) {
	query := shardDialectFor(target.backend()).primaryKey
	common.LogInteractionSQL(query, target.schema, target.table)
	rows, err := db.Query(common.TagSQL(query), target.schema, target.table)
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
func findUniqueKey(db *sql.DB, target shardTarget) ([]string, error) {
	query := shardDialectFor(target.backend()).uniqueKey
	common.LogInteractionSQL(query, target.schema, target.table)
	rows, err := db.Query(common.TagSQL(query), target.schema, target.table)
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
func getShardInfo(db *sql.DB, be backend.Backend, target shardTarget, columnNames []string) (*ShardInfo, error) {
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
	dialect := shardDialectFor(target.backend())
	for i, colName := range columnNames {
		colQuery := dialect.columnType
		common.LogInteractionSQL(colQuery, target.schema, target.table, colName)
		var colType string
		var isNullable string
		err := db.QueryRow(common.TagSQL(colQuery), target.schema, target.table, colName).Scan(&colType, &isNullable)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to get column type for %s", colName)
		}
		info.ColumnTypes[i] = colType
		info.Nullable[i] = strings.EqualFold(isNullable, "YES")

		// Determine if numeric. The type vocabulary is the dialect's, so the answer
		// comes from Backend.MetadataKind rather than from a list of words copied
		// into this package: MySQL's information_schema reports the base type
		// (`double` for a REAL column, `decimal` for DEC/NUMERIC/FIXED, `bigint`
		// for SERIAL), so the wider table behind MetadataKind cannot change an
		// answer this query can return — measured on MariaDB 10.6.23. PostgreSQL's
		// information_schema reports `integer`/`bigint`/`numeric`, which
		// ClassifyPostgresType maps onto the same tool vocabulary.
		info.IsNumeric[i] = be.MetadataKind(colType) == common.KindNumber
	}

	// Get min/max values for all columns. Identifier quoting is the dialect's:
	// PostgreSQL reads MIN("id"), MySQL keeps MIN(`id`) byte for byte.
	selectParts := make([]string, len(columnNames)*2)
	for i, colName := range columnNames {
		quoted := be.QuoteIdent(colName)
		selectParts[i*2] = "MIN(" + quoted + ")"
		selectParts[i*2+1] = "MAX(" + quoted + ")"
	}
	minMaxSQL := fmt.Sprintf("SELECT %s FROM %s", strings.Join(selectParts, ", "), target.source())
	common.LogInteractionSQL(minMaxSQL)
	minMaxQuery := common.TagSQL(minMaxSQL)

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
