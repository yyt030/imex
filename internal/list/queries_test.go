package list

import (
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
	want := []string{"databases", "tables", "views", "columns", "indexes", "triggers", "routines", "events"}
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
