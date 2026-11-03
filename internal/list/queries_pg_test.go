package list

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/imex/internal/config"
)

// pgListCfg mirrors listCfg but is explicit for PG tests.
func pgListCfg(object string) *config.ListConfig {
	cfg := config.NewListConfig()
	cfg.Object = object
	cfg.Engine = "pg"
	return cfg
}

// TestPGColumnsMatchMySQLColumns asserts that buildPGQuery returns the same
// column names as BuildQuery (the MySQL builder) for every object and every
// option combination. Downstream scripts parse these headers, so they must not
// drift between engines.
func TestPGColumnsMatchMySQLColumns(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.ListConfig
	}{
		{"databases", pgListCfg(ObjectDatabases)},
		{"databases_with_db", func() *config.ListConfig { c := pgListCfg(ObjectDatabases); c.Database = "mydb"; return c }()},
		{"databases_with_pattern", func() *config.ListConfig { c := pgListCfg(ObjectDatabases); c.Pattern = "app_%"; return c }()},
		{"tables", pgListCfg(ObjectTables)},
		{"tables_with_db", func() *config.ListConfig { c := pgListCfg(ObjectTables); c.Database = "mydb"; return c }()},
		{"tables_type_view", func() *config.ListConfig { c := pgListCfg(ObjectTables); c.Type = "view"; return c }()},
		{"tables_type_all", func() *config.ListConfig { c := pgListCfg(ObjectTables); c.Type = "all"; return c }()},
		{"tables_with_pattern", func() *config.ListConfig { c := pgListCfg(ObjectTables); c.Pattern = "user_%"; return c }()},
		{"views", pgListCfg(ObjectViews)},
		{"views_with_db", func() *config.ListConfig { c := pgListCfg(ObjectViews); c.Database = "mydb"; return c }()},
		{"columns", func() *config.ListConfig {
			c := pgListCfg(ObjectColumns)
			c.Database = "mydb"
			c.Table = "users"
			return c
		}()},
		{"columns_no_db", func() *config.ListConfig { c := pgListCfg(ObjectColumns); c.Table = "users"; return c }()},
		{"columns_with_pattern", func() *config.ListConfig {
			c := pgListCfg(ObjectColumns)
			c.Database = "mydb"
			c.Table = "users"
			c.Pattern = "n%"
			return c
		}()},
		{"indexes", func() *config.ListConfig {
			c := pgListCfg(ObjectIndexes)
			c.Database = "mydb"
			c.Table = "users"
			return c
		}()},
		{"indexes_no_db", func() *config.ListConfig { c := pgListCfg(ObjectIndexes); c.Table = "users"; return c }()},
		{"indexes_with_pattern", func() *config.ListConfig {
			c := pgListCfg(ObjectIndexes)
			c.Database = "mydb"
			c.Table = "users"
			c.Pattern = "idx_%"
			return c
		}()},
		{"triggers", pgListCfg(ObjectTriggers)},
		{"triggers_with_table", func() *config.ListConfig {
			c := pgListCfg(ObjectTriggers)
			c.Database = "mydb"
			c.Table = "users"
			return c
		}()},
		{"routines", pgListCfg(ObjectRoutines)},
		{"routines_type_function", func() *config.ListConfig {
			c := pgListCfg(ObjectRoutines)
			c.Database = "mydb"
			c.Type = "function"
			return c
		}()},
		{"routines_type_procedure", func() *config.ListConfig {
			c := pgListCfg(ObjectRoutines)
			c.Database = "mydb"
			c.Type = "procedure"
			return c
		}()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Events is the one object PostgreSQL cannot answer at all; that refusal
			// has its own test.
			if tt.cfg.Object == ObjectEvents {
				return
			}

			// Build the same object for both engines. A usage error belongs to the
			// CLI contract, so the engines must refuse with the same words: `list
			// columns` without --database is an error for MySQL and must stay an
			// error for pg, not quietly become a wider query.
			mysqlQuery, mysqlArgs, mysqlCols, mysqlErr := buildMySQLQuery(tt.cfg)
			pgQuery, pgArgs, pgCols, pgErr := buildPGQuery(tt.cfg)
			if (mysqlErr == nil) != (pgErr == nil) {
				t.Fatalf("mysql err = %v, pg err = %v: the two engines must agree on what is valid", mysqlErr, pgErr)
			}
			if mysqlErr != nil {
				if mysqlErr.Error() != pgErr.Error() {
					t.Errorf("refusal text differs:\n  MySQL: %v\n  PG:    %v", mysqlErr, pgErr)
				}
				return
			}

			// Compare column names.
			if strings.Join(pgCols, ",") != strings.Join(mysqlCols, ",") {
				t.Errorf("columns mismatch:\n  PG:    %v\n  MySQL: %v", pgCols, mysqlCols)
			}

			verifyPGPlaceholders(t, pgQuery, pgArgs)

			// MySQL uses ? placeholders.
			mysqlPlaceholderCount := strings.Count(mysqlQuery, "?")
			if mysqlPlaceholderCount != len(mysqlArgs) {
				t.Errorf("MySQL placeholder count %d != args count %d", mysqlPlaceholderCount, len(mysqlArgs))
			}
		})
	}
}

// verifyPGPlaceholders checks that PG placeholders are $1..$n with no gaps
// and that the count matches len(args).
func verifyPGPlaceholders(t *testing.T, query string, args []any) {
	t.Helper()

	// Count $ signs (rough check).
	dollarCount := strings.Count(query, "$")
	if dollarCount != len(args) {
		t.Errorf("PG $ count %d != args count %d in query: %s", dollarCount, len(args), query)
	}

	// Verify PG placeholders are $1, $2, ... with no gaps.
	re := regexp.MustCompile(`\$(\d+)`)
	matches := re.FindAllStringSubmatch(query, -1)
	if len(matches) != len(args) {
		t.Errorf("PG found %d placeholders but have %d args", len(matches), len(args))
	}
	for i, m := range matches {
		expected := fmt.Sprintf("$%d", i+1)
		if m[0] != expected {
			t.Errorf("PG placeholder %d is %s, expected %s", i+1, m[0], expected)
		}
	}
}

// TestPGEventsError asserts that buildPGQuery returns an error for events,
// since PostgreSQL has no event scheduler.
func TestPGEventsError(t *testing.T) {
	cfg := pgListCfg(ObjectEvents)
	_, _, _, err := buildPGQuery(cfg)
	if err == nil || !strings.Contains(err.Error(), "no event scheduler") {
		t.Errorf("events should fail with 'no event scheduler', got %v", err)
	}
}

// TestPGInvalidType asserts that invalid --type values are rejected.
func TestPGInvalidType(t *testing.T) {
	cfg := pgListCfg(ObjectTables)
	cfg.Type = "nonsense"
	_, _, _, err := buildPGQuery(cfg)
	if err == nil || !strings.Contains(err.Error(), "invalid --type") {
		t.Errorf("bad table type should be rejected, got %v", err)
	}

	cfg = pgListCfg(ObjectRoutines)
	cfg.Type = "nonsense"
	_, _, _, err = buildPGQuery(cfg)
	if err == nil || !strings.Contains(err.Error(), "invalid --type") {
		t.Errorf("bad routine type should be rejected, got %v", err)
	}
}

// TestPGRequiresScope asserts that columns and indexes require --database and
// --table, with the same words MySQL uses: -D names the connected database and
// the schema it resolves to, so it is still the object container the user has to
// name.
func TestPGRequiresScope(t *testing.T) {
	for _, object := range []string{ObjectColumns, ObjectIndexes} {
		cfg := pgListCfg(object)
		_, _, _, err := buildPGQuery(cfg)
		if err == nil || err.Error() != "--database is required for list "+object {
			t.Errorf("%s without --database: %v", object, err)
		}
		cfg.Database = "mydb"
		_, _, _, err = buildPGQuery(cfg)
		if err == nil || err.Error() != "--table is required for list "+object {
			t.Errorf("%s without --table: %v", object, err)
		}
	}
}

// TestPGNamespaceScoping verifies the three scoping modes:
// 1. -D given → current_database() = $1 AND schema = current_schema()
// 2. -D empty, no --all → schema NOT IN (system schemas)
// 3. --all → no schema filter
func TestPGNamespaceScoping(t *testing.T) {
	// Mode 1: -D given → scoped to current_database() + current_schema().
	cfg := pgListCfg(ObjectTables)
	cfg.Database = "pgprobe"
	query, args, _, err := buildPGQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "current_database() = $1") {
		t.Errorf("expected current_database() = $1 in: %s", query)
	}
	if !strings.Contains(query, "n.nspname = current_schema()") {
		t.Errorf("expected n.nspname = current_schema() in: %s", query)
	}
	if len(args) != 1 || args[0] != "pgprobe" {
		t.Errorf("args = %v, want [pgprobe]", args)
	}

	// Mode 2: -D empty, no --all → system schemas excluded.
	cfg = pgListCfg(ObjectTables)
	query, _, _, err = buildPGQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "NOT IN ("+pgSystemSchemas+")") {
		t.Errorf("system schemas must be excluded by default: %s", query)
	}

	// Mode 3: --all → no schema exclusion.
	cfg = pgListCfg(ObjectTables)
	cfg.All = true
	query, _, _, err = buildPGQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(query, "NOT IN") {
		t.Errorf("--all must not exclude system schemas: %s", query)
	}
	if strings.Contains(query, "current_database()") {
		t.Errorf("--all must not add current_database() filter: %s", query)
	}
}

// TestPGInfoSchemaScoping verifies the information_schema scoping `list columns`
// uses: the catalog comes from -D, the schema from current_schema().
func TestPGInfoSchemaScoping(t *testing.T) {
	cfg := pgListCfg(ObjectColumns)
	cfg.Database = "pgprobe"
	cfg.Table = "t1"
	query, args, _, err := buildPGQuery(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "c.table_catalog = $1") {
		t.Errorf("expected c.table_catalog = $1 in: %s", query)
	}
	if !strings.Contains(query, "c.table_schema = current_schema()") {
		t.Errorf("expected c.table_schema = current_schema() in: %s", query)
	}
	// $1 = pgprobe, $2 = t1
	if len(args) != 2 || args[0] != "pgprobe" || args[1] != "t1" {
		t.Errorf("args = %v, want [pgprobe t1]", args)
	}

	// --all does not widen a columns query: --database already names the catalog
	// the one table belongs to, and MySQL behaves the same way.
	cfg.All = true
	query, args, _, err = buildPGQuery(cfg)
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

// TestPGLiveServer runs the built queries against a real PostgreSQL server when
// IMEX_TEST_PG_DSN is set. It creates fixture objects in schema imex_list_it
// AND public, and verifies:
//   - with -D pgprobe: only public-schema objects are returned
//   - without -D: both imex_list_it and public objects are returned
func TestPGLiveServer(t *testing.T) {
	dsn := os.Getenv("IMEX_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("no PostgreSQL test server")
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("failed to open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("failed to ping: %v", err)
	}

	// Create fixture schema and objects if they don't exist.
	setupFixtures(t, db)

	// Test 1: -D pgprobe scopes to current_schema() = 'public'.
	t.Run("scoped_to_public", func(t *testing.T) {
		cfg := pgListCfg(ObjectTables)
		cfg.Database = "pgprobe" // matches the DSN database
		query, args, columns, err := buildPGQuery(cfg)
		if err != nil {
			t.Fatalf("buildPGQuery failed: %v", err)
		}
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			t.Fatalf("query failed: %v\nQuery: %s\nArgs: %v", err, query, args)
		}
		defer rows.Close()

		rowCols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns: %v", err)
		}
		if len(rowCols) != len(columns) {
			t.Errorf("result has %d columns, expected %d", len(rowCols), len(columns))
		}

		// Collect schema names from results.
		var schemas []string
		for rows.Next() {
			vals := make([]sql.NullString, len(columns))
			dest := make([]any, len(columns))
			for i := range vals {
				dest[i] = &vals[i]
			}
			if err := rows.Scan(dest...); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if vals[0].Valid {
				schemas = append(schemas, vals[0].String)
			}
		}
		// All returned rows should be in 'public' schema.
		for _, s := range schemas {
			if s != "public" {
				t.Errorf("scoped query returned schema %q, expected only 'public'", s)
			}
		}
		if len(schemas) == 0 {
			t.Error("scoped query returned no rows; expected at least public.rt_probe")
		}
	})

	// Test 2: no -D returns objects from all non-system schemas.
	t.Run("all_non_system", func(t *testing.T) {
		cfg := pgListCfg(ObjectTables)
		// No Database, no All → default exclusion of system schemas.
		query, args, _, err := buildPGQuery(cfg)
		if err != nil {
			t.Fatalf("buildPGQuery failed: %v", err)
		}
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			t.Fatalf("query failed: %v\nQuery: %s\nArgs: %v", err, query, args)
		}
		defer rows.Close()

		var schemas []string
		for rows.Next() {
			var schema sql.NullString
			var colRest [7]sql.NullString
			dest := []any{&schema}
			for i := range colRest {
				dest = append(dest, &colRest[i])
			}
			if err := rows.Scan(dest...); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if schema.Valid {
				schemas = append(schemas, schema.String)
			}
		}

		// Should see both 'public' and 'imex_list_it'.
		hasPublic, hasImex := false, false
		for _, s := range schemas {
			if s == "public" {
				hasPublic = true
			}
			if s == "imex_list_it" {
				hasImex = true
			}
		}
		if !hasPublic {
			t.Errorf("expected 'public' schema in results, got: %v", schemas)
		}
		if !hasImex {
			t.Errorf("expected 'imex_list_it' schema in results, got: %v", schemas)
		}
	})

	// Test 3: every object type runs without error against the live server.
	objects := []struct {
		name    string
		cfg     *config.ListConfig
		minRows int
	}{
		{"databases", pgListCfg(ObjectDatabases), 1},
		{"tables_scoped", func() *config.ListConfig { c := pgListCfg(ObjectTables); c.Database = "pgprobe"; return c }(), 1},
		{"views_scoped", func() *config.ListConfig { c := pgListCfg(ObjectViews); c.Database = "pgprobe"; return c }(), 1},
		{"columns_scoped", func() *config.ListConfig {
			c := pgListCfg(ObjectColumns)
			c.Database = "pgprobe"
			c.Table = "rt_probe"
			return c
		}(), 1},
		{"indexes_scoped", func() *config.ListConfig {
			c := pgListCfg(ObjectIndexes)
			c.Database = "pgprobe"
			c.Table = "rt_probe"
			return c
		}(), 1},
		{"triggers_scoped", func() *config.ListConfig { c := pgListCfg(ObjectTriggers); c.Database = "pgprobe"; return c }(), 1},
		{"routines_scoped", func() *config.ListConfig { c := pgListCfg(ObjectRoutines); c.Database = "pgprobe"; return c }(), 1},
	}
	for _, tt := range objects {
		t.Run("live_"+tt.name, func(t *testing.T) {
			query, args, columns, err := buildPGQuery(tt.cfg)
			if err != nil {
				t.Fatalf("buildPGQuery failed: %v", err)
			}
			rows, err := db.QueryContext(ctx, query, args...)
			if err != nil {
				t.Fatalf("query failed: %v\nQuery: %s\nArgs: %v", err, query, args)
			}
			defer rows.Close()

			rowCols, err := rows.Columns()
			if err != nil {
				t.Fatalf("columns: %v", err)
			}
			if len(rowCols) != len(columns) {
				t.Errorf("result has %d columns, expected %d", len(rowCols), len(columns))
			}

			count := 0
			for rows.Next() {
				count++
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}
			if count < tt.minRows {
				t.Errorf("got %d rows, expected at least %d", count, tt.minRows)
			}
		})
	}
}

// setupFixtures creates the imex_list_it schema and fixture objects, plus
// public-schema fixtures so the scoped path is genuinely exercised.
func setupFixtures(t *testing.T, db *sql.DB) {
	ctx := context.Background()
	stmts := []string{
		// imex_list_it schema fixtures.
		"CREATE SCHEMA IF NOT EXISTS imex_list_it",
		`CREATE TABLE IF NOT EXISTS imex_list_it.t1 (
			id serial PRIMARY KEY,
			name text NOT NULL,
			num numeric(10,2),
			ts timestamptz,
			flt real,
			created date,
			gen text GENERATED ALWAYS AS (upper(name)) STORED,
			blob bytea
		)`,
		`CREATE TABLE IF NOT EXISTS imex_list_it.t2 (
			aid bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			body text
		)`,
		`CREATE TABLE IF NOT EXISTS imex_list_it."MixedCase" (
			a integer,
			b varchar(20) DEFAULT 'x'
		)`,
		`CREATE OR REPLACE VIEW imex_list_it.v1 AS SELECT id, name FROM imex_list_it.t1`,
		`CREATE INDEX IF NOT EXISTS ix_t1_created ON imex_list_it.t1 (created)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_t1_name ON imex_list_it.t1 (name)`,
		`CREATE OR REPLACE FUNCTION imex_list_it.trg_fn() RETURNS trigger AS $$ BEGIN RETURN NEW; END; $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg1 ON imex_list_it.t1`,
		`CREATE TRIGGER trg1 BEFORE INSERT ON imex_list_it.t1 FOR EACH ROW EXECUTE FUNCTION imex_list_it.trg_fn()`,
		`CREATE OR REPLACE FUNCTION imex_list_it.f1(x integer) RETURNS integer AS $$ SELECT x * 2 $$ LANGUAGE sql`,
		`CREATE OR REPLACE PROCEDURE imex_list_it.p1() LANGUAGE plpgsql AS $$ BEGIN NULL; END; $$`,
		"COMMENT ON COLUMN imex_list_it.t1.name IS 'the name column'",

		// public schema fixtures (so scoped queries with -D pgprobe find something).
		`CREATE TABLE IF NOT EXISTS public.rt_probe (
			id serial PRIMARY KEY,
			label text
		)`,
		`CREATE INDEX IF NOT EXISTS ix_rt_probe_label ON public.rt_probe (label)`,
		`CREATE OR REPLACE VIEW public.v_probe AS SELECT id, label FROM public.rt_probe`,
		`CREATE OR REPLACE FUNCTION public.fn_probe() RETURNS integer AS $$ SELECT 1 $$ LANGUAGE sql`,
		`CREATE OR REPLACE FUNCTION public.trg_probe_fn() RETURNS trigger AS $$ BEGIN RETURN NEW; END; $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_probe ON public.rt_probe`,
		`CREATE TRIGGER trg_probe BEFORE INSERT ON public.rt_probe FOR EACH ROW EXECUTE FUNCTION public.trg_probe_fn()`,
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup failed on %q: %v", stmt, err)
		}
	}
}
