package export

import "github.com/imex/internal/backend"

// shardDialect holds the four catalogue statements the parallel export path
// uses to measure a table's key space. They are the only MySQL-only SQL left in
// this package: identifier quoting and placeholder spelling come from
// Backend.QuoteIdent and Backend.PlaceholderList, and the data reads (MIN/MAX
// and the boundary probes) are built from those two. Adding an engine therefore
// means adding its four statements, not a second copy of the planner.
//
// Every statement binds its arguments in the order the MySQL path has always
// used: (schema, table), plus (column) for the type lookup. An empty schema
// means "the connection's own database", resolved server-side so one predicate
// serves both a qualified and an unqualified query.
type shardDialect struct {
	primaryKey string // -> (column_name), ordered by key position
	uniqueKey  string // -> (column_name, constraint_name), ordered by constraint
	columnType string // -> (data_type, is_nullable)
	tableStats string // -> (rows, avg_row_len)
}

// shardDialectFor picks the statements for one run. PostgreSQL and the GaussDB
// spike that inherits its refusals and wire protocol both read the PostgreSQL
// catalogue; everything else keeps the MySQL statements — those are the bytes
// every existing MySQL run sends (contract face ⑤), and the Dameng engine
// refuses --threads > 1 at the CLI before this path is reached.
func shardDialectFor(be backend.Backend) shardDialect {
	switch be.Engine() {
	case backend.EnginePG, backend.EngineGauss:
		return pgShardDialect
	}
	return mysqlShardDialect
}

// mysqlShardDialect is the four information_schema reads the MySQL path has
// always sent, kept verbatim — including the whitespace and the ? placeholders —
// so a MySQL run's general_log is unchanged by this increment. schemaFilter is
// declared in shard.go because the contract tests pin it by name.
var mysqlShardDialect = shardDialect{
	primaryKey: `
		SELECT COLUMN_NAME 
		FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE 
		WHERE ` + schemaFilter + ` 
			AND TABLE_NAME = ? 
			AND CONSTRAINT_NAME = 'PRIMARY'
		ORDER BY ORDINAL_POSITION
	`,
	uniqueKey: `
		SELECT COLUMN_NAME, CONSTRAINT_NAME
		FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE 
		WHERE ` + schemaFilter + ` 
			AND TABLE_NAME = ? 
			AND CONSTRAINT_NAME != 'PRIMARY'
			AND REFERENCED_TABLE_NAME IS NULL
		ORDER BY CONSTRAINT_NAME, ORDINAL_POSITION
	`,
	columnType: `
			SELECT DATA_TYPE, IS_NULLABLE
			FROM INFORMATION_SCHEMA.COLUMNS 
			WHERE ` + schemaFilter + ` 
				AND TABLE_NAME = ? 
				AND COLUMN_NAME = ?
		`,
	tableStats: `
		SELECT TABLE_ROWS, AVG_ROW_LENGTH
		FROM INFORMATION_SCHEMA.TABLES
		WHERE ` + schemaFilter + `
			AND TABLE_NAME = ?
	`,
}

// pgShardDialect reads the same four answers from PostgreSQL's own catalogue.
// information_schema is only usable for the column type: it has no row estimate
// or average row length, its key_column_usage names a primary key after the
// table (not the literal 'PRIMARY'), and it has no REFERENCED_TABLE_NAME column
// to exclude foreign keys. pg_constraint/pg_class/pg_attribute answer all three
// of those directly, and conkey WITH ORDINALITY preserves key column order the
// way ORDINAL_POSITION does on MySQL.
//
// The schema predicate mirrors MySQL's COALESCE(NULLIF(?, ”), DATABASE()):
// PostgreSQL has no DATABASE() and --database is already spent as the DSN
// database, so an explicit schema wins and an empty one falls back to
// current_schema() (the role's search_path), exactly the two-level-namespace
// rule the rest of this backend follows. $1 is always the schema, $2 the table,
// $3 the column.
const pgSchemaPredicate = "n.nspname = COALESCE(NULLIF($1, ''), current_schema()::text)"

var pgShardDialect = shardDialect{
	primaryKey: `
		SELECT a.attname
		FROM pg_catalog.pg_constraint con
		JOIN pg_catalog.pg_class c ON c.oid = con.conrelid
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		JOIN LATERAL unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord) ON true
		JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
		WHERE con.contype = 'p'
			AND c.relname = $2
			AND ` + pgSchemaPredicate + `
		ORDER BY k.ord
	`,
	uniqueKey: `
		SELECT a.attname, con.conname
		FROM pg_catalog.pg_constraint con
		JOIN pg_catalog.pg_class c ON c.oid = con.conrelid
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		JOIN LATERAL unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord) ON true
		JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
		WHERE con.contype = 'u'
			AND c.relname = $2
			AND ` + pgSchemaPredicate + `
		ORDER BY con.conname, k.ord
	`,
	columnType: `
		SELECT data_type, is_nullable
		FROM information_schema.columns
		WHERE table_schema = COALESCE(NULLIF($1, ''), current_schema()::text)
			AND table_name = $2
			AND column_name = $3
	`,
	// reltuples/relpages are the planner's own statistics. GREATEST(...,0)
	// turns the -1 PostgreSQL writes for a never-analysed table into 0, which
	// the caller already treats as "no estimate, split by value width". The
	// average row length is derived from the page count so samplingWorthwhile's
	// 256 MiB threshold keeps working without an extra round-trip.
	tableStats: `
		SELECT GREATEST(c.reltuples, 0)::bigint,
		       CASE WHEN c.relpages > 0
		            THEN (c.relpages * 8192 / NULLIF(GREATEST(c.reltuples, 1), 0))::bigint
		            ELSE 0 END
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relname = $2
			AND ` + pgSchemaPredicate + `
	`,
}
