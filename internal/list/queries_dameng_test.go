package list

import (
	"strings"
	"testing"

	"github.com/imex/internal/config"
)

// dmListCfg mirrors listCfg but is explicit for Dameng tests.
func dmListCfg(object string) *config.ListConfig {
	cfg := config.NewListConfig()
	cfg.Object = object
	cfg.Engine = "dm"
	return cfg
}

// TestDMDispatchFollowsTheParsedEngine pins the registry row: the spelling on the
// command line must not decide which catalog is read. `--engine DM`, `--engine dm`
// and `--engine dameng` all reach the Dameng builder, and an unset engine still
// reaches MySQL's.
func TestDMDispatchFollowsTheParsedEngine(t *testing.T) {
	for _, spelling := range []string{"dm", "DM", " Dameng ", "dameng"} {
		cfg := listCfg(ObjectTables)
		cfg.Engine = spelling
		query, _, _, err := BuildQuery(cfg)
		if err != nil {
			t.Fatalf("engine %q: %v", spelling, err)
		}
		if !strings.Contains(query, "SYS.DBA_TABLES") {
			t.Errorf("engine %q read the MySQL catalog: %s", spelling, query)
		}
	}

	cfg := listCfg(ObjectTables)
	query, _, _, err := BuildQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(query, "SYS.DBA_") {
		t.Errorf("an unset engine must keep the MySQL catalog: %s", query)
	}
}

// TestDMHeadersMatchMySQLHeaders asserts that every object Dameng serves returns
// the same column names as buildMySQLQuery. Downstream scripts parse these
// headers, so they must not drift between engines.
func TestDMHeadersMatchMySQLHeaders(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.ListConfig
	}{
		{"databases", dmListCfg(ObjectDatabases)},
		{"databases_with_db", func() *config.ListConfig { c := dmListCfg(ObjectDatabases); c.Database = "mydb"; return c }()},
		{"databases_with_pattern", func() *config.ListConfig { c := dmListCfg(ObjectDatabases); c.Pattern = "app_%"; return c }()},
		{"tables", dmListCfg(ObjectTables)},
		{"tables_with_db", func() *config.ListConfig { c := dmListCfg(ObjectTables); c.Database = "mydb"; return c }()},
		{"tables_type_view", func() *config.ListConfig { c := dmListCfg(ObjectTables); c.Type = "view"; return c }()},
		{"tables_type_all", func() *config.ListConfig { c := dmListCfg(ObjectTables); c.Type = "all"; return c }()},
		{"tables_with_pattern", func() *config.ListConfig { c := dmListCfg(ObjectTables); c.Pattern = "user_%"; return c }()},
		{"views", dmListCfg(ObjectViews)},
		{"views_with_db", func() *config.ListConfig { c := dmListCfg(ObjectViews); c.Database = "mydb"; return c }()},
		{"views_with_pattern", func() *config.ListConfig { c := dmListCfg(ObjectViews); c.Pattern = "v_%"; return c }()},
		{"triggers", dmListCfg(ObjectTriggers)},
		{"triggers_with_table", func() *config.ListConfig { c := dmListCfg(ObjectTriggers); c.Table = "users"; return c }()},
		{"triggers_with_pattern", func() *config.ListConfig { c := dmListCfg(ObjectTriggers); c.Pattern = "trg_%"; return c }()},
		{"routines", dmListCfg(ObjectRoutines)},
		{"routines_procedure", func() *config.ListConfig { c := dmListCfg(ObjectRoutines); c.Type = "procedure"; return c }()},
		{"routines_function", func() *config.ListConfig { c := dmListCfg(ObjectRoutines); c.Type = "function"; return c }()},
		{"routines_with_pattern", func() *config.ListConfig { c := dmListCfg(ObjectRoutines); c.Pattern = "p_%"; return c }()},
		{"columns", func() *config.ListConfig {
			c := dmListCfg(ObjectColumns)
			c.Database = "mydb"
			c.Table = "users"
			return c
		}()},
		{"columns_with_pattern", func() *config.ListConfig {
			c := dmListCfg(ObjectColumns)
			c.Database = "mydb"
			c.Table = "users"
			c.Pattern = "n%"
			return c
		}()},
		{"indexes", func() *config.ListConfig {
			c := dmListCfg(ObjectIndexes)
			c.Database = "mydb"
			c.Table = "users"
			return c
		}()},
		{"indexes_with_pattern", func() *config.ListConfig {
			c := dmListCfg(ObjectIndexes)
			c.Database = "mydb"
			c.Table = "users"
			c.Pattern = "idx_%"
			return c
		}()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, mysqlCols, mysqlErr := buildMySQLQuery(tt.cfg)
			dmQuery, dmArgs, dmCols, dmErr := buildDMQuery(tt.cfg)
			if (mysqlErr == nil) != (dmErr == nil) {
				t.Fatalf("mysql err = %v, dm err = %v: the two engines must agree on what is valid", mysqlErr, dmErr)
			}
			if mysqlErr != nil {
				if mysqlErr.Error() != dmErr.Error() {
					t.Errorf("refusal text differs:\n  MySQL: %v\n  Dameng: %v", mysqlErr, dmErr)
				}
				return
			}
			if strings.Join(dmCols, ",") != strings.Join(mysqlCols, ",") {
				t.Errorf("columns mismatch:\n  Dameng: %v\n  MySQL:  %v", dmCols, mysqlCols)
			}

			// Dameng takes positional ? placeholders, exactly like MySQL.
			if n := strings.Count(dmQuery, "?"); n != len(dmArgs) {
				t.Errorf("Dameng placeholder count %d != args count %d in query: %s", n, len(dmArgs), dmQuery)
			}
			if strings.Contains(dmQuery, "$1") {
				t.Errorf("Dameng query carries a PostgreSQL placeholder: %s", dmQuery)
			}
			// --database, --table and --pattern never reach the statement text.
			for _, userValue := range []string{tt.cfg.Database, tt.cfg.Table, tt.cfg.Pattern} {
				if userValue != "" && strings.Contains(dmQuery, userValue) {
					t.Errorf("user input %q interpolated into: %s", userValue, dmQuery)
				}
			}
		})
	}
}

// TestDMRefusesUnmeasuredObjects pins the library-level wall for the one object
// the engine does not claim: events, which need a scheduler DM8 Oracle mode does
// not have. The CLI refuses it before it dials; a hand-built config must still
// not reach the MySQL catalog for it.
func TestDMRefusesUnmeasuredObjects(t *testing.T) {
	for _, object := range []string{ObjectEvents} {
		cfg := dmListCfg(object)
		_, _, _, err := buildDMQuery(cfg)
		want := "list " + object + " is not supported with --engine dm: this object has not been measured against Dameng Oracle-mode catalog; supported objects are databases, schemas, tables, views, columns, indexes, triggers and routines"
		if err == nil || err.Error() != want {
			t.Errorf("list %s refusal = %v, want %q", object, err, want)
		}
		if _, _, _, err := BuildQuery(cfg); err == nil {
			t.Errorf("BuildQuery must route list %s to the Dameng refusal", object)
		}
	}
}

// TestDMRequiresScope asserts columns and indexes need --database and --table,
// with the same words MySQL uses.
func TestDMRequiresScope(t *testing.T) {
	for _, object := range []string{ObjectColumns, ObjectIndexes} {
		cfg := dmListCfg(object)
		_, _, _, err := buildDMQuery(cfg)
		if err == nil || err.Error() != "--database is required for list "+object {
			t.Errorf("%s without --database: %v", object, err)
		}
		cfg.Database = "mydb"
		_, _, _, err = buildDMQuery(cfg)
		if err == nil || err.Error() != "--table is required for list "+object {
			t.Errorf("%s without --table: %v", object, err)
		}
	}
}

// TestDMInvalidType pins the --type refusal for tables.
func TestDMInvalidType(t *testing.T) {
	cfg := dmListCfg(ObjectTables)
	cfg.Type = "nonsense"
	_, _, _, err := buildDMQuery(cfg)
	if err == nil || !strings.Contains(err.Error(), "invalid --type") {
		t.Errorf("bad table type should be rejected, got %v", err)
	}
}

// TestDMDatabasesReadTheDatabaseLayer pins `list databases` to DM's database
// layer: one instance owns one database, so V$DATABASE returns the single row and
// the value is filtered there. A schema name must not be the answer here -- the
// server would reject it as a schema at login (Error -2103), and schemas have
// their own object (TestDMSchemasListTheSchemaLayer).
func TestDMDatabasesReadTheDatabaseLayer(t *testing.T) {
	cfg := dmListCfg(ObjectDatabases)
	query, args, headers, err := buildDMQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "FROM V$DATABASE") {
		t.Fatalf("databases must read V$DATABASE: %s", query)
	}
	for _, wrong := range []string{"ALL_OBJECTS", "DBA_USERS"} {
		if strings.Contains(query, wrong) {
			t.Fatalf("%s is the wrong source for the database list: %s", wrong, query)
		}
	}
	if len(args) != 0 {
		t.Fatalf("an unscoped list must bind nothing, got %v", args)
	}
	if strings.Join(headers, ",") != "database,charset,collation" {
		t.Fatalf("headers = %v", headers)
	}

	// --database filters the database name; cmd keeps it out of the DSN because
	// the server would read it as a schema and fail login.
	cfg = dmListCfg(ObjectDatabases)
	cfg.Database = "dameng"
	query, args, _, err = buildDMQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "UPPER(NAME) = UPPER(?)") {
		t.Fatalf("--database must filter the database name: %s", query)
	}
	if len(args) != 1 || args[0] != "dameng" {
		t.Fatalf("args = %v, want [dameng]", args)
	}

	cfg = dmListCfg(ObjectDatabases)
	cfg.Pattern = "DA%"
	query, args, _, err = buildDMQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "NAME LIKE ? ESCAPE") || len(args) != 1 || args[0] != "DA%" {
		t.Fatalf("--pattern must bind on the database name: %s %v", query, args)
	}
}

// TestDMSchemasListTheSchemaLayer pins `list schemas`: ALL_OBJECTS with
// OBJECT_TYPE='SCH' is the direct way to read DM's schemas, and it stays readable
// for ordinary accounts (SYS.SYSOBJECTS answers "No select privilege", -5504).
// Without --all the server's own schemas are hidden, the same default scope
// `list tables` applies; --all opens it; --database narrows to one name and, as
// on the other objects, is a schema name.
func TestDMSchemasListTheSchemaLayer(t *testing.T) {
	cfg := dmListCfg(ObjectSchemas)
	query, args, headers, err := buildDMQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "FROM ALL_OBJECTS") || !strings.Contains(query, "OBJECT_TYPE = 'SCH'") {
		t.Fatalf("schemas must read ALL_OBJECTS: %s", query)
	}
	if !strings.Contains(query, "DISTINCT OBJECT_NAME") {
		t.Fatalf("one row per schema, not per object: %s", query)
	}
	if !strings.Contains(query, "OBJECT_NAME NOT IN (") {
		t.Fatalf("the default scope must hide system schemas: %s", query)
	}
	for _, wrong := range []string{"V$DATABASE", "DBA_USERS", "SYSOBJECTS"} {
		if strings.Contains(query, wrong) {
			t.Fatalf("%s is the wrong source for the schema list: %s", wrong, query)
		}
	}
	if len(args) != 0 {
		t.Fatalf("an unscoped list must bind nothing, got %v", args)
	}
	if strings.Join(headers, ",") != "schema" {
		t.Fatalf("headers = %v, want [schema]", headers)
	}

	cfg = dmListCfg(ObjectSchemas)
	cfg.All = true
	query, _, _, err = buildDMQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(query, "NOT IN (") {
		t.Fatalf("--all must lift the exclusion: %s", query)
	}

	cfg = dmListCfg(ObjectSchemas)
	cfg.Database = "dmprobe"
	query, args, _, err = buildDMQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "UPPER(OBJECT_NAME) = UPPER(?)") {
		t.Fatalf("--database must filter the schema name: %s", query)
	}
	if len(args) != 1 || args[0] != "dmprobe" {
		t.Fatalf("args = %v, want [dmprobe]", args)
	}

	cfg = dmListCfg(ObjectSchemas)
	cfg.Pattern = "DM%"
	query, args, _, err = buildDMQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "OBJECT_NAME LIKE ? ESCAPE") || len(args) != 1 || args[0] != "DM%" {
		t.Fatalf("--pattern must bind on the schema name: %s %v", query, args)
	}

	// The object lists keep their own meaning of --database (the DSN schema and
	// the OWNER filter); separating the two layers must not touch them.
	cfg = dmListCfg(ObjectTables)
	cfg.Database = "dmprobe"
	query, _, _, err = buildDMQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "UPPER(OWNER) = UPPER(?)") {
		t.Fatalf("tables must still scope by schema: %s", query)
	}
}

// TestDMDefaultExclusionListIsMeasured pins the default scope to the schemas
// DM8 actually installs. The first version of this list was copied from Oracle
// (SYSTEM/OUTLN/SYSMAN/DBSNMP); none of those names exist on DM8, so the filter
// matched nothing and `list tables` without --database returned 12 system
// tables out of 20. Every name below was read from DBA_USERS on
// dm8_20241022_rev244896, except CTISYS, which has no account but owns
// SYSCONTEXTINDEXES. If a server upgrade adds accounts, this is the test that
// should fail first.
func TestDMDefaultExclusionListIsMeasured(t *testing.T) {
	got := strings.Split(strings.Trim(dmSystemSchemas, "'"), "','")
	want := []string{"SYS", "SYSDBA", "SYSSSO", "SYSAUDITOR", "CTISYS"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dmSystemSchemas = %v, want %v", got, want)
	}
	for _, oracleOnly := range []string{"SYSTEM", "OUTLN", "SYSMAN", "DBSNMP"} {
		if strings.Contains(dmSystemSchemas, "'"+oracleOnly+"'") {
			t.Errorf("%s is an Oracle account and does not exist on DM8", oracleOnly)
		}
	}
}

// TestDMScoping verifies the three scoping modes:
// 1. -D given → UPPER(OWNER) = UPPER(?)
// 2. -D empty, no --all → system schemas excluded
// 3. --all → no schema filter
func TestDMScoping(t *testing.T) {
	cfg := dmListCfg(ObjectTables)
	cfg.Database = "myschema"
	query, args, _, err := buildDMQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "UPPER(OWNER) = UPPER(?)") {
		t.Errorf("expected UPPER(OWNER) = UPPER(?) in: %s", query)
	}
	if len(args) != 1 || args[0] != "myschema" {
		t.Errorf("args = %v, want [myschema]", args)
	}

	cfg = dmListCfg(ObjectTables)
	query, _, _, err = buildDMQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "OWNER NOT IN ("+dmSystemSchemas+")") {
		t.Errorf("system schemas must be excluded by default: %s", query)
	}

	cfg = dmListCfg(ObjectTables)
	cfg.All = true
	query, _, _, err = buildDMQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(query, "NOT IN") {
		t.Errorf("--all must not exclude system schemas: %s", query)
	}

	// --all does not widen a columns query: --database already names the schema
	// the one table belongs to.
	cfg = dmListCfg(ObjectColumns)
	cfg.Database = "myschema"
	cfg.Table = "t1"
	cfg.All = true
	query, args, _, err = buildDMQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(query, "NOT IN") {
		t.Errorf("--all must not add a schema exclusion to list columns: %s", query)
	}
	if len(args) != 2 {
		t.Errorf("--all changed the bound args: %v", args)
	}
}

// TestDMPatternUsesBindAndEscape pins that a LIKE pattern is bound, not
// concatenated, and that a backslash is the escape character so a user-written
// \_ stays literal the way MySQL's builder does.
func TestDMPatternUsesBindAndEscape(t *testing.T) {
	cfg := dmListCfg(ObjectTables)
	cfg.Pattern = `user\_%`
	query, args, _, err := buildDMQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, `TABLE_NAME LIKE ? ESCAPE '\'`) {
		t.Errorf("pattern condition missing or wrong: %s", query)
	}
	if len(args) != 1 || args[0] != `user\_%` {
		t.Errorf("args = %v, want the pattern", args)
	}
}

// TestDMViewsReadTheAllViewsCatalog pins the source and the shape of
// `list views`. ALL_VIEWS is the readable catalog for an ordinary account
// (DBA_VIEWS answers -5504), and the two columns DM does not record stay blank
// instead of being filled with a guess: check_option has no catalog column at
// all, and is_updatable only gets a value when READ_ONLY carries one.
func TestDMViewsReadTheAllViewsCatalog(t *testing.T) {
	cfg := dmListCfg(ObjectViews)
	query, args, headers, err := buildDMQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "FROM SYS.ALL_VIEWS") {
		t.Fatalf("views must read SYS.ALL_VIEWS: %s", query)
	}
	if strings.Contains(query, "DBA_VIEWS") {
		t.Fatalf("DBA_VIEWS is denied to an ordinary account: %s", query)
	}
	if !strings.Contains(query, "OWNER NOT IN ("+dmSystemSchemas+")") {
		t.Fatalf("the default scope must hide system schemas: %s", query)
	}
	if len(args) != 0 {
		t.Fatalf("an unscoped list must bind nothing, got %v", args)
	}
	want := "database,view,check_option,is_updatable,definer,security_type"
	if strings.Join(headers, ",") != want {
		t.Fatalf("headers = %v, want %s", headers, want)
	}
	// Measured: Oracle-mode views run with the owner's privileges.
	if !strings.Contains(query, "'DEFINER'") {
		t.Errorf("security_type must be the measured DEFINER, got: %s", query)
	}

	cfg = dmListCfg(ObjectViews)
	cfg.Database = "dmprobe"
	query, args, _, err = buildDMQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "UPPER(OWNER) = UPPER(?)") || len(args) != 1 || args[0] != "dmprobe" {
		t.Fatalf("--database must scope by schema: %s %v", query, args)
	}

	cfg = dmListCfg(ObjectViews)
	cfg.All = true
	query, _, _, err = buildDMQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(query, "NOT IN (") {
		t.Fatalf("--all must lift the schema exclusion: %s", query)
	}
}

// TestDMTriggersMapTheTiming pins the two transformations `list triggers` makes:
// TRIGGERING_TYPE folds the row mode into MySQL's BEFORE/AFTER vocabulary, while
// the event and the body are printed as stored (a DM trigger may fire on several
// events, which MySQL's one-event-per-trigger column cannot express).
func TestDMTriggersMapTheTiming(t *testing.T) {
	cfg := dmListCfg(ObjectTriggers)
	query, args, headers, err := buildDMQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "FROM SYS.ALL_TRIGGERS") {
		t.Fatalf("triggers must read SYS.ALL_TRIGGERS: %s", query)
	}
	for _, arm := range []string{
		"TRIGGERING_TYPE LIKE 'BEFORE%' THEN 'BEFORE'",
		"TRIGGERING_TYPE LIKE 'AFTER%' THEN 'AFTER'",
		"TRIGGERING_TYPE LIKE 'INSTEAD OF%' THEN 'INSTEAD OF'",
	} {
		if !strings.Contains(query, arm) {
			t.Errorf("timing mapping missing %q in: %s", arm, query)
		}
	}
	want := "database,trigger,event,table,timing,statement,definer"
	if strings.Join(headers, ",") != want {
		t.Fatalf("headers = %v, want %s", headers, want)
	}
	if len(args) != 0 {
		t.Fatalf("an unscoped list must bind nothing, got %v", args)
	}

	cfg = dmListCfg(ObjectTriggers)
	cfg.Table = "fx_src"
	query, args, _, err = buildDMQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "TABLE_NAME = ?") || len(args) != 1 || args[0] != "fx_src" {
		t.Fatalf("--table must filter the subject table: %s %v", query, args)
	}

	cfg = dmListCfg(ObjectTriggers)
	cfg.Pattern = "FX%"
	query, args, _, err = buildDMQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "TRIGGER_NAME LIKE ? ESCAPE") || len(args) != 1 || args[0] != "FX%" {
		t.Fatalf("--pattern must bind on the trigger name: %s %v", query, args)
	}
}

// TestDMRoutinesReadTheAllProceduresCatalog pins `list routines`. DM8 has no
// DBA_ROUTINES at all (-2106), and ALL_PROCEDURES lists packages and their
// subprograms next to top-level routines: only rows with PROCEDURE_NAME NULL
// are routines, which is what keeps a package from being reported as one.
func TestDMRoutinesReadTheAllProceduresCatalog(t *testing.T) {
	cfg := dmListCfg(ObjectRoutines)
	query, args, headers, err := buildDMQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "FROM SYS.ALL_PROCEDURES") {
		t.Fatalf("routines must read SYS.ALL_PROCEDURES: %s", query)
	}
	if strings.Contains(query, "DBA_ROUTINES") {
		t.Fatalf("DBA_ROUTINES does not exist on DM8: %s", query)
	}
	if !strings.Contains(query, "PROCEDURE_NAME IS NULL") {
		t.Fatalf("package subprograms and package rows must be excluded: %s", query)
	}
	if !strings.Contains(query, "OBJECT_TYPE IN ('PROCEDURE', 'FUNCTION')") {
		t.Fatalf("the default must list both routine kinds: %s", query)
	}
	want := "database,routine,type,definer,security,deterministic,data_access"
	if strings.Join(headers, ",") != want {
		t.Fatalf("headers = %v, want %s", headers, want)
	}
	if len(args) != 0 {
		t.Fatalf("an unscoped list must bind nothing, got %v", args)
	}

	for _, tt := range []struct{ typ, clause string }{
		{"procedure", "OBJECT_TYPE = 'PROCEDURE'"},
		{"function", "OBJECT_TYPE = 'FUNCTION'"},
	} {
		cfg = dmListCfg(ObjectRoutines)
		cfg.Type = tt.typ
		query, _, _, err = buildDMQuery(cfg)
		if err != nil {
			t.Fatalf("--type %s: %v", tt.typ, err)
		}
		if !strings.Contains(query, tt.clause) {
			t.Errorf("--type %s must filter with %q: %s", tt.typ, tt.clause, query)
		}
	}

	cfg = dmListCfg(ObjectRoutines)
	cfg.Type = "nonsense"
	if _, _, _, err := buildDMQuery(cfg); err == nil || !strings.Contains(err.Error(), "invalid --type") {
		t.Errorf("bad routine type should be rejected, got %v", err)
	}

	cfg = dmListCfg(ObjectRoutines)
	cfg.Pattern = "FX%"
	query, args, _, err = buildDMQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "OBJECT_NAME LIKE ? ESCAPE") || len(args) != 1 || args[0] != "FX%" {
		t.Fatalf("--pattern must bind on the routine name: %s %v", query, args)
	}
}

// TestDMIndexesJoinColumnsAndSkipTheImplicitClusterIndex pins the reason
// `list indexes` can return no rows for a table that DBA_INDEXES mentions: every
// DM table carries one implicit cluster index, its DDL has no column list, and
// DBA_IND_COLUMNS has no row for it -- so the join (not a filter) is what leaves
// it out. Two properties matter and are asserted here: the join keys include the
// table name, and there is no INDEX_TYPE filter, because a declared CLUSTER
// PRIMARY KEY does have column rows and a type filter would hide it.
func TestDMIndexesJoinColumnsAndSkipTheImplicitClusterIndex(t *testing.T) {
	cfg := dmListCfg(ObjectIndexes)
	cfg.Database = "dmprobe"
	cfg.Table = "fx_dst"
	query, args, headers, err := buildDMQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	wantJoin := "JOIN SYS.DBA_IND_COLUMNS ic ON i.OWNER = ic.INDEX_OWNER" +
		" AND i.INDEX_NAME = ic.INDEX_NAME AND i.TABLE_NAME = ic.TABLE_NAME"
	if !strings.Contains(query, wantJoin) {
		t.Fatalf("the column join must carry all three keys, got: %s", query)
	}
	for _, banned := range []string{"INDEX_TYPE =", "INDEX_TYPE <", "INDEX_TYPE >", "INDEX_TYPE NOT"} {
		if strings.Contains(query, banned) {
			t.Errorf("an INDEX_TYPE filter (%q) would hide a CLUSTER PRIMARY KEY: %s", banned, query)
		}
	}
	if !strings.Contains(query, "UPPER(i.OWNER) = UPPER(?)") || !strings.Contains(query, "i.TABLE_NAME = ?") {
		t.Fatalf("--database and --table must both scope the query: %s", query)
	}
	if len(args) != 2 || args[0] != "dmprobe" || args[1] != "fx_dst" {
		t.Fatalf("args = %v, want [dmprobe fx_dst]", args)
	}
	want := "database,table,index,non_unique,seq,column,cardinality,index_type"
	if strings.Join(headers, ",") != want {
		t.Fatalf("headers = %v, want %s", headers, want)
	}

	cfg = dmListCfg(ObjectIndexes)
	cfg.Database = "dmprobe"
	cfg.Table = "fx_src"
	cfg.Pattern = "INDEX%"
	query, args, _, err = buildDMQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "i.INDEX_NAME LIKE ? ESCAPE") || len(args) != 3 || args[2] != "INDEX%" {
		t.Fatalf("--pattern must bind on the index name: %s %v", query, args)
	}

	for _, object := range []string{ObjectIndexes, ObjectColumns} {
		cfg = dmListCfg(object)
		if _, _, _, err := buildDMQuery(cfg); err == nil || !strings.Contains(err.Error(), "--database is required") {
			t.Errorf("%s without --database = %v, want the scope error", object, err)
		}
	}
}
