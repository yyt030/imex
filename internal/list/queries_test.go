package list

import (
	"fmt"
	"strings"
	"testing"

	"github.com/imex/internal/config"
)

func listCfg(object string) *config.ListConfig {
	cfg := config.NewListConfig()
	cfg.Object = object
	return cfg
}

func TestObjectsCoverTheCommandTree(t *testing.T) {
	want := []string{"databases", "schemas", "tables", "views", "columns", "indexes", "triggers", "routines", "events"}
	got := Objects()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Objects() = %v, want %v", got, want)
	}
}

func TestBuildQueryDatabases(t *testing.T) {
	cfg := listCfg(ObjectDatabases)
	query, args, columns, err := BuildQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 0 {
		t.Errorf("no filter should bind no args, got %v", args)
	}
	if !strings.Contains(query, "FROM information_schema.SCHEMATA") || !strings.Contains(query, "ORDER BY SCHEMA_NAME") {
		t.Errorf("unexpected query: %s", query)
	}
	if strings.Contains(query, "NOT IN") {
		t.Errorf("databases must not hide system schemas: %s", query)
	}
	if strings.Join(columns, ",") != "database,charset,collation" {
		t.Errorf("columns = %v", columns)
	}

	cfg.Database = "mydb"
	cfg.Pattern = "app_%"
	query, args, _, err = BuildQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 2 || args[0] != "mydb" || args[1] != "app_%" {
		t.Errorf("args = %v, want [mydb app_%%]", args)
	}
	if !strings.Contains(query, "SCHEMA_NAME = ?") || !strings.Contains(query, "LIKE ?") {
		t.Errorf("filters missing from %s", query)
	}
}

func TestBuildQueryTables(t *testing.T) {
	cfg := listCfg(ObjectTables)
	query, args, columns, err := BuildQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 0 {
		t.Errorf("scope-only query should bind no args, got %v", args)
	}
	if !strings.Contains(query, "TABLE_SCHEMA NOT IN ("+systemSchemas+")") {
		t.Errorf("system schemas must be excluded by default: %s", query)
	}
	if !strings.Contains(query, "TABLE_TYPE = 'BASE TABLE'") {
		t.Errorf("default type must be base: %s", query)
	}
	if strings.Join(columns, ",") != "database,table,type,engine,rows,data_length,index_length,comment" {
		t.Errorf("columns = %v", columns)
	}

	cfg.Database = "mydb"
	cfg.Type = "all"
	cfg.Pattern = "user_%"
	_, args, _, err = BuildQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 2 || args[0] != "mydb" || args[1] != "user_%" {
		t.Errorf("args = %v", args)
	}

	cfg.Database = ""
	cfg.All = true
	query, _, _, err = BuildQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(query, "NOT IN") {
		t.Errorf("--all without --database must not exclude system schemas: %s", query)
	}

	cfg.Type = "view"
	query, _, _, err = BuildQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "TABLE_TYPE = 'VIEW'") {
		t.Errorf("view type missing: %s", query)
	}

	cfg.Type = "nonsense"
	if _, _, _, err := BuildQuery(cfg); err == nil || !strings.Contains(err.Error(), "invalid --type") {
		t.Errorf("bad type should be rejected, got %v", err)
	}
}

func TestBuildQueryColumnsAndIndexesRequireScope(t *testing.T) {
	for _, object := range []string{ObjectColumns, ObjectIndexes} {
		cfg := listCfg(object)
		if _, _, _, err := BuildQuery(cfg); err == nil || !strings.Contains(err.Error(), "--database is required") {
			t.Errorf("%s without --database: %v", object, err)
		}
		cfg.Database = "mydb"
		if _, _, _, err := BuildQuery(cfg); err == nil || !strings.Contains(err.Error(), "--table is required") {
			t.Errorf("%s without --table: %v", object, err)
		}
	}
}

func TestBuildQueryColumns(t *testing.T) {
	cfg := listCfg(ObjectColumns)
	cfg.Database = "mydb"
	cfg.Table = "users"
	cfg.Pattern = "n%"
	query, args, columns, err := BuildQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 3 || args[0] != "mydb" || args[1] != "users" || args[2] != "n%" {
		t.Errorf("args = %v", args)
	}
	if !strings.Contains(query, "COLUMN_NAME LIKE ?") || !strings.Contains(query, "ORDER BY ORDINAL_POSITION") {
		t.Errorf("unexpected query: %s", query)
	}
	if strings.Join(columns, ",") != "database,table,column,ordinal,type,nullable,default,key,extra,comment" {
		t.Errorf("columns = %v", columns)
	}
}

func TestBuildQueryIndexes(t *testing.T) {
	cfg := listCfg(ObjectIndexes)
	cfg.Database = "mydb"
	cfg.Table = "users"
	cfg.Pattern = "PRIMARY"
	query, args, columns, err := BuildQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 3 || args[2] != "PRIMARY" {
		t.Errorf("args = %v", args)
	}
	if !strings.Contains(query, "INDEX_NAME LIKE ?") {
		t.Errorf("index pattern missing: %s", query)
	}
	if strings.Join(columns, ",") != "database,table,index,non_unique,seq,column,cardinality,index_type" {
		t.Errorf("columns = %v", columns)
	}
}

func TestBuildQueryTriggersRoutinesEvents(t *testing.T) {
	cfg := listCfg(ObjectTriggers)
	cfg.Database = "mydb"
	cfg.Table = "users"
	query, args, _, err := BuildQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 2 || args[0] != "mydb" || args[1] != "users" {
		t.Errorf("args = %v", args)
	}
	if !strings.Contains(query, "EVENT_OBJECT_TABLE = ?") {
		t.Errorf("table filter missing: %s", query)
	}

	cfg = listCfg(ObjectRoutines)
	cfg.Type = "function"
	query, _, _, err = BuildQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "ROUTINE_TYPE = 'FUNCTION'") {
		t.Errorf("routine type missing: %s", query)
	}
	cfg.Type = "procedure"
	query, _, _, err = BuildQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "ROUTINE_TYPE = 'PROCEDURE'") {
		t.Errorf("routine type missing: %s", query)
	}
	cfg.Type = "nonsense"
	if _, _, _, err := BuildQuery(cfg); err == nil || !strings.Contains(err.Error(), "invalid --type") {
		t.Errorf("bad routine type should be rejected, got %v", err)
	}

	cfg = listCfg(ObjectEvents)
	cfg.Pattern = "nightly_%"
	query, args, _, err = BuildQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 1 || args[0] != "nightly_%" {
		t.Errorf("args = %v", args)
	}
	if !strings.Contains(query, "EVENT_NAME LIKE ?") {
		t.Errorf("event pattern missing: %s", query)
	}
}

func TestBuildQueryUnknownObject(t *testing.T) {
	cfg := listCfg("tabel")
	_, _, _, err := BuildQuery(cfg)
	if err == nil || !strings.Contains(err.Error(), "unknown object") {
		t.Fatalf("unknown object should be rejected, got %v", err)
	}
}

func TestBuildQueryAlwaysUsesInformationSchema(t *testing.T) {
	for _, object := range Objects() {
		cfg := listCfg(object)
		cfg.Database = "mydb"
		cfg.Table = "users"
		query, _, _, err := BuildQuery(cfg)
		if err != nil {
			t.Errorf("%s: %v", object, err)
			continue
		}
		if !strings.Contains(query, "information_schema.") {
			t.Errorf("%s does not read information_schema: %s", object, query)
		}
	}
}

// TestBuildQueryDispatchesOnEngine pins the engine switch on the one entry point
// Run uses. Both the flag default ("mysql") and the zero value must stay on the
// MySQL builder — that is what every run from before PostgreSQL support looked
// like to the server — and "pg" must reach the catalog builder with $n
// placeholders instead of ?.
func TestBuildQueryDispatchesOnEngine(t *testing.T) {
	for _, engine := range []string{"mysql", ""} {
		cfg := listCfg(ObjectTables)
		cfg.Engine = engine
		query, _, _, err := BuildQuery(cfg)
		if err != nil {
			t.Fatalf("engine %q: %v", engine, err)
		}
		if !strings.Contains(query, "FROM information_schema.TABLES") {
			t.Errorf("engine %q did not build the MySQL statement: %s", engine, query)
		}
		if strings.Contains(query, "$1") {
			t.Errorf("engine %q emitted a PostgreSQL placeholder: %s", engine, query)
		}
	}

	pgCfg := listCfg(ObjectTables)
	pgCfg.Engine = "pg"
	query, args, _, err := BuildQuery(pgCfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "FROM pg_catalog.pg_class c") {
		t.Errorf("--engine pg did not build the catalog statement: %s", query)
	}
	if strings.Contains(query, "?") {
		t.Errorf("--engine pg emitted a MySQL placeholder: %s", query)
	}
	if len(args) != 0 {
		t.Errorf("--engine pg bound %d args to an unscoped list tables: %v", len(args), args)
	}
}

// TestBuildQueryNormalisesTheEngineSpelling pins the 131 ruling: the catalog is
// chosen by the parsed engine, not by the exact text typed after --engine. Every
// spelling of pg must produce the byte-identical statement and argument list of
// "pg", and the default (empty, mysql, or a value cmd would have rejected) must
// still land on the MySQL catalog.
func TestBuildQueryNormalisesTheEngineSpelling(t *testing.T) {
	ref := listCfg(ObjectColumns)
	ref.Engine = "pg"
	ref.Database = "mydb"
	ref.Table = "t"
	refQuery, refArgs, refCols, err := BuildQuery(ref)
	if err != nil {
		t.Fatal(err)
	}

	for _, engine := range []string{"PG", "Pg", " pg ", "pg\t", "PG\t"} {
		cfg := listCfg(ObjectColumns)
		cfg.Engine = engine
		cfg.Database = "mydb"
		cfg.Table = "t"
		query, args, cols, err := BuildQuery(cfg)
		if err != nil {
			t.Fatalf("engine %q: %v", engine, err)
		}
		if query != refQuery {
			t.Errorf("engine %q built a different statement than \"pg\":\n%s\nvs\n%s", engine, query, refQuery)
		}
		if !strings.Contains(query, "current_schema()") {
			// PostgreSQL has an information_schema of its own, so the catalog name
			// alone cannot tell the two statements apart; the schema function can.
			t.Errorf("engine %q did not build the PostgreSQL statement: %s", engine, query)
		}
		if fmt.Sprint(args) != fmt.Sprint(refArgs) {
			t.Errorf("engine %q bound %v, want %v", engine, args, refArgs)
		}
		if strings.Join(cols, ",") != strings.Join(refCols, ",") {
			t.Errorf("engine %q column names %v, want %v", engine, cols, refCols)
		}
	}

	for _, engine := range []string{"", "mysql", "MYSQL", "oracle"} {
		cfg := listCfg(ObjectTables)
		cfg.Engine = engine
		query, _, _, err := BuildQuery(cfg)
		if err != nil {
			t.Fatalf("engine %q: %v", engine, err)
		}
		if !strings.Contains(query, "FROM information_schema.TABLES") {
			t.Errorf("engine %q did not build the MySQL statement: %s", engine, query)
		}
	}
}

// TestBuildQuerySchemas pins `list schemas` on MySQL: one namespace, so the
// catalog is information_schema.SCHEMATA -- the table SHOW DATABASES reads --
// and the object prints one column. --all is a no-op here because the list never
// hides the server's own schemas, matching `list databases`.
func TestBuildQuerySchemas(t *testing.T) {
	cfg := listCfg(ObjectSchemas)
	query, args, columns, err := BuildQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "FROM information_schema.SCHEMATA") || !strings.Contains(query, "ORDER BY SCHEMA_NAME") {
		t.Errorf("unexpected query: %s", query)
	}
	if strings.Contains(query, "NOT IN") {
		t.Errorf("schemas must not hide system schemas on MySQL: %s", query)
	}
	if len(args) != 0 {
		t.Errorf("no filter should bind no args, got %v", args)
	}
	if strings.Join(columns, ",") != "schema" {
		t.Errorf("columns = %v, want [schema]", columns)
	}

	cfg.Database = "appdb"
	query, args, _, err = BuildQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "SCHEMA_NAME = ?") || len(args) != 1 || args[0] != "appdb" {
		t.Errorf("--database must bind the schema name: %s %v", query, args)
	}

	cfg = listCfg(ObjectSchemas)
	cfg.Pattern = "app%"
	query, args, _, err = BuildQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "SCHEMA_NAME LIKE ? ESCAPE") || len(args) != 1 || args[0] != "app%" {
		t.Errorf("--pattern must bind on SCHEMA_NAME: %s %v", query, args)
	}
}
