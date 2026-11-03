package list

import (
	"strconv"
	"strings"

	"github.com/pkg/errors"

	"github.com/imex/internal/config"
)

// PostgreSQL namespace scoping for `imex list`.
//
// PostgreSQL has a two-level namespace (database + schema) where MySQL has one
// (database == schema). With --engine pg, the -D/--database flag is consumed by
// the connection string (it names the DSN database), so it cannot also mean
// "schema". The rule implemented here:
//
//   - list databases: -D filters pg_database.datname (cluster-wide catalog).
//   - every other object:
//     * -D given → scope to current_database() = $1 AND schema = current_schema().
//       The connection sees exactly one database; current_schema() is the first
//       schema in the role's search_path (measured: 'public' by default).
//     * -D empty, no --all → all non-system schemas of the connected database.
//       System schemas excluded: pg_catalog, information_schema, pg_toast
//       (measured on PG 14; these are the three PostgreSQL reserves).
//     * --all → no schema exclusion at all.
//
// The output column named "database" carries the schema name for PG (since a
// connection sees exactly one database, the schema is the object's container).
// This is documented per-object below.

// pgSystemSchemas is the exclusion list for PostgreSQL when --database is not
// given and --all is false. Measured on PG 14: pg_catalog, information_schema,
// and pg_toast are the three schemas PostgreSQL reserves for its own bookkeeping.
// Note: information_schema and pg_catalog do appear in pg_namespace, but most
// information_schema views (tables, columns, etc.) filter to the connected
// database automatically, so the NOT IN is mainly a guard for pg_namespace-
// based queries (tables, views, triggers, routines).
const pgSystemSchemas = "'pg_catalog','information_schema','pg_toast'"

// pgBuilder accumulates a PostgreSQL query with numbered placeholders ($1, $2,
// ...) and the matching args slice. The counter ensures placeholders are always
// sequential with no gaps, which the PostgreSQL wire protocol requires.
type pgBuilder struct {
	args   []any
	nextID int
}

// addArg records a value and returns its $n placeholder text. Placeholders are
// 1-based ($1, $2, ...) to match the PostgreSQL wire protocol.
func (b *pgBuilder) addArg(v any) string {
	b.nextID++
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(b.nextID)
}

// pgScopedToNamespace adds schema/database scoping for pg_catalog-based queries
// (tables, views, triggers, routines). The schema column is the pg_namespace.nspname
// alias passed in.
//
// With -D given: current_database() = $1 AND schema_col = current_schema().
// With -D empty, no --all: schema_col NOT IN (system schemas).
// With --all: no filter.
func pgScopedToNamespace(schemaCol string, cfg *config.ListConfig, b *pgBuilder) []string {
	if cfg.Database != "" {
		// Scope to the connected database and its current schema.
		return []string{
			"current_database() = " + b.addArg(cfg.Database),
			schemaCol + " = current_schema()",
		}
	}
	if cfg.All {
		return nil
	}
	return []string{schemaCol + " NOT IN (" + pgSystemSchemas + ")"}
}

// pgScopedToInfoSchema adds database/schema scoping to an information_schema
// query (columns). These views have table_catalog and table_schema columns
// instead of pg_namespace, and the tableAlias parameter is the alias used in the
// FROM clause ("c" for `information_schema.columns c`).
//
// --database is required for this object, exactly as it is for MySQL, and with
// --engine pg it names the connected database. So the scope is always one
// catalog plus the schema that catalog resolves to for this connection.
func pgScopedToInfoSchema(tableAlias string, cfg *config.ListConfig, b *pgBuilder) []string {
	return []string{
		tableAlias + ".table_catalog = " + b.addArg(cfg.Database),
		tableAlias + ".table_schema = current_schema()",
	}
}

func pgPatternCondition(column, pattern string, b *pgBuilder) []string {
	if pattern == "" {
		return nil
	}
	// ESCAPE '\' tells PostgreSQL that the backslash is the LIKE escape character,
	// matching the MySQL builder's ESCAPE '\\' behaviour (MySQL's default
	// NO_BACKSLASH_ESCAPES off means \\ in a literal is one backslash; PG's
	// standard_conforming_strings on means '\' in a literal is one backslash).
	return []string{column + " LIKE " + b.addArg(pattern) + ` ESCAPE '\'`}
}

func pgWhereClause(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// buildPGQuery is the PostgreSQL counterpart of BuildQuery. It returns the
// statement, its bound arguments and the output column names for one list
// object. The statement reads pg_catalog and information_schema and never
// interpolates user input.
func buildPGQuery(cfg *config.ListConfig) (query string, args []any, columns []string, err error) {
	switch cfg.Object {
	case ObjectDatabases:
		return buildPGDatabases(cfg)
	case ObjectTables:
		return buildPGTables(cfg)
	case ObjectViews:
		return buildPGViews(cfg)
	case ObjectColumns:
		return buildPGColumns(cfg)
	case ObjectIndexes:
		return buildPGIndexes(cfg)
	case ObjectTriggers:
		return buildPGTriggers(cfg)
	case ObjectRoutines:
		return buildPGRoutines(cfg)
	case ObjectEvents:
		return "", nil, nil, errors.New("PostgreSQL has no event scheduler; events are a MySQL/MariaDB extension")
	default:
		return "", nil, nil, errors.Errorf("unknown object %q: expected one of %s", cfg.Object, strings.Join(objectOrder, ", "))
	}
}

func buildPGDatabases(cfg *config.ListConfig) (string, []any, []string, error) {
	// pg_database lists all databases in the cluster. Unlike MySQL's
	// information_schema.SCHEMATA (which lists schemas within one database),
	// PostgreSQL's pg_database is cluster-wide, so --database here filters by
	// datname rather than schema name.
	b := &pgBuilder{}
	var conds []string
	if cfg.Database != "" {
		conds = append(conds, "datname = "+b.addArg(cfg.Database))
	}
	conds = append(conds, pgPatternCondition("datname", cfg.Pattern, b)...)

	// The "database" column carries the database name here (not schema), because
	// pg_database is cluster-wide and each row IS a database.
	query := "SELECT datname, pg_encoding_to_char(encoding), datcollate" +
		" FROM pg_catalog.pg_database" + pgWhereClause(conds) +
		" ORDER BY datname"
	return query, b.args, []string{"database", "charset", "collation"}, nil
}

func buildPGTables(cfg *config.ListConfig) (string, []any, []string, error) {
	// pg_class holds tables, views, indexes, sequences, etc. relkind distinguishes:
	// 'r' = ordinary table, 'p' = partitioned table, 'v' = view, 'f' = foreign table.
	// We map 'r' and 'p' to "BASE TABLE" to match MySQL's TABLE_TYPE output.
	b := &pgBuilder{}
	conds := pgScopedToNamespace("n.nspname", cfg, b)
	switch cfg.Type {
	case "", "base":
		conds = append(conds, "c.relkind IN ('r','p')")
	case "view":
		conds = append(conds, "c.relkind = 'v'")
	case "all":
		// Both base tables and views.
	default:
		return "", nil, nil, errors.Errorf("invalid --type %q: must be one of base/view/all", cfg.Type)
	}
	conds = append(conds, pgPatternCondition("c.relname", cfg.Pattern, b)...)

	// TABLE_ROWS → reltuples (estimated row count from ANALYZE; clamped to 0 for
	//   tables that have never been analyzed, where reltuples can be -1)
	// DATA_LENGTH → pg_relation_size (main fork bytes, excludes indexes/TOAST)
	// INDEX_LENGTH → pg_indexes_size (total bytes of all indexes on the table)
	// ENGINE → pg_am.amname (access method: heap, BRIN, GiST, gin, etc.)
	// COMMENT → shobj_description (object comment stored in pg_description)
	// The "database" column carries the schema name (n.nspname), because a PG
	// connection sees exactly one database and the schema is the object's container.
	query := "SELECT n.nspname, c.relname," +
		" CASE c.relkind WHEN 'r' THEN 'BASE TABLE' WHEN 'p' THEN 'BASE TABLE' WHEN 'v' THEN 'VIEW' ELSE c.relkind::text END," +
		" COALESCE(am.amname, '')," +
		" GREATEST(c.reltuples::bigint, 0)," +
		" pg_relation_size(c.oid)," +
		" pg_indexes_size(c.oid)," +
		" COALESCE(shobj_description(c.oid, 'pg_class'), '')" +
		" FROM pg_catalog.pg_class c" +
		" JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace" +
		" LEFT JOIN pg_catalog.pg_am am ON am.oid = c.relam" +
		pgWhereClause(conds) +
		" ORDER BY n.nspname, c.relname"
	return query, b.args, []string{
		"database", "table", "type", "engine", "rows", "data_length", "index_length", "comment",
	}, nil
}

func buildPGViews(cfg *config.ListConfig) (string, []any, []string, error) {
	// PostgreSQL's information_schema.views has check_option and is_updatable,
	// but not definer or security_type. The definer is the view owner (from
	// pg_class.relowner), and security_type is always INVOKER because PG views
	// don't have a SECURITY DEFINER mode like MySQL does.
	b := &pgBuilder{}
	conds := pgScopedToNamespace("n.nspname", cfg, b)
	conds = append(conds, "c.relkind = 'v'")
	conds = append(conds, pgPatternCondition("c.relname", cfg.Pattern, b)...)

	// The "database" column carries the schema name (n.nspname).
	query := "SELECT n.nspname, c.relname, v.check_option, v.is_updatable," +
		" pg_catalog.pg_get_userbyid(c.relowner)," +
		" 'INVOKER'" +
		" FROM pg_catalog.pg_class c" +
		" JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace" +
		" JOIN information_schema.views v ON v.table_schema = n.nspname AND v.table_name = c.relname" +
		pgWhereClause(conds) +
		" ORDER BY n.nspname, c.relname"
	return query, b.args, []string{
		"database", "view", "check_option", "is_updatable", "definer", "security_type",
	}, nil
}

func buildPGColumns(cfg *config.ListConfig) (string, []any, []string, error) {
	if cfg.Database == "" {
		return "", nil, nil, errors.New("--database is required for list columns")
	}
	if cfg.Table == "" {
		return "", nil, nil, errors.New("--table is required for list columns")
	}
	b := &pgBuilder{}
	conds := pgScopedToInfoSchema("c", cfg, b)
	conds = append(conds, "c.table_name = "+b.addArg(cfg.Table))
	conds = append(conds, pgPatternCondition("c.column_name", cfg.Pattern, b)...)

	// column_type → data_type (PG doesn't have MySQL's COLUMN_TYPE with display
	//   widths like "int(11)"; data_type is the honest PG spelling)
	// nullable → is_nullable (YES/NO, same as MySQL)
	// default → column_default (PG's expression text, not MySQL's value text)
	// key → PRI if in a primary key, UNI if in a unique constraint, else ''
	//   (MySQL also has MUL for non-unique indexes; PG has no honest single-column
	//   equivalent without joining pg_index, so we return '' for those)
	// extra → auto_increment for identity columns, VIRTUAL GENERATED for generated
	//   columns (PG has two mechanisms: attgenerated and attidentity)
	// comment → pg_description.objsubid (column-level comment)
	// The "database" column carries the schema name (c.table_schema).
	query := "SELECT c.table_schema, c.table_name, c.column_name, c.ordinal_position, c.data_type," +
		" c.is_nullable," +
		" c.column_default," +
		" CASE WHEN pk.column_name IS NOT NULL THEN 'PRI' WHEN uq.column_name IS NOT NULL THEN 'UNI' ELSE '' END," +
		" CASE WHEN c.is_identity = 'YES' THEN 'auto_increment' WHEN c.is_generated = 'ALWAYS' THEN 'VIRTUAL GENERATED' ELSE '' END," +
		" COALESCE(pgd.description, '')" +
		" FROM information_schema.columns c" +
		" LEFT JOIN pg_catalog.pg_statio_all_tables st ON st.schemaname = c.table_schema AND st.relname = c.table_name" +
		" LEFT JOIN pg_catalog.pg_description pgd ON pgd.objoid = st.relid AND pgd.objsubid = c.ordinal_position" +
		" LEFT JOIN (" +
		"   SELECT kcu.table_schema, kcu.table_name, kcu.column_name" +
		"   FROM information_schema.table_constraints tc" +
		"   JOIN information_schema.key_column_usage kcu" +
		"     ON kcu.constraint_name = tc.constraint_name AND kcu.table_schema = tc.table_schema" +
		"   WHERE tc.constraint_type = 'PRIMARY KEY'" +
		" ) pk ON pk.table_schema = c.table_schema AND pk.table_name = c.table_name AND pk.column_name = c.column_name" +
		" LEFT JOIN (" +
		"   SELECT kcu.table_schema, kcu.table_name, kcu.column_name" +
		"   FROM information_schema.table_constraints tc" +
		"   JOIN information_schema.key_column_usage kcu" +
		"     ON kcu.constraint_name = tc.constraint_name AND kcu.table_schema = tc.table_schema" +
		"   WHERE tc.constraint_type = 'UNIQUE'" +
		" ) uq ON uq.table_schema = c.table_schema AND uq.table_name = c.table_name AND uq.column_name = c.column_name" +
		pgWhereClause(conds) +
		" ORDER BY c.ordinal_position"
	return query, b.args, []string{
		"database", "table", "column", "ordinal", "type", "nullable",
		"default", "key", "extra", "comment",
	}, nil
}

func buildPGIndexes(cfg *config.ListConfig) (string, []any, []string, error) {
	if cfg.Database == "" {
		return "", nil, nil, errors.New("--database is required for list indexes")
	}
	if cfg.Table == "" {
		return "", nil, nil, errors.New("--table is required for list indexes")
	}
	b := &pgBuilder{}
	conds := pgScopedToNamespace("n.nspname", cfg, b)
	conds = append(conds, "c.relname = "+b.addArg(cfg.Table))
	conds = append(conds, pgPatternCondition("i.relname", cfg.Pattern, b)...)

	// NON_UNIQUE → 0 if indisunique, else 1 (same 0/1 convention as MySQL)
	// SEQ → ordinal position of the column in the index (1-based, same as MySQL's
	//   SEQ_IN_INDEX)
	// COLUMN → attribute name from pg_attribute
	// CARDINALITY → indnkeyatts (number of key columns in the index). MySQL's
	//   CARDINALITY is an estimate of distinct values, which PG does not store in
	//   the catalog; indnkeyatts is the closest honest structural property.
	// INDEX_TYPE → pg_am.amname (btree, hash, gist, gin, brin, etc.)
	// The "database" column carries the schema name (n.nspname).
	query := "SELECT n.nspname, c.relname, i.relname," +
		" CASE WHEN ix.indisunique THEN 0 ELSE 1 END," +
		" uk.ord," +
		" a.attname," +
		" ix.indnkeyatts," +
		" am.amname" +
		" FROM pg_catalog.pg_index ix" +
		" JOIN pg_catalog.pg_class c ON c.oid = ix.indrelid" +
		" JOIN pg_catalog.pg_class i ON i.oid = ix.indexrelid" +
		" JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace" +
		" JOIN pg_catalog.pg_am am ON am.oid = i.relam" +
		" CROSS JOIN LATERAL unnest(ix.indkey) WITH ORDINALITY AS uk(attnum, ord)" +
		" JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid AND a.attnum = uk.attnum" +
		pgWhereClause(conds) +
		" ORDER BY i.relname, uk.ord"
	return query, b.args, []string{
		"database", "table", "index", "non_unique", "seq", "column", "cardinality", "index_type",
	}, nil
}

func buildPGTriggers(cfg *config.ListConfig) (string, []any, []string, error) {
	// pg_trigger holds the trigger metadata. tgtype is a bitmask (measured on PG 14):
	// bit 0 (value 1):  1 = BEFORE, 0 = AFTER/INSTEAD
	// bit 1 (value 2):  1 = FOR EACH ROW, 0 = FOR EACH STATEMENT
	// bit 2 (value 4):  INSERT
	// bit 3 (value 8):  DELETE
	// bit 4 (value 16): UPDATE
	// bit 5 (value 32): TRUNCATE
	// bit 6 (value 64): INSTEAD OF (requires bit 0 = 0)
	b := &pgBuilder{}
	conds := pgScopedToNamespace("n.nspname", cfg, b)
	if cfg.Table != "" {
		conds = append(conds, "c.relname = "+b.addArg(cfg.Table))
	}
	conds = append(conds, pgPatternCondition("trg.tgname", cfg.Pattern, b)...)
	conds = append(conds, "NOT trg.tgisinternal")

	// timing → BEFORE/AFTER/INSTEAD OF (from tgtype bitmask)
	// event → INSERT/DELETE/UPDATE/TRUNCATE (from tgtype bitmask; a trigger can
	//   fire on multiple events, but tgtype stores one event per trigger in PG)
	// table → pg_class.relname of the subject table
	// statement → pg_get_triggerdef (the full CREATE TRIGGER statement text)
	// definer → pg_proc.proowner (the trigger function's owner, since pg_trigger
	//   has no owner column of its own)
	// The "database" column carries the schema name (n.nspname).
	query := "SELECT n.nspname, trg.tgname," +
		" CASE" +
		"   WHEN (trg.tgtype & 64) != 0 THEN 'INSTEAD OF'" +
		"   WHEN (trg.tgtype & 1) != 0 THEN 'BEFORE'" +
		"   ELSE 'AFTER'" +
		" END," +
		" CASE" +
		"   WHEN (trg.tgtype & 4) != 0 THEN 'INSERT'" +
		"   WHEN (trg.tgtype & 8) != 0 THEN 'DELETE'" +
		"   WHEN (trg.tgtype & 16) != 0 THEN 'UPDATE'" +
		"   WHEN (trg.tgtype & 32) != 0 THEN 'TRUNCATE'" +
		"   ELSE 'UNKNOWN'" +
		" END," +
		" c.relname," +
		" pg_get_triggerdef(trg.oid)," +
		" pg_catalog.pg_get_userbyid(p.proowner)" +
		" FROM pg_catalog.pg_trigger trg" +
		" JOIN pg_catalog.pg_class c ON c.oid = trg.tgrelid" +
		" JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace" +
		" JOIN pg_catalog.pg_proc p ON p.oid = trg.tgfoid" +
		pgWhereClause(conds) +
		" ORDER BY n.nspname, trg.tgname"
	return query, b.args, []string{
		"database", "trigger", "event", "table", "timing", "statement", "definer",
	}, nil
}

func buildPGRoutines(cfg *config.ListConfig) (string, []any, []string, error) {
	// pg_proc holds functions and procedures. prokind (PG 11+) distinguishes:
	// 'f' = function, 'p' = procedure, 'a' = aggregate, 'w' = window function.
	b := &pgBuilder{}
	conds := pgScopedToNamespace("n.nspname", cfg, b)
	switch cfg.Type {
	case "", "all":
		conds = append(conds, "p.prokind IN ('f','p')")
	case "procedure":
		conds = append(conds, "p.prokind = 'p'")
	case "function":
		conds = append(conds, "p.prokind = 'f'")
	default:
		return "", nil, nil, errors.Errorf("invalid --type %q: must be one of procedure/function/all", cfg.Type)
	}
	conds = append(conds, pgPatternCondition("p.proname", cfg.Pattern, b)...)

	// type → FUNCTION/PROCEDURE (from prokind; aggregate/window excluded above)
	// definer → proowner (the function owner role)
	// security → prosecdef (SECURITY DEFINER vs INVOKER)
	// deterministic → PG has no IS_DETERMINISTIC attribute. proisstrict (RETURNS
	//   NULL ON NULL INPUT) is the closest measurable property but is NOT the same
	//   thing; we return it with a comment so the difference is visible.
	// data_access → provolatile: i=immutable (NO SQL), s=stable (CONTAINS SQL),
	//   v=volatile (CONTAINS SQL). MySQL's SQL_DATA_ACCESS has four values; PG's
	//   volatile is the closest mapping for the two "contains" cases.
	// The "database" column carries the schema name (n.nspname).
	query := "SELECT n.nspname, p.proname," +
		" CASE p.prokind WHEN 'f' THEN 'FUNCTION' WHEN 'p' THEN 'PROCEDURE' ELSE p.prokind::text END," +
		" pg_catalog.pg_get_userbyid(p.proowner)," +
		" CASE WHEN p.prosecdef THEN 'DEFINER' ELSE 'INVOKER' END," +
		" CASE WHEN p.proisstrict THEN 'YES' ELSE 'NO' END," +
		" CASE" +
		"   WHEN p.provolatile = 'i' THEN 'NO SQL'" +
		"   ELSE 'CONTAINS SQL'" +
		" END" +
		" FROM pg_catalog.pg_proc p" +
		" JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace" +
		pgWhereClause(conds) +
		" ORDER BY n.nspname, p.proname"
	return query, b.args, []string{
		"database", "routine", "type", "definer", "security", "deterministic", "data_access",
	}, nil
}
