package export

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"testing"

	"github.com/pkg/errors"
)

// shardTarget decides which table the parallel path measures: the key it shards
// on, the statistics it reads and the boundary values it samples all have to come
// from the table the query names, which is not always the connection's
// --database. A query that names a schema wins; naming none leaves the old
// behaviour (the connection's own database) untouched.
//
// These tests run against an in-process fake that records the SQL and the
// arguments of every read, because the whole point is what reaches the server:
// an information_schema lookup filtered by the wrong schema finds either nothing
// (the run falls back to single-threaded) or, worse, a same-named table in the
// connection's database, whose key distribution plans ranges for a table nobody
// asked about.

func TestShardTargetRendering(t *testing.T) {
	cases := []struct {
		name     string
		target   shardTarget
		wantFrom string
		wantText string
	}{
		{"bare table", shardTarget{table: "t"}, "`t`", "t"},
		{"qualified table", shardTarget{schema: "other", table: "t"}, "`other`.`t`", "other.t"},
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

// targetCall is one read the shard machinery sent to the server.
type targetCall struct {
	sql  string
	args []driver.Value
}

// targetFake answers every read the parallel path performs — key discovery,
// column types, MIN/MAX, table statistics, boundary probes — and records the SQL
// and the bound arguments of each, so a test can assert on the schema the reads
// were scoped to rather than on an internal call graph.
type targetFake struct {
	pkCols    []string
	ukCols    []string
	colType   string
	nullable  string
	min       interface{}
	max       interface{}
	statsRows int64
	statsAvg  int64
	probes    []interface{}

	calls  []targetCall
	probed int
}

func (f *targetFake) callsFor(needles ...string) []targetCall {
	var out []targetCall
	for _, c := range f.calls {
		match := true
		for _, n := range needles {
			if !strings.Contains(c.sql, n) {
				match = false
				break
			}
		}
		if match {
			out = append(out, c)
		}
	}
	return out
}

// schemaScoped returns every information_schema read the fake saw.
func (f *targetFake) schemaScoped() []targetCall {
	var out []targetCall
	for _, c := range f.calls {
		if strings.Contains(strings.ToUpper(c.sql), "INFORMATION_SCHEMA") {
			out = append(out, c)
		}
	}
	return out
}

type targetConnector struct{ f *targetFake }

func (c targetConnector) Connect(context.Context) (driver.Conn, error) {
	return &targetConn{f: c.f}, nil
}
func (c targetConnector) Driver() driver.Driver { return targetDriver{} }

type targetDriver struct{}

func (targetDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("target fake: dial through the connector")
}

type targetConn struct{ f *targetFake }

func (c *targetConn) Prepare(query string) (driver.Stmt, error) {
	return &targetStmt{f: c.f, query: query}, nil
}
func (c *targetConn) Close() error { return nil }
func (c *targetConn) Begin() (driver.Tx, error) {
	return nil, errors.New("target fake: no transactions")
}

type targetStmt struct {
	f     *targetFake
	query string
}

func (s *targetStmt) Close() error  { return nil }
func (s *targetStmt) NumInput() int { return -1 }

func (s *targetStmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("target fake: exec is not supported")
}

func (s *targetStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.f.calls = append(s.f.calls, targetCall{sql: s.query, args: append([]driver.Value(nil), args...)})
	return s.f.rowsFor(s.query, args)
}

func (f *targetFake) rowsFor(query string, args []driver.Value) (driver.Rows, error) {
	upper := strings.ToUpper(query)
	switch {
	case strings.Contains(upper, "KEY_COLUMN_USAGE") && strings.Contains(query, "CONSTRAINT_NAME = 'PRIMARY'"):
		return &targetRows{cols: []string{"COLUMN_NAME"}, rows: singleColumn(f.pkCols)}, nil
	case strings.Contains(upper, "KEY_COLUMN_USAGE"):
		rows := make([][]driver.Value, 0, len(f.ukCols))
		for _, c := range f.ukCols {
			rows = append(rows, []driver.Value{c, "uk_first"})
		}
		return &targetRows{cols: []string{"COLUMN_NAME", "CONSTRAINT_NAME"}, rows: rows}, nil
	case strings.Contains(upper, "INFORMATION_SCHEMA.COLUMNS"):
		return &targetRows{cols: []string{"DATA_TYPE", "IS_NULLABLE"},
			rows: [][]driver.Value{{f.colType, f.nullable}}}, nil
	case strings.Contains(upper, "TABLE_ROWS"):
		return &targetRows{cols: []string{"TABLE_ROWS", "AVG_ROW_LENGTH"},
			rows: [][]driver.Value{{f.statsRows, f.statsAvg}}}, nil
	case strings.Contains(upper, "MIN("):
		return &targetRows{cols: []string{"MIN", "MAX"},
			rows: [][]driver.Value{{f.min, f.max}}}, nil
	case strings.Contains(query, "LIMIT 1 OFFSET"):
		if f.probed >= len(f.probes) {
			// The estimate was higher than the table: no such row, and the
			// planner stops asking.
			return &targetRows{cols: []string{"key"}}, nil
		}
		v := f.probes[f.probed]
		f.probed++
		return &targetRows{cols: []string{"key"}, rows: [][]driver.Value{{v}}}, nil
	}
	return nil, errors.Errorf("target fake: unexpected query %q", query)
}

func singleColumn(values []string) [][]driver.Value {
	rows := make([][]driver.Value, 0, len(values))
	for _, v := range values {
		rows = append(rows, []driver.Value{v})
	}
	return rows
}

type targetRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *targetRows) Columns() []string { return r.cols }
func (r *targetRows) Close() error      { return nil }

func (r *targetRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

func newTargetDB(t *testing.T, f *targetFake) *sql.DB {
	t.Helper()
	db := sql.OpenDB(targetConnector{f: f})
	t.Cleanup(func() { db.Close() })
	return db
}

// assertSchemaScoped fails if any information_schema read was bound to a schema
// or table other than the target, or lost the schema argument altogether.
func assertSchemaScoped(t *testing.T, f *targetFake, wantSchema, wantTable string) {
	t.Helper()
	reads := f.schemaScoped()
	if len(reads) == 0 {
		t.Fatal("no information_schema read was sent")
	}
	for _, c := range reads {
		if !strings.Contains(c.sql, schemaFilter) {
			t.Errorf("read is not scoped by the query schema: %s", c.sql)
		}
		if len(c.args) < 2 {
			t.Fatalf("read sent %d args, want at least 2 (schema, table): %s", len(c.args), c.sql)
		}
		if got := c.args[0]; got != wantSchema {
			t.Errorf("schema argument = %v, want %q", got, wantSchema)
		}
		if got := c.args[1]; got != wantTable {
			t.Errorf("table argument = %v, want %q", got, wantTable)
		}
	}
}

func baseTargetFake() *targetFake {
	return &targetFake{
		pkCols:    []string{"id"},
		colType:   "bigint",
		min:       int64(1),
		max:       int64(1000),
		statsRows: 1000000,
		statsAvg:  140,
	}
}

// TestDetectShardKey_ReadsTheSchemaTheQueryNamed is the precedence property: a
// query that names a schema has its key, its column type and its MIN/MAX read
// from that schema, not from the connection's --database.
func TestDetectShardKey_ReadsTheSchemaTheQueryNamed(t *testing.T) {
	f := baseTargetFake()
	db := newTargetDB(t, f)

	info, err := DetectShardKey(db, shardTarget{schema: "other", table: "t"}, "")
	if err != nil {
		t.Fatalf("DetectShardKey: %v", err)
	}
	if len(info.Columns) != 1 || info.Columns[0] != "id" {
		t.Errorf("shard key = %v, want [id]", info.Columns)
	}

	assertSchemaScoped(t, f, "other", "t")

	minMax := f.callsFor("SELECT MIN(")
	if len(minMax) != 1 {
		t.Fatalf("MIN/MAX reads = %d, want 1", len(minMax))
	}
	if want := "FROM `other`.`t`"; !strings.Contains(minMax[0].sql, want) {
		t.Errorf("MIN/MAX reads the wrong table: got %s, want it to contain %q", minMax[0].sql, want)
	}
}

// TestDetectShardKey_BareQueryKeepsTheConnectionDatabase pins the unchanged
// half: a query that names no schema still resolves against the connection, so
// every run that worked before this feature works exactly as before.
func TestDetectShardKey_BareQueryKeepsTheConnectionDatabase(t *testing.T) {
	f := baseTargetFake()
	db := newTargetDB(t, f)

	if _, err := DetectShardKey(db, shardTarget{table: "t"}, ""); err != nil {
		t.Fatalf("DetectShardKey: %v", err)
	}

	// The empty schema is what makes COALESCE(NULLIF(?, ''), DATABASE()) fall
	// back to the connection's database; a literal name here would instead read
	// some other schema's key space.
	assertSchemaScoped(t, f, "", "t")

	minMax := f.callsFor("SELECT MIN(")
	if len(minMax) != 1 {
		t.Fatalf("MIN/MAX reads = %d, want 1", len(minMax))
	}
	if want := "FROM `t`"; !strings.Contains(minMax[0].sql, want) {
		t.Errorf("MIN/MAX: got %s, want it to contain %q", minMax[0].sql, want)
	}
	if strings.Contains(minMax[0].sql, "`other`") {
		t.Errorf("unqualified target must not pick up a schema: %s", minMax[0].sql)
	}
}

// TestDetectShardKey_ExplicitShardKeyIsScopedToo: --shard-key skips key
// discovery, but the column lookup it still performs belongs to the named schema.
func TestDetectShardKey_ExplicitShardKeyIsScopedToo(t *testing.T) {
	f := baseTargetFake()
	db := newTargetDB(t, f)

	if _, err := DetectShardKey(db, shardTarget{schema: "other", table: "t"}, "id"); err != nil {
		t.Fatalf("DetectShardKey: %v", err)
	}
	if got := f.callsFor("KEY_COLUMN_USAGE"); len(got) != 0 {
		t.Errorf("explicit shard key still probed for keys: %d reads", len(got))
	}
	assertSchemaScoped(t, f, "other", "t")
}

// TestDetectShardKey_ErrorNamesTheQualifiedTable: the refusal an operator reads
// has to say which table could not be sharded, and for `FROM other.t` that is
// not a table in the connection's database.
func TestDetectShardKey_ErrorNamesTheQualifiedTable(t *testing.T) {
	f := baseTargetFake()
	f.pkCols = nil
	f.ukCols = nil
	db := newTargetDB(t, f)

	_, err := DetectShardKey(db, shardTarget{schema: "other", table: "t"}, "")
	if err == nil {
		t.Fatal("DetectShardKey succeeded without any key")
	}
	if !strings.Contains(err.Error(), "other.t") {
		t.Errorf("error = %q, want it to name other.t", err.Error())
	}
}

// TestPlanShardRanges_ProbesTheNamedSchema: the boundaries come from index reads
// against the target table. Sampling the connection's same-named table instead
// would produce ranges that do not cover the rows the query returns.
func TestPlanShardRanges_ProbesTheNamedSchema(t *testing.T) {
	f := baseTargetFake()
	// Sampling only runs once one index pass is a rounding error against the dump
	// it is balancing, so the fake reports rows wide enough to clear that bar.
	f.probes = []interface{}{int64(300), int64(700)}
	f.statsAvg = 1024
	db := newTargetDB(t, f)

	info := numericKeyInfo(int64(1), int64(1000))
	ranges := planShardRanges(db, shardTarget{schema: "other", table: "t"}, info, 4)

	assertSchemaScoped(t, f, "other", "t")

	probes := f.callsFor("LIMIT 1 OFFSET")
	if len(probes) == 0 {
		t.Fatal("no boundary probe was sent")
	}
	for _, p := range probes {
		if want := "FROM `other`.`t`"; !strings.Contains(p.sql, want) {
			t.Errorf("probe reads the wrong table: got %s, want it to contain %q", p.sql, want)
		}
	}
	// Sampled boundaries, not the equal-width ones: 1..300 / 300..700 / 700..1000
	// plus nothing else, so three ranges.
	if len(ranges) != 3 {
		t.Errorf("ranges = %d, want 3 (%+v)", len(ranges), ranges)
	}
}
