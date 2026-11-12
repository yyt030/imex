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
	case ObjectColumns:
		return buildDMColumns(cfg)
	case ObjectIndexes:
		return buildDMIndexes(cfg)
	default:
		// `imex list` refuses these objects before it dials, with the same words
		// (backend.Refusals for CmdList). This branch is the library-level wall a
		// hand-built config hits, so the two must not drift.
		return "", nil, nil, errors.Errorf("list %s is not supported with --engine dm: this object has not been measured against Dameng Oracle-mode catalog; supported objects are databases, schemas, tables, columns and indexes", cfg.Object)
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
		return buildDMViews(cfg)
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

func buildDMViews(cfg *config.ListConfig) (string, []any, []string, error) {
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
		" JOIN SYS.DBA_IND_COLUMNS ic ON i.OWNER = ic.INDEX_OWNER AND i.INDEX_NAME = ic.INDEX_NAME" + where +
		" ORDER BY i.INDEX_NAME, ic.COLUMN_POSITION"
	return query, args, []string{
		"database", "table", "index", "non_unique", "seq", "column", "cardinality", "index_type",
	}, nil
}
