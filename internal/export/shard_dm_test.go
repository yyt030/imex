package export

import (
	"strings"
	"testing"

	"github.com/imex/internal/backend"
)

var dmShardBackend = backend.Must(backend.EngineDM)

func dmTarget(schema, table string) shardTarget {
	return shardTarget{schema: schema, table: table, be: dmShardBackend}
}

// TestShardDialectIsChosenForDameng is the face-⑤ half of the Dameng parallel
// export increment: the MySQL and PostgreSQL statements are untouched bytes, and
// what a Dameng run now sends is a real Oracle-mode catalogue read (ALL_* views,
// `?` placeholders, an owner predicate that survives the identifier case the
// server folds to) rather than a translated copy of the MySQL queries, which a
// DM8 server rejects outright.
func TestShardDialectIsChosenForDameng(t *testing.T) {
	mysql := shardDialectFor(backend.Must(backend.EngineMySQL))
	dm := shardDialectFor(dmShardBackend)

	for name, q := range map[string]string{
		"primary key": dm.primaryKey,
		"unique key":  dm.uniqueKey,
		"column type": dm.columnType,
		"table stats": dm.tableStats,
	} {
		if strings.Contains(q, "$1") {
			t.Errorf("dm %s uses a PostgreSQL ordinal placeholder", name)
		}
		if strings.Contains(q, "`") {
			t.Errorf("dm %s uses MySQL backticks", name)
		}
		if !strings.Contains(q, "COALESCE(NULLIF(?, ''), SYS_CONTEXT('USERENV','CURRENT_SCHEMA'))") {
			t.Errorf("dm %s must fall back to the session schema for an empty --database: %s", name, q)
		}
	}

	if dm.primaryKey == mysql.primaryKey || dm.tableStats == mysql.tableStats {
		t.Error("dm must not keep the MySQL shard statements")
	}
	if !strings.Contains(dm.primaryKey, "ALL_CONS_COLUMNS") || !strings.Contains(dm.primaryKey, "K.CONSTRAINT_TYPE = 'P'") {
		t.Error("dm primary key lookup must read ALL_CONSTRAINTS for a 'P' constraint")
	}
	if !strings.Contains(dm.primaryKey, "ORDER BY C.POSITION") {
		t.Error("dm primary key lookup must preserve the key column order like MySQL's ORDINAL_POSITION")
	}
	if !strings.Contains(dm.uniqueKey, "K.CONSTRAINT_TYPE = 'U'") || !strings.Contains(dm.uniqueKey, "C.CONSTRAINT_NAME") {
		t.Error("dm unique key lookup must read 'U' constraints and name the first one")
	}
	// NULLABLE answers 'Y'/'N' on Dameng. getShardInfo compares the second column
	// against "YES", so an unmapped read would mark every shard key NOT NULL and
	// the dump would silently lose the rows whose key is NULL.
	if !strings.Contains(dm.columnType, "ALL_TAB_COLUMNS") || !strings.Contains(dm.columnType, "CASE NULLABLE WHEN 'Y' THEN 'YES' ELSE 'NO' END") {
		t.Error("dm column type read must fold NULLABLE 'Y'/'N' into the YES/NO answer getShardInfo compares")
	}
	if !strings.Contains(dm.columnType, "UPPER(COLUMN_NAME) = UPPER(?)") {
		t.Error("dm column type read must bind the column name through UPPER like the other Dameng catalogue reads")
	}
	// AVG_ROW_LEN is measured as -1 on DM8 even after statistics gathering, so the
	// average row length comes from the page count, the way the PostgreSQL read
	// derives it from relpages.
	if strings.Contains(dm.tableStats, "AVG_ROW_LEN") {
		t.Error("dm table stats must not read AVG_ROW_LEN: measured -1 on DM8 after SP_TAB_STAT_INIT")
	}
	if !strings.Contains(dm.tableStats, "NUM_ROWS") || !strings.Contains(dm.tableStats, "BLOCKS") || !strings.Contains(dm.tableStats, "SF_GET_PAGE_SIZE()") {
		t.Error("dm table stats must derive the average row length from BLOCKS and the page size")
	}
}

// TestShardTargetDamengRendering pins the identifier spelling the data reads use.
// Oracle-mode DM has no backticks, so a MySQL-style quote is a server syntax
// error rather than a cosmetic difference.
func TestShardTargetDamengRendering(t *testing.T) {
	cases := []struct {
		name     string
		target   shardTarget
		wantFrom string
		wantText string
	}{
		{"bare table", dmTarget("", "DMPAR169"), `"DMPAR169"`, "DMPAR169"},
		{"qualified table", dmTarget("IMEXIT", "DMPAR169"), `"IMEXIT"."DMPAR169"`, "IMEXIT.DMPAR169"},
		{"embedded double quote", shardTarget{table: `We"ird`, be: dmShardBackend}, `"We""ird"`, `We"ird`},
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

// TestDetectShardKey_DamengCatalogue drives key discovery against the in-process
// fake and asserts what reaches a DM8 server: the key from ALL_CONS_COLUMNS, the
// type from ALL_TAB_COLUMNS, and a double-quoted MIN/MAX read on the schema the
// query named. The column name is what Oracle-mode DM reports for an unquoted
// table — upper case — and it is what the shard condition then carries.
func TestDetectShardKey_DamengCatalogue(t *testing.T) {
	f := baseTargetFake()
	f.pkCols = []string{"ID"}
	f.colType = "BIGINT" // what DM8's ALL_TAB_COLUMNS reports for a BIGINT column
	f.nullable = "NO"
	db := newTargetDB(t, f)

	info, err := DetectShardKey(db, dmShardBackend, dmTarget("IMEXIT", "DMPAR169"), "")
	if err != nil {
		t.Fatalf("DetectShardKey on dm: %v", err)
	}
	if len(info.Columns) != 1 || info.Columns[0] != "ID" {
		t.Errorf("shard key = %v, want [ID]", info.Columns)
	}
	if !info.IsNumeric[0] {
		t.Error("Dameng 'BIGINT' must classify as a numeric shard key")
	}
	if info.Nullable[0] {
		t.Error("a NOT NULL column must not read as nullable")
	}

	for _, c := range f.calls {
		upper := strings.ToUpper(c.sql)
		if strings.Contains(upper, "MIN(") || strings.Contains(c.sql, "LIMIT 1 OFFSET") {
			continue // data reads, not catalogue reads
		}
		if !strings.Contains(upper, "ALL_CONS_COLUMNS") && !strings.Contains(upper, "ALL_TAB_COLUMNS") {
			t.Errorf("catalogue read is not a Dameng ALL_* view: %s", c.sql)
		}
		if len(c.args) < 2 || c.args[0] != "IMEXIT" || c.args[1] != "DMPAR169" {
			t.Errorf("catalogue read not scoped to IMEXIT.DMPAR169: args=%v sql=%s", c.args, c.sql)
		}
	}
	if got := f.callsFor("ALL_CONS_COLUMNS", "CONSTRAINT_TYPE = 'P'"); len(got) != 1 {
		t.Errorf("primary key reads = %d, want 1 from ALL_CONS_COLUMNS", len(got))
	}
	// The type read carries a third bind: the column name, in the same order the
	// MySQL and PostgreSQL reads do.
	types := f.callsFor("ALL_TAB_COLUMNS")
	if len(types) != 1 {
		t.Fatalf("column type reads = %d, want 1", len(types))
	}
	if len(types[0].args) != 3 || types[0].args[2] != "ID" {
		t.Errorf("column type read bound %v, want [IMEXIT DMPAR169 ID]", types[0].args)
	}

	minMax := f.callsFor("SELECT MIN(")
	if len(minMax) != 1 {
		t.Fatalf("MIN/MAX reads = %d, want 1", len(minMax))
	}
	for _, want := range []string{`MIN("ID")`, `MAX("ID")`, `FROM "IMEXIT"."DMPAR169"`} {
		if !strings.Contains(minMax[0].sql, want) {
			t.Errorf("MIN/MAX read missing %q: %s", want, minMax[0].sql)
		}
	}
	if strings.Contains(minMax[0].sql, "`") {
		t.Error("MySQL backticks leaked into a Dameng data read")
	}
}

// TestDetectShardKey_DamengNullableKey is the row-loss guard: Dameng reports
// NULLABLE as 'Y', the statement folds it to 'YES', and a nullable single-column
// key must therefore gain the IS NULL shard. Read the raw 'Y' instead and every
// shard compares against a value a NULL can never match, so those rows leave no
// trace in the dump.
func TestDetectShardKey_DamengNullableKey(t *testing.T) {
	f := baseTargetFake()
	f.pkCols = nil
	f.ukCols = []string{"K"}
	f.colType = "INT"
	f.nullable = "YES"
	db := newTargetDB(t, f)

	info, err := DetectShardKey(db, dmShardBackend, dmTarget("IMEXIT", "DMPAR169"), "")
	if err != nil {
		t.Fatalf("DetectShardKey on a unique key: %v", err)
	}
	if len(info.Columns) != 1 || info.Columns[0] != "K" {
		t.Fatalf("shard key = %v, want [K] from the 'U' constraint read", info.Columns)
	}
	if len(f.callsFor("CONSTRAINT_TYPE = 'U'")) != 1 {
		t.Error("a table without a primary key must reach the Dameng unique constraint read")
	}
	if !info.Nullable[0] {
		t.Fatal("NULLABLE folded to 'YES' must read as nullable")
	}
	ranges := GenerateShardRanges(info, 2)
	if ranges == nil {
		t.Fatal("a nullable single-column key must still shard")
	}
	if !ranges[len(ranges)-1].NullOnly {
		t.Errorf("last range must be the IS NULL shard, got %+v", ranges[len(ranges)-1])
	}
}

// TestPlanShardRanges_DamengProbe pins the two remaining surfaces of the
// parallel path: the row estimate comes from ALL_TABLES with the average row
// length derived from the page count, and every boundary probe is a Dameng
// statement — `?` placeholder, double-quoted identifiers, and the LIMIT/OFFSET
// spelling DM8 answers (measured: the OFFSET is 0-based, same as MySQL's).
func TestPlanShardRanges_DamengProbe(t *testing.T) {
	f := baseTargetFake()
	f.probes = []interface{}{int64(300), int64(700)}
	f.statsAvg = 1024
	db := newTargetDB(t, f)

	info := numericKeyInfo(int64(1), int64(1000))
	ranges := planShardRanges(db, dmTarget("IMEXIT", "DMPAR169"), info, 4)
	if len(ranges) != 3 {
		t.Fatalf("ranges = %d, want 3 (%+v)", len(ranges), ranges)
	}
	if got := f.callsFor("ALL_TABLES", "NUM_ROWS"); len(got) != 1 {
		t.Errorf("row estimate reads = %d, want 1 from ALL_TABLES", len(got))
	}

	probes := f.callsFor("LIMIT 1 OFFSET")
	if len(probes) != 3 {
		t.Fatalf("boundary probes = %d, want 3", len(probes))
	}
	for i, p := range probes {
		for _, want := range []string{`SELECT "id"`, `FROM "IMEXIT"."DMPAR169"`, `"id" > ?`, `ORDER BY "id"`, "LIMIT 1 OFFSET"} {
			if !strings.Contains(p.sql, want) {
				t.Errorf("probe %d missing %q: %s", i, want, p.sql)
			}
		}
		if strings.Contains(p.sql, "`") || strings.Contains(p.sql, "$1") {
			t.Errorf("probe %d is not a Dameng statement: %s", i, p.sql)
		}
		if len(p.args) != 1 {
			t.Errorf("probe %d bound %d args, want 1", i, len(p.args))
		}
	}
}
