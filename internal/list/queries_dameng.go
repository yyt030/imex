package list

import (
	"strings"

	"github.com/pkg/errors"

	"github.com/imex/internal/config"
)

// dmSystemSchemas is the exclusion list used when the user did not name a
// database. Oracle semantics: hide the schemas the server installs for its own
// bookkeeping, the same idea as Oracle's SYS/SYSTEM/OUTLN exclusion. DM8 ships
// its own set, so the names are measured rather than copied from Oracle -- on
// dm8_20241022_rev244896 (dmsingle) DBA_USERS holds SYS, SYSDBA, SYSAUDITOR and
// SYSSSO, and CTISYS is a schema without a login account (full-text index
// internals: it owns SYSCONTEXTINDEXES, one row in DBA_TABLES). Two Oracle
// shortcuts do not exist on this server: DBA_USERS has no ORACLE_MAINTAINED
// column, and USER_ID does not separate accounts (ordinary users continue the
// same sequence -- DMPROBE follows SYSSSO by 98), so an explicit list is the
// only reliable test.
const dmSystemSchemas = "'SYS','SYSDBA','SYSSSO','SYSAUDITOR','CTISYS'"

// dmScopedToSchema restricts a query to --database, or to every non-system
// schema when the option was not given.
func dmScopedToSchema(column string, cfg *config.ListConfig) []condition {
	if cfg.Database != "" {
		return []condition{{clause: "UPPER(" + column + ") = UPPER(?)", args: []any{cfg.Database}}}
	}
	if cfg.All {
		return nil
	}
	return []condition{{clause: column + " NOT IN (" + dmSystemSchemas + ")"}}
}

func dmPatternCondition(column, pattern string) []condition {
	if pattern == "" {
		return nil
	}
	return []condition{{clause: column + " LIKE ? ESCAPE '\\'", args: []any{pattern}}}
}

func dmWhereClause(conds []condition) (string, []any) {
	if len(conds) == 0 {
		return "", nil
	}
	parts := make([]string, 0, len(conds))
	var args []any
	for _, c := range conds {
		parts = append(parts, c.clause)
		args = append(args, c.args...)
	}
	return " WHERE " + strings.Join(parts, " AND "), args
}

func buildDMQuery(cfg *config.ListConfig) (string, []any, []string, error) {
	switch cfg.Object {
	case ObjectDatabases:
		return buildDMDatabases(cfg)
	case ObjectSchemas:
		return buildDMSchemas(cfg)
	case ObjectTables:
		return buildDMTables(cfg)
	case ObjectViews:
		return buildDMViews(cfg)
	case ObjectColumns:
		return buildDMColumns(cfg)
	case ObjectIndexes:
		return buildDMIndexes(cfg)
	case ObjectTriggers:
		return buildDMTriggers(cfg)
	case ObjectRoutines:
		return buildDMRoutines(cfg)
	default:
		// `imex list` refuses these objects before it dials, with the same words
		// (backend.Refusals for CmdList). This branch is the library-level wall a
		// hand-built config hits, so the two must not drift.
		return "", nil, nil, errors.Errorf("list %s is not supported with --engine dm: this object has not been measured against Dameng Oracle-mode catalog; supported objects are databases, schemas, tables, views, columns, indexes, triggers and routines", cfg.Object)
	}
}

// buildDMDatabases answers the database layer, which on DM is a single row: an
// instance owns exactly one database (V$DATABASE, DAMENG on this server). The
// value is NOT a schema and must not reach the DSN -- the server rejects an
// unknown schema during login ("Error -2103: invalid schema name"), so cmd
// strips --database from the connection for this object and filters V$DATABASE
// in SQL instead. Schemas are a separate object (buildDMSchemas).
func buildDMDatabases(cfg *config.ListConfig) (string, []any, []string, error) {
	var conds []condition
	if cfg.Database != "" {
		conds = append(conds, condition{clause: "UPPER(NAME) = UPPER(?)", args: []any{cfg.Database}})
	}
	conds = append(conds, dmPatternCondition("NAME", cfg.Pattern)...)

	where, args := dmWhereClause(conds)
	query := "SELECT NAME, '', '' FROM V$DATABASE" + where + " ORDER BY NAME"
	return query, args, []string{"database", "charset", "collation"}, nil
}

// buildDMSchemas lists DM's schemas -- the layer --database selects on the
// object lists, where buildDamengDSN writes it into the DSN's schema parameter.
// ALL_OBJECTS with OBJECT_TYPE='SCH' is the direct, Oracle-shaped way to read
// them, and it needs no DBA rights: an ordinary account holding only RESOURCE
// sees the same rows here, while SYS.SYSOBJECTS answers "No select privilege"
// (-5504). Without --all the default exclusion list hides the schemas the
// server installs for itself, the same rule `list tables` applies.
func buildDMSchemas(cfg *config.ListConfig) (string, []any, []string, error) {
	conds := []condition{{clause: "OBJECT_TYPE = 'SCH'"}}
	conds = append(conds, dmScopedToSchema("OBJECT_NAME", cfg)...)
	conds = append(conds, dmPatternCondition("OBJECT_NAME", cfg.Pattern)...)

	where, args := dmWhereClause(conds)
	query := "SELECT DISTINCT OBJECT_NAME FROM ALL_OBJECTS" + where + " ORDER BY OBJECT_NAME"
	return query, args, []string{"schema"}, nil
}

func buildDMTables(cfg *config.ListConfig) (string, []any, []string, error) {
	conds := dmScopedToSchema("OWNER", cfg)
	switch cfg.Type {
	case "", "base":
		// DBA_TABLES already contains only base tables.
	case "view":
		return buildDMTableViews(cfg)
	case "all":
		// Both base tables and views: handled below by union with DBA_VIEWS.
		return buildDMTablesAndViews(cfg)
	default:
		return "", nil, nil, errors.Errorf("invalid --type %q: must be one of base/view/all", cfg.Type)
	}
	conds = append(conds, dmPatternCondition("TABLE_NAME", cfg.Pattern)...)

	where, args := dmWhereClause(conds)
	query := "SELECT OWNER, TABLE_NAME, 'BASE TABLE', '', NVL(NUM_ROWS, 0), 0, 0, ''" +
		" FROM SYS.DBA_TABLES" + where +
		" ORDER BY OWNER, TABLE_NAME"
	return query, args, []string{
		"database", "table", "type", "engine", "rows", "data_length", "index_length", "comment",
	}, nil
}

// buildDMTableViews is the `list tables --type view` shape: a view reported as a
// table (eight columns, TABLE-shaped). `list views` is a different object and
// prints the six-column view shape -- see buildDMViews.
func buildDMTableViews(cfg *config.ListConfig) (string, []any, []string, error) {
	conds := dmScopedToSchema("OWNER", cfg)
	conds = append(conds, dmPatternCondition("VIEW_NAME", cfg.Pattern)...)

	where, args := dmWhereClause(conds)
	query := "SELECT OWNER, VIEW_NAME, 'VIEW', '', 0, 0, 0, ''" +
		" FROM SYS.DBA_VIEWS" + where +
		" ORDER BY OWNER, VIEW_NAME"
	return query, args, []string{
		"database", "table", "type", "engine", "rows", "data_length", "index_length", "comment",
	}, nil
}

func buildDMTablesAndViews(cfg *config.ListConfig) (string, []any, []string, error) {
	// Reuse the base-table and view builders, then concatenate the results.
	// The list runner only needs one statement, so we union them.
	baseConds := dmScopedToSchema("OWNER", cfg)
	baseConds = append(baseConds, dmPatternCondition("TABLE_NAME", cfg.Pattern)...)
	baseWhere, baseArgs := dmWhereClause(baseConds)

	viewConds := dmScopedToSchema("OWNER", cfg)
	viewConds = append(viewConds, dmPatternCondition("VIEW_NAME", cfg.Pattern)...)
	viewWhere, viewArgs := dmWhereClause(viewConds)

	query := "SELECT OWNER, TABLE_NAME, 'BASE TABLE', '', NVL(NUM_ROWS, 0), 0, 0, '' FROM SYS.DBA_TABLES" + baseWhere +
		" UNION ALL " +
		"SELECT OWNER, VIEW_NAME, 'VIEW', '', 0, 0, 0, '' FROM SYS.DBA_VIEWS" + viewWhere +
		" ORDER BY 1, 2"
	return query, append(baseArgs, viewArgs...), []string{
		"database", "table", "type", "engine", "rows", "data_length", "index_length", "comment",
	}, nil
}

func buildDMColumns(cfg *config.ListConfig) (string, []any, []string, error) {
	if cfg.Database == "" {
		return "", nil, nil, errors.New("--database is required for list columns")
	}
	if cfg.Table == "" {
		return "", nil, nil, errors.New("--table is required for list columns")
	}
	conds := []condition{
		{clause: "UPPER(OWNER) = UPPER(?)", args: []any{cfg.Database}},
		{clause: "TABLE_NAME = ?", args: []any{cfg.Table}},
	}
	conds = append(conds, dmPatternCondition("COLUMN_NAME", cfg.Pattern)...)

	where, args := dmWhereClause(conds)
	query := "SELECT OWNER, TABLE_NAME, COLUMN_NAME, COLUMN_ID, DATA_TYPE," +
		" NULLABLE, DATA_DEFAULT, '', '', ''" +
		" FROM SYS.DBA_TAB_COLUMNS" + where +
		" ORDER BY COLUMN_ID"
	return query, args, []string{
		"database", "table", "column", "ordinal", "type", "nullable",
		"default", "key", "extra", "comment",
	}, nil
}

// buildDMIndexes answers `list indexes`. The inner join on DBA_IND_COLUMNS is
// what hides DM's implicit cluster index, and that is deliberate rather than an
// oversight: every table carries one (INDEX_TYPE='CLUSTER', name INDEX335554xx)
// and its DDL has no column list at all -- GET_DDL returns
// `CREATE OR REPLACE CLUSTER INDEX "DMPROBE"."INDEX33555473" ON
// "DMPROBE"."FX_DST"( STORAGE(ON "MAIN", CLUSTERBTR) ;` -- so the catalog has no
// row for it and there is nothing to print in a row-per-column result. MySQL's
// information_schema.STATISTICS has no such row either, so a table with no
// declared index lists no rows on both engines (measured against MySQL 8.0.46:
// a two-column table without keys returns the header alone). A declared CLUSTER
// PRIMARY KEY is a different thing and keeps its column rows (measured: one row,
// UNIQUENESS=UNIQUE, INDEX_TYPE=CLUSTER), which is why no INDEX_TYPE filter is
// applied here -- filtering on the type would hide it. DBA_IND_COLUMNS.TABLE_NAME
// is part of the join key for the same reason it is part of the WHERE clause: an
// index belongs to one table, and the catalog never disagreed (0 mismatches over
// 190 column rows on dm8_20241022_rev244896), so the extra key costs nothing.
func buildDMIndexes(cfg *config.ListConfig) (string, []any, []string, error) {
	if cfg.Database == "" {
		return "", nil, nil, errors.New("--database is required for list indexes")
	}
	if cfg.Table == "" {
		return "", nil, nil, errors.New("--table is required for list indexes")
	}
	conds := []condition{
		{clause: "UPPER(i.OWNER) = UPPER(?)", args: []any{cfg.Database}},
		{clause: "i.TABLE_NAME = ?", args: []any{cfg.Table}},
	}
	conds = append(conds, dmPatternCondition("i.INDEX_NAME", cfg.Pattern)...)

	where, args := dmWhereClause(conds)
	query := "SELECT i.OWNER, i.TABLE_NAME, i.INDEX_NAME," +
		" CASE WHEN i.UNIQUENESS = 'UNIQUE' THEN 0 ELSE 1 END," +
		" ic.COLUMN_POSITION, ic.COLUMN_NAME, 0, i.INDEX_TYPE" +
		" FROM SYS.DBA_INDEXES i" +
		" JOIN SYS.DBA_IND_COLUMNS ic ON i.OWNER = ic.INDEX_OWNER AND i.INDEX_NAME = ic.INDEX_NAME AND i.TABLE_NAME = ic.TABLE_NAME" + where +
		" ORDER BY i.INDEX_NAME, ic.COLUMN_POSITION"
	return query, args, []string{
		"database", "table", "index", "non_unique", "seq", "column", "cardinality", "index_type",
	}, nil
}

// buildDMViews answers `list views` with the six columns MySQL's
// information_schema.VIEWS and the PostgreSQL builder also print. DM's catalog
// carries less, and the gaps are printed as blanks instead of guessed:
//
//   - check_option: DM8 accepts `WITH [CASCADED] CHECK OPTION` (measured: the
//     CREATE VIEW succeeds), but no readable catalog column records it --
//     ALL_VIEWS.TEXT holds the query text only, without the clause. Only
//     DBMS_METADATA.GET_DDL renders it ("... WITH CASCADED CHECK OPTION"), which
//     is what `show vw` prints.
//   - is_updatable: ALL_VIEWS.READ_ONLY is the only candidate, and this build
//     leaves it NULL for every row (244 of 244 on dm8_20241022_rev244896). The
//     CASE keeps the mapping ready for a server that does populate it and prints
//     an empty cell here.
//   - security_type: Oracle-mode views run with the owner's privileges. Measured:
//     a RESOURCE-only account reads DMPROBE.FX_V after GRANT SELECT on the view
//     while the same SELECT on the base table answers -5504, so DEFINER is a
//     measured constant rather than a default, and definer is the owner.
//
// ALL_VIEWS, not DBA_VIEWS: measured on the same server, the DBA_ view answers
// "No select privilege" (-5504) to a RESOURCE-only account while ALL_VIEWS
// returns the rows that account may see. Triggers and routines follow the same
// choice.
func buildDMViews(cfg *config.ListConfig) (string, []any, []string, error) {
	conds := dmScopedToSchema("OWNER", cfg)
	conds = append(conds, dmPatternCondition("VIEW_NAME", cfg.Pattern)...)

	where, args := dmWhereClause(conds)
	query := "SELECT OWNER, VIEW_NAME, ''," +
		" CASE READ_ONLY WHEN 'Y' THEN 'NO' WHEN 'N' THEN 'YES' ELSE '' END," +
		" OWNER, 'DEFINER'" +
		" FROM SYS.ALL_VIEWS" + where +
		" ORDER BY OWNER, VIEW_NAME"
	return query, args, []string{
		"database", "view", "check_option", "is_updatable", "definer", "security_type",
	}, nil
}

// buildDMTriggers answers `list triggers`. TRIGGERING_TYPE carries the row mode
// with the timing ("BEFORE ROW", "AFTER ROW", "INSTEAD OF ROW" -- measured on
// dm8_20241022_rev244896), while MySQL's ACTION_TIMING column holds the timing
// alone, so the prefix is what gets printed. TRIGGERING_EVENT is one event per
// trigger in MySQL and PostgreSQL; DM allows a trigger on several events and
// stores them alphabetically in one field ("DELETE OR INSERT OR UPDATE",
// measured), which is printed as stored. The statement is TRIGGER_BODY, which on
// this server holds the whole trigger source -- header included, e.g. "CREATE OR
// REPLACE TRIGGER FX_TRG BEFORE INSERT ON FX_DST FOR EACH ROW BEGIN NULL; END;"
// -- where MySQL's ACTION_STATEMENT holds the body alone. DM8 has no body-only
// column, so the text is printed as stored instead of cutting it at a keyword
// guessed in SQL. The definer is the owner: ALL_TRIGGERS has no
// owner-of-execution column, and an Oracle-mode trigger runs as its owner.
func buildDMTriggers(cfg *config.ListConfig) (string, []any, []string, error) {
	conds := dmScopedToSchema("OWNER", cfg)
	if cfg.Table != "" {
		conds = append(conds, condition{clause: "TABLE_NAME = ?", args: []any{cfg.Table}})
	}
	conds = append(conds, dmPatternCondition("TRIGGER_NAME", cfg.Pattern)...)

	where, args := dmWhereClause(conds)
	query := "SELECT OWNER, TRIGGER_NAME, TRIGGERING_EVENT, TABLE_NAME," +
		" CASE" +
		" WHEN TRIGGERING_TYPE LIKE 'BEFORE%' THEN 'BEFORE'" +
		" WHEN TRIGGERING_TYPE LIKE 'AFTER%' THEN 'AFTER'" +
		" WHEN TRIGGERING_TYPE LIKE 'INSTEAD OF%' THEN 'INSTEAD OF'" +
		" ELSE TRIGGERING_TYPE END," +
		" TRIGGER_BODY, OWNER" +
		" FROM SYS.ALL_TRIGGERS" + where +
		" ORDER BY OWNER, TRIGGER_NAME"
	return query, args, []string{
		"database", "trigger", "event", "table", "timing", "statement", "definer",
	}, nil
}

// buildDMRoutines answers `list routines`. There is no DBA_ROUTINES on DM8:
// `SELECT COUNT(*) FROM SYS.DBA_ROUTINES` fails with -2106 "invalid table or
// view name". ALL_PROCEDURES is the catalog that has the rows; it also lists
// packages and their subprograms, so the query keeps top-level routines only --
// object rows for procedures and functions have PROCEDURE_NAME NULL, package
// rows carry OBJECT_TYPE='PACKAGE' and each subprogram its own PROCEDURE_NAME
// (measured with a package holding an overloaded procedure).
//
// security is AUTHID mapped to MySQL's DEFINER/INVOKER vocabulary: the catalog
// stores DEFINER or CURRENT_USER. deterministic is already YES/NO. data_access
// has no counterpart in ALL_PROCEDURES (it has no SQL_DATA_ACCESS column at
// all), so it prints empty rather than borrowing PostgreSQL's volatility guess.
func buildDMRoutines(cfg *config.ListConfig) (string, []any, []string, error) {
	conds := []condition{{clause: "PROCEDURE_NAME IS NULL"}}
	switch cfg.Type {
	case "", "all":
		conds = append(conds, condition{clause: "OBJECT_TYPE IN ('PROCEDURE', 'FUNCTION')"})
	case "procedure":
		conds = append(conds, condition{clause: "OBJECT_TYPE = 'PROCEDURE'"})
	case "function":
		conds = append(conds, condition{clause: "OBJECT_TYPE = 'FUNCTION'"})
	default:
		return "", nil, nil, errors.Errorf("invalid --type %q: must be one of procedure/function/all", cfg.Type)
	}
	conds = append(conds, dmScopedToSchema("OWNER", cfg)...)
	conds = append(conds, dmPatternCondition("OBJECT_NAME", cfg.Pattern)...)

	where, args := dmWhereClause(conds)
	query := "SELECT OWNER, OBJECT_NAME, OBJECT_TYPE, OWNER," +
		" CASE WHEN AUTHID = 'DEFINER' THEN 'DEFINER' ELSE 'INVOKER' END," +
		" DETERMINISTIC, ''" +
		" FROM SYS.ALL_PROCEDURES" + where +
		" ORDER BY OWNER, OBJECT_NAME"
	return query, args, []string{
		"database", "routine", "type", "definer", "security", "deterministic", "data_access",
	}, nil
}
