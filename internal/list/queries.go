package list

import (
	"strings"

	"github.com/pkg/errors"

	"github.com/imex/internal/config"
)

// Object names accepted by `imex list`. They are also the subcommand names, so
// the command tree and this list cannot drift apart without a test failing.
const (
	ObjectDatabases = "databases"
	ObjectTables    = "tables"
	ObjectViews     = "views"
	ObjectColumns   = "columns"
	ObjectIndexes   = "indexes"
	ObjectTriggers  = "triggers"
	ObjectRoutines  = "routines"
	ObjectEvents    = "events"
)

var objectOrder = []string{
	ObjectDatabases, ObjectTables, ObjectViews, ObjectColumns,
	ObjectIndexes, ObjectTriggers, ObjectRoutines, ObjectEvents,
}

// Objects returns the accepted object names in help/error order.
func Objects() []string {
	out := make([]string, len(objectOrder))
	copy(out, objectOrder)
	return out
}

// systemSchemas is the exclusion list used when the user did not name a
// database. It is a literal, not a parameter: these are the four schemas MySQL,
// MariaDB and OceanBase reserve for the server's own bookkeeping.
const systemSchemas = "'information_schema','mysql','performance_schema','sys'"

// likeSuffix is appended to a name column when --pattern was given. The escape
// character is written as a two-backslash SQL literal, which the server reads
// as one backslash unless NO_BACKSLASH_ESCAPES is set; --pattern therefore uses
// the usual `\%` / `\_` spelling for a literal wildcard.
const likeSuffix = ` LIKE ? ESCAPE '\\'`

type condition struct {
	clause string
	args   []any
}

// scopedToSchema restricts a query to --database, or to every non-system schema
// when the option was not given. An explicit --database wins over --all: asking
// for `mysql` must still list `mysql`.
func scopedToSchema(column string, cfg *config.ListConfig) []condition {
	if cfg.Database != "" {
		return []condition{{clause: column + " = ?", args: []any{cfg.Database}}}
	}
	if cfg.All {
		return nil
	}
	return []condition{{clause: column + " NOT IN (" + systemSchemas + ")"}}
}

func patternCondition(column, pattern string) []condition {
	if pattern == "" {
		return nil
	}
	return []condition{{clause: column + likeSuffix, args: []any{pattern}}}
}

func whereClause(conds []condition) (string, []any) {
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

// BuildQuery returns the statement, its bound arguments and the output column
// names for one list object. The statement always reads information_schema and
// never interpolates user input: --database, --table and --pattern travel as
// placeholders.
func BuildQuery(cfg *config.ListConfig) (string, []any, []string, error) {
	switch cfg.Object {
	case ObjectDatabases:
		return buildDatabases(cfg)
	case ObjectTables:
		return buildTables(cfg)
	case ObjectViews:
		return buildViews(cfg)
	case ObjectColumns:
		return buildColumns(cfg)
	case ObjectIndexes:
		return buildIndexes(cfg)
	case ObjectTriggers:
		return buildTriggers(cfg)
	case ObjectRoutines:
		return buildRoutines(cfg)
	case ObjectEvents:
		return buildEvents(cfg)
	default:
		return "", nil, nil, errors.Errorf("unknown object %q: expected one of %s", cfg.Object, strings.Join(objectOrder, ", "))
	}
}

func buildDatabases(cfg *config.ListConfig) (string, []any, []string, error) {
	// Unlike the object lists, `databases` does not hide the system schemas by
	// default: `SHOW DATABASES` shows them, and an operator asking what exists
	// should see the whole server. --database narrows it to one row.
	var conds []condition
	if cfg.Database != "" {
		conds = append(conds, condition{clause: "SCHEMA_NAME = ?", args: []any{cfg.Database}})
	}
	conds = append(conds, patternCondition("SCHEMA_NAME", cfg.Pattern)...)

	where, args := whereClause(conds)
	query := "SELECT SCHEMA_NAME, DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME" +
		" FROM information_schema.SCHEMATA" + where +
		" ORDER BY SCHEMA_NAME"
	return query, args, []string{"database", "charset", "collation"}, nil
}

func buildTables(cfg *config.ListConfig) (string, []any, []string, error) {
	conds := scopedToSchema("TABLE_SCHEMA", cfg)
	switch cfg.Type {
	case "", "base":
		conds = append(conds, condition{clause: "TABLE_TYPE = 'BASE TABLE'"})
	case "view":
		conds = append(conds, condition{clause: "TABLE_TYPE = 'VIEW'"})
	case "all":
		// Both base tables and views.
	default:
		return "", nil, nil, errors.Errorf("invalid --type %q: must be one of base/view/all", cfg.Type)
	}
	conds = append(conds, patternCondition("TABLE_NAME", cfg.Pattern)...)

	where, args := whereClause(conds)
	query := "SELECT TABLE_SCHEMA, TABLE_NAME, TABLE_TYPE, COALESCE(ENGINE, '')," +
		" COALESCE(TABLE_ROWS, 0), COALESCE(DATA_LENGTH, 0)," +
		" COALESCE(INDEX_LENGTH, 0), COALESCE(TABLE_COMMENT, '')" +
		" FROM information_schema.TABLES" + where +
		" ORDER BY TABLE_SCHEMA, TABLE_NAME"
	return query, args, []string{
		"database", "table", "type", "engine", "rows", "data_length", "index_length", "comment",
	}, nil
}

func buildViews(cfg *config.ListConfig) (string, []any, []string, error) {
	conds := scopedToSchema("TABLE_SCHEMA", cfg)
	conds = append(conds, patternCondition("TABLE_NAME", cfg.Pattern)...)

	where, args := whereClause(conds)
	query := "SELECT TABLE_SCHEMA, TABLE_NAME, CHECK_OPTION, IS_UPDATABLE, DEFINER, SECURITY_TYPE" +
		" FROM information_schema.VIEWS" + where +
		" ORDER BY TABLE_SCHEMA, TABLE_NAME"
	return query, args, []string{
		"database", "view", "check_option", "is_updatable", "definer", "security_type",
	}, nil
}

func buildColumns(cfg *config.ListConfig) (string, []any, []string, error) {
	if cfg.Database == "" {
		return "", nil, nil, errors.New("--database is required for list columns")
	}
	if cfg.Table == "" {
		return "", nil, nil, errors.New("--table is required for list columns")
	}
	conds := []condition{
		{clause: "TABLE_SCHEMA = ?", args: []any{cfg.Database}},
		{clause: "TABLE_NAME = ?", args: []any{cfg.Table}},
	}
	conds = append(conds, patternCondition("COLUMN_NAME", cfg.Pattern)...)

	where, args := whereClause(conds)
	query := "SELECT TABLE_SCHEMA, TABLE_NAME, COLUMN_NAME, ORDINAL_POSITION, COLUMN_TYPE," +
		" IS_NULLABLE, COLUMN_DEFAULT, COLUMN_KEY, EXTRA, COLUMN_COMMENT" +
		" FROM information_schema.COLUMNS" + where +
		" ORDER BY ORDINAL_POSITION"
	return query, args, []string{
		"database", "table", "column", "ordinal", "type", "nullable",
		"default", "key", "extra", "comment",
	}, nil
}

func buildIndexes(cfg *config.ListConfig) (string, []any, []string, error) {
	if cfg.Database == "" {
		return "", nil, nil, errors.New("--database is required for list indexes")
	}
	if cfg.Table == "" {
		return "", nil, nil, errors.New("--table is required for list indexes")
	}
	conds := []condition{
		{clause: "TABLE_SCHEMA = ?", args: []any{cfg.Database}},
		{clause: "TABLE_NAME = ?", args: []any{cfg.Table}},
	}
	conds = append(conds, patternCondition("INDEX_NAME", cfg.Pattern)...)

	where, args := whereClause(conds)
	query := "SELECT TABLE_SCHEMA, TABLE_NAME, INDEX_NAME, NON_UNIQUE, SEQ_IN_INDEX," +
		" COLUMN_NAME, CARDINALITY, INDEX_TYPE" +
		" FROM information_schema.STATISTICS" + where +
		" ORDER BY INDEX_NAME, SEQ_IN_INDEX"
	return query, args, []string{
		"database", "table", "index", "non_unique", "seq", "column", "cardinality", "index_type",
	}, nil
}

func buildTriggers(cfg *config.ListConfig) (string, []any, []string, error) {
	conds := scopedToSchema("TRIGGER_SCHEMA", cfg)
	if cfg.Table != "" {
		conds = append(conds, condition{clause: "EVENT_OBJECT_TABLE = ?", args: []any{cfg.Table}})
	}
	conds = append(conds, patternCondition("TRIGGER_NAME", cfg.Pattern)...)

	where, args := whereClause(conds)
	query := "SELECT TRIGGER_SCHEMA, TRIGGER_NAME, EVENT_MANIPULATION, EVENT_OBJECT_TABLE," +
		" ACTION_TIMING, ACTION_STATEMENT, DEFINER" +
		" FROM information_schema.TRIGGERS" + where +
		" ORDER BY TRIGGER_SCHEMA, TRIGGER_NAME"
	return query, args, []string{
		"database", "trigger", "event", "table", "timing", "statement", "definer",
	}, nil
}

func buildRoutines(cfg *config.ListConfig) (string, []any, []string, error) {
	conds := scopedToSchema("ROUTINE_SCHEMA", cfg)
	switch cfg.Type {
	case "", "all":
		// Procedures and functions.
	case "procedure":
		conds = append(conds, condition{clause: "ROUTINE_TYPE = 'PROCEDURE'"})
	case "function":
		conds = append(conds, condition{clause: "ROUTINE_TYPE = 'FUNCTION'"})
	default:
		return "", nil, nil, errors.Errorf("invalid --type %q: must be one of procedure/function/all", cfg.Type)
	}
	conds = append(conds, patternCondition("ROUTINE_NAME", cfg.Pattern)...)

	where, args := whereClause(conds)
	query := "SELECT ROUTINE_SCHEMA, ROUTINE_NAME, ROUTINE_TYPE, DEFINER, SECURITY_TYPE," +
		" IS_DETERMINISTIC, SQL_DATA_ACCESS" +
		" FROM information_schema.ROUTINES" + where +
		" ORDER BY ROUTINE_SCHEMA, ROUTINE_NAME"
	return query, args, []string{
		"database", "routine", "type", "definer", "security", "deterministic", "data_access",
	}, nil
}

func buildEvents(cfg *config.ListConfig) (string, []any, []string, error) {
	conds := scopedToSchema("EVENT_SCHEMA", cfg)
	conds = append(conds, patternCondition("EVENT_NAME", cfg.Pattern)...)

	where, args := whereClause(conds)
	query := "SELECT EVENT_SCHEMA, EVENT_NAME, STATUS, EXECUTE_AT, INTERVAL_VALUE," +
		" INTERVAL_FIELD, DEFINER, LAST_EXECUTED" +
		" FROM information_schema.EVENTS" + where +
		" ORDER BY EVENT_SCHEMA, EVENT_NAME"
	return query, args, []string{
		"database", "event", "status", "execute_at", "interval_value",
		"interval_field", "definer", "last_executed",
	}, nil
}
