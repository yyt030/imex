package export

import (
	"strings"
	"testing"

	"github.com/imex/internal/backend"
)

var pgShardBackend = backend.Must(backend.EnginePG)

func pgTarget(schema, table string) shardTarget {
	return shardTarget{schema: schema, table: table, be: pgShardBackend}
}

// TestShardDialectIsChosenByEngine is the face-⑤ half of the PostgreSQL
// parallel export increment: the MySQL statements are unchanged byte for byte,
// and the PostgreSQL ones are a real catalogue dialect (pg_catalog, ordinal
// placeholders, current_schema fallback) rather than a translated copy of the
// MySQL queries that would fail on a server.
func TestShardDialectIsChosenByEngine(t *testing.T) {
	mysql := shardDialectFor(backend.Must(backend.EngineMySQL))
	pg := shardDialectFor(pgShardBackend)

	allMySQL := mysql.primaryKey + mysql.uniqueKey + mysql.columnType + mysql.tableStats
	for _, want := range []string{
		"INFORMATION_SCHEMA.KEY_COLUMN_USAGE", schemaFilter, "CONSTRAINT_NAME = 'PRIMARY'",
		"REFERENCED_TABLE_NAME IS NULL", "INFORMATION_SCHEMA.COLUMNS", "TABLE_ROWS", "AVG_ROW_LENGTH",
	} {
		if !strings.Contains(allMySQL, want) {
			t.Errorf("mysql shard dialect lost %q (contract face ⑤)", want)
		}
	}

	for name, q := range map[string]string{
		"primary key": pg.primaryKey,
		"unique key":  pg.uniqueKey,
		"column type": pg.columnType,
		"table stats": pg.tableStats,
	} {
		if strings.Contains(q, "?") {
			t.Errorf("pg %s still uses a ? placeholder", name)
		}
		if !strings.Contains(q, "$1") || !strings.Contains(q, "$2") {
			t.Errorf("pg %s must bind the schema as $1 and the table as $2", name)
		}
		if !strings.Contains(q, "current_schema()") {
			t.Errorf("pg %s must fall back to current_schema() for an empty schema", name)
		}
	}
	if !strings.Contains(pg.columnType, "$3") {
		t.Error("pg column type lookup must bind the column name as $3")
	}
	if !strings.Contains(pg.primaryKey, "pg_catalog.pg_constraint") || !strings.Contains(pg.primaryKey, "con.contype = 'p'") {
		t.Error("pg primary key lookup must read pg_constraint for a primary constraint")
	}
	if !strings.Contains(pg.primaryKey, "WITH ORDINALITY") {
		t.Error("pg primary key lookup must preserve conkey order like MySQL's ORDINAL_POSITION")
	}
	if !strings.Contains(pg.uniqueKey, "con.contype = 'u'") || !strings.Contains(pg.uniqueKey, "con.conname") {
		t.Error("pg unique key lookup must read unique constraints and name the first one")
	}
	if !strings.Contains(pg.tableStats, "reltuples") || !strings.Contains(pg.tableStats, "relpages") || !strings.Contains(pg.tableStats, "::bigint") {
		t.Error("pg row estimate must come from pg_class statistics cast to bigint")
	}

	// Dameng refuses --threads > 1 before this path, so it keeps the MySQL
	// statements; the Gauss spike inherits PostgreSQL's refusals and protocol and
	// must therefore inherit its sharding dialect too.
	if got := shardDialectFor(backend.Must(backend.EngineDM)).primaryKey; got != mysql.primaryKey {
		t.Error("dm shard dialect must stay MySQL")
	}
	if got := shardDialectFor(backend.Must(backend.EngineGauss)).primaryKey; got != pg.primaryKey {
		t.Error("gauss must inherit the PostgreSQL shard dialect")
	}
}

// TestShardTargetPGRendering pins the identifier spelling the data reads use.
// PostgreSQL double-quotes and folds unquoted names, so a backtick here would be
// a syntax error on the server rather than a cosmetic difference.
func TestShardTargetPGRendering(t *testing.T) {
	cases := []struct {
		name     string
		target   shardTarget
		wantFrom string
		wantText string
	}{
		{"bare table", pgTarget("", "t"), `"t"`, "t"},
		{"qualified table", pgTarget("app", "t"), `"app"."t"`, "app.t"},
		{"embedded double quote", shardTarget{table: `we"ird`, be: pgShardBackend}, `"we""ird"`, `we"ird`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.target.source(); got != tc.wantFrom {
				t.Errorf("source() = %q, want %q", got, tc.wantFrom)
			}
			if got := tc.target.display(); got != tc.wantText {
				t.Errorf("display() = %q, want %q", got, tc.wantText)
			}
		})
	}
}

// TestDetectShardKey_PostgresCatalogue drives the whole key-discovery path
// against the in-process fake and asserts the statements that reach the server:
// pg_constraint for the key, information_schema for the column type, and a
// double-quoted MIN/MAX data read — all scoped to the schema the query named.
func TestDetectShardKey_PostgresCatalogue(t *testing.T) {
	f := baseTargetFake()
	f.colType = "integer" // what PostgreSQL's information_schema.columns reports
	db := newTargetDB(t, f)

	info, err := DetectShardKey(db, pgShardBackend, pgTarget("app", "t"), "")
	if err != nil {
		t.Fatalf("DetectShardKey on pg: %v", err)
	}
	if len(info.Columns) != 1 || info.Columns[0] != "id" {
		t.Errorf("shard key = %v, want [id]", info.Columns)
	}
	if !info.IsNumeric[0] {
		t.Error("PostgreSQL 'integer' must classify as a numeric shard key")
	}

	for _, c := range f.calls {
		upper := strings.ToUpper(c.sql)
		if strings.Contains(upper, "MIN(") || strings.Contains(c.sql, "LIMIT 1 OFFSET") {
			continue // data reads, not catalogue reads
		}
		if len(c.args) < 2 || c.args[0] != "app" || c.args[1] != "t" {
			t.Errorf("catalogue read not scoped to app.t: args=%v sql=%s", c.args, c.sql)
		}
		if !strings.Contains(c.sql, "$1") || !strings.Contains(c.sql, "$2") {
			t.Errorf("catalogue read is not PostgreSQL: %s", c.sql)
		}
	}
	if got := f.callsFor("pg_catalog.pg_constraint"); len(got) != 1 {
		t.Errorf("primary key reads = %d, want 1 from pg_constraint", len(got))
	}
	if got := f.callsFor("information_schema.columns"); len(got) != 1 {
		t.Errorf("column type reads = %d, want 1 from information_schema.columns", len(got))
	}

	minMax := f.callsFor("SELECT MIN(")
	if len(minMax) != 1 {
		t.Fatalf("MIN/MAX reads = %d, want 1", len(minMax))
	}
	for _, want := range []string{`MIN("id")`, `MAX("id")`, `FROM "app"."t"`} {
		if !strings.Contains(minMax[0].sql, want) {
			t.Errorf("MIN/MAX read missing %q: %s", want, minMax[0].sql)
		}
	}
	if strings.Contains(minMax[0].sql, "`") {
		t.Error("MySQL backticks leaked into a PostgreSQL data read")
	}
}

// TestPlanShardRanges_PostgresProbe pins the two remaining dialect surfaces of
// the parallel path: the row estimate comes from pg_class, and every boundary
// probe uses $1 plus double-quoted identifiers on the named schema.
func TestPlanShardRanges_PostgresProbe(t *testing.T) {
	f := baseTargetFake()
	f.probes = []interface{}{int64(300), int64(700)}
	f.statsAvg = 1024 // large enough that sampling pays for itself
	db := newTargetDB(t, f)

	info := numericKeyInfo(int64(1), int64(1000))
	ranges := planShardRanges(db, pgTarget("app", "t"), info, 4)
	if len(ranges) != 3 {
		t.Fatalf("ranges = %d, want 3 (%+v)", len(ranges), ranges)
	}
	if got := f.callsFor("reltuples"); len(got) != 1 {
		t.Errorf("row estimate reads = %d, want 1 from pg_class.reltuples", len(got))
	}

	probes := f.callsFor("LIMIT 1 OFFSET")
	if len(probes) != 3 {
		t.Fatalf("boundary probes = %d, want 3", len(probes))
	}
	for i, p := range probes {
		for _, want := range []string{`SELECT "id"`, `FROM "app"."t"`, `"id" > $1`, `ORDER BY "id"`, "LIMIT 1 OFFSET"} {
			if !strings.Contains(p.sql, want) {
				t.Errorf("probe %d missing %q: %s", i, want, p.sql)
			}
		}
		if strings.Contains(p.sql, "`") {
			t.Errorf("probe %d used MySQL backticks: %s", i, p.sql)
		}
		if len(p.args) != 1 {
			t.Errorf("probe %d bound %d args, want 1", i, len(p.args))
		}
	}
}
