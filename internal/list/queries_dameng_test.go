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

// TestDMHeadersMatchMySQLHeaders asserts that the four objects Dameng serves
// return the same column names as buildMySQLQuery. Downstream scripts parse these
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

// TestDMRefusesUnmeasuredObjects pins the library-level wall for the four objects
// the MVP does not claim. The CLI refuses them before it dials; a hand-built
// config must still not reach the MySQL catalog for them.
func TestDMRefusesUnmeasuredObjects(t *testing.T) {
	for _, object := range []string{ObjectViews, ObjectTriggers, ObjectRoutines, ObjectEvents} {
		cfg := dmListCfg(object)
		_, _, _, err := buildDMQuery(cfg)
		want := "list " + object + " is not supported with --engine dm: this object has not been measured against Dameng Oracle-mode catalog; supported objects are databases, tables, columns and indexes"
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
