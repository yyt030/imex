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
// catalogue; Dameng reads the Oracle-compatible ALL_* views; everything else
// keeps the MySQL statements — those are the bytes every existing MySQL run
// sends (contract face ⑤).
func shardDialectFor(be backend.Backend) shardDialect {
	switch be.Engine() {
	case backend.EnginePG, backend.EngineGauss:
		return pgShardDialect
	case backend.EngineDM:
		return dmShardDialect
	}
	return mysqlShardDialect
}

// shardRowValueComparison reports whether the engine reads a row-value
// comparison — `(a, b) >= (1, 2)` — which is how a composite shard key bounds a
// range. MySQL (5.7/8.0, MariaDB, OceanBase) and the PostgreSQL family do, and
// that is the SQL every existing run sends.
//
// Dameng does not: measured on DM8 2026-11-14, `WHERE (A, B) >= (15, 1)` is
// rejected with Error -2007 "nearby [15] has error: Syntax error", in both the
// WHERE and the ON position, integers and decimals alike. Only the equality form
// of a row comparison is in its grammar. A composite key therefore has to be
// bounded by the lexicographic expansion of the same order (see
// lexicographicComparison in sql_parser.go), which the measured DM8 accepts and
// which covers the key space exactly the same way.
func shardRowValueComparison(be backend.Backend) bool {
	if be != nil && be.Engine() == backend.EngineDM {
		return false
	}
	return true
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

// dmSchemaPredicate is Dameng's spelling of "the schema this run named, or the
// one the session is in". CURRENT_SCHEMA is the schema the DSN's schema= opened
// the session with (internal/backend/dameng.go pins it from --database), so an
// unqualified `SELECT * FROM DMPAR` plans its ranges against the same table the
// query reads — the rule shardTarget documents. UPPER() wraps both sides because
// Oracle-mode DM folds unquoted identifiers to upper case, which is also how the
// catalogue reads in internal/backend/dameng.go and internal/list/queries_dameng.go
// already spell their owner predicate; a query that names the table in lower case
// therefore still finds its own key, while a quoted mixed-case name is exactly as
// unreachable here as it is for the rest of this backend.
const dmSchemaPredicate = "UPPER(C.OWNER) = UPPER(COALESCE(NULLIF(?, ''), SYS_CONTEXT('USERENV','CURRENT_SCHEMA')))"

// dmShardDialect reads the four shard-planner answers from Dameng's
// Oracle-compatible ALL_* views. ALL_* rather than the SYS.DBA_* views the import
// path uses because ALL_* answers for whatever the connected account can see: a
// RESOURCE-only account is the common case on a real DM8 and the catalogue read
// must not be the reason a run falls back to one thread (measured on DM8 with
// IMEXIT, which holds RESOURCE only). Bind order is MySQL's — (schema, table),
// plus (column) for the type lookup — and the placeholders are the driver's `?`.
var dmShardDialect = shardDialect{
	// CONSTRAINT_TYPE is 'P' for a primary key and 'U' for a unique key, and
	// POSITION carries the column's place inside a composite key, which is what
	// keeps the tuple comparison in buildShardCondition in key order.
	primaryKey: `
		SELECT C.COLUMN_NAME
		FROM ALL_CONS_COLUMNS C
		JOIN ALL_CONSTRAINTS K
			ON K.OWNER = C.OWNER AND K.TABLE_NAME = C.TABLE_NAME
			AND K.CONSTRAINT_NAME = C.CONSTRAINT_NAME
		WHERE ` + dmSchemaPredicate + `
			AND C.TABLE_NAME = ?
			AND K.CONSTRAINT_TYPE = 'P'
		ORDER BY C.POSITION
	`,
	uniqueKey: `
		SELECT C.COLUMN_NAME, C.CONSTRAINT_NAME
		FROM ALL_CONS_COLUMNS C
		JOIN ALL_CONSTRAINTS K
			ON K.OWNER = C.OWNER AND K.TABLE_NAME = C.TABLE_NAME
			AND K.CONSTRAINT_NAME = C.CONSTRAINT_NAME
		WHERE ` + dmSchemaPredicate + `
			AND C.TABLE_NAME = ?
			AND K.CONSTRAINT_TYPE = 'U'
		ORDER BY C.CONSTRAINT_NAME, C.POSITION
	`,
	// NULLABLE is 'Y'/'N' on Dameng, not the 'YES'/'NO' the MySQL and PostgreSQL
	// spellings of this read return, and getShardInfo compares against "YES".
	// Left unmapped every shard key column would look NOT NULL, the IS NULL shard
	// would never be emitted, and rows whose key is NULL would silently vanish
	// from the dump — so the answer is folded into YES/NO in the statement and
	// this package stays engine-blind.
	columnType: `
		SELECT DATA_TYPE, CASE NULLABLE WHEN 'Y' THEN 'YES' ELSE 'NO' END
		FROM ALL_TAB_COLUMNS
		WHERE UPPER(OWNER) = UPPER(COALESCE(NULLIF(?, ''), SYS_CONTEXT('USERENV','CURRENT_SCHEMA')))
			AND TABLE_NAME = ?
			AND UPPER(COLUMN_NAME) = UPPER(?)
	`,
	// NUM_ROWS is a statistic, not a count: it is NULL until the DBA has run
	// SP_TAB_STAT_INIT, and COALESCE turns that into the 0 the caller already
	// treats as "no estimate, split by value width". AVG_ROW_LEN is present in
	// DM8's ALL_TABLES but stays -1 after statistics gathering on the measured
	// build, so the average row length is derived from the page count exactly the
	// way the PostgreSQL read derives it from relpages — which is what keeps
	// samplingWorthwhile's 256 MiB threshold comparing dump size, not a constant.
	tableStats: `
		SELECT COALESCE(NUM_ROWS, 0),
		       CASE WHEN COALESCE(NUM_ROWS, 0) > 0 AND COALESCE(BLOCKS, 0) > 0
		            THEN CAST(COALESCE(BLOCKS, 0) * SF_GET_PAGE_SIZE() / COALESCE(NUM_ROWS, 0) AS BIGINT)
		            ELSE 0 END
		FROM ALL_TABLES
		WHERE UPPER(OWNER) = UPPER(COALESCE(NULLIF(?, ''), SYS_CONTEXT('USERENV','CURRENT_SCHEMA')))
			AND TABLE_NAME = ?
	`,
}
