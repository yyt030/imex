package export

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/pkg/errors"
)

// sampleFake is an in-process stand-in for the two server reads sampling does:
// the statistics lookup and the boundary probes. It answers by query shape, so a
// test can tell which path planShardRanges took from the SQL it actually sent,
// and it scripts the probe results so a run never needs a live table to exercise
// the interesting cases (fewer values than planned boundaries, a rejected probe,
// a duplicate boundary).
type sampleFake struct {
	statsRows int64
	statsAvg  int64
	statsErr  bool
	probeErr  error

	// probes holds the values the server would hand back, in order; once it is
	// drained every further probe returns no rows.
	probes []interface{}

	queries    []string
	probeCalls [][]driver.Value
}

func (f *sampleFake) probeValue() (interface{}, bool) {
	if len(f.probes) == 0 {
		return nil, false
	}
	v := f.probes[0]
	f.probes = f.probes[1:]
	return v, true
}

// probeArgs returns the argument lists the probes were sent with, in order.
func (f *sampleFake) probeArgs() [][]driver.Value { return f.probeCalls }

func (f *sampleFake) probeQueries() []string {
	var out []string
	for _, q := range f.queries {
		if !strings.Contains(q, "TABLE_ROWS") {
			out = append(out, q)
		}
	}
	return out
}

type sampleConnector struct{ f *sampleFake }

func (c sampleConnector) Connect(context.Context) (driver.Conn, error) {
	return &sampleConn{f: c.f}, nil
}
func (c sampleConnector) Driver() driver.Driver { return sampleDriver{} }

type sampleDriver struct{}

func (sampleDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("sample fake: dial through the connector")
}

type sampleConn struct{ f *sampleFake }

func (c *sampleConn) Prepare(query string) (driver.Stmt, error) {
	return &sampleStmt{f: c.f, query: query}, nil
}
func (c *sampleConn) Close() error { return nil }
func (c *sampleConn) Begin() (driver.Tx, error) {
	return nil, errors.New("sample fake: no transactions")
}

type sampleStmt struct {
	f     *sampleFake
	query string
}

func (s *sampleStmt) Close() error  { return nil }
func (s *sampleStmt) NumInput() int { return -1 }

func (s *sampleStmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("sample fake: exec is not supported")
}

func (s *sampleStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.f.queries = append(s.f.queries, s.query)

	if strings.Contains(s.query, "TABLE_ROWS") {
		if s.f.statsErr {
			return nil, errors.New("sample fake: statistics lookup failed")
		}
		return &sampleRows{cols: []string{"TABLE_ROWS", "AVG_ROW_LENGTH"},
			row: []driver.Value{s.f.statsRows, s.f.statsAvg}}, nil
	}

	s.f.probeCalls = append(s.f.probeCalls, args)

	if s.f.probeErr != nil {
		return nil, s.f.probeErr
	}
	v, ok := s.f.probeValue()
	if !ok {
		return &sampleRows{}, nil
	}
	return &sampleRows{cols: []string{"key"}, row: []driver.Value{v}}, nil
}

type sampleRows struct {
	cols []string
	row  []driver.Value
	done bool
}

func (r *sampleRows) Columns() []string {
	if len(r.cols) == 0 {
		return []string{"key"}
	}
	return r.cols
}
func (r *sampleRows) Close() error { return nil }

func (r *sampleRows) Next(dest []driver.Value) error {
	if r.done || len(r.row) == 0 {
		return io.EOF
	}
	r.done = true
	copy(dest, r.row)
	return nil
}

func newSampleDB(t *testing.T, f *sampleFake) *sql.DB {
	t.Helper()
	db := sql.OpenDB(sampleConnector{f: f})
	t.Cleanup(func() { db.Close() })
	return db
}

func keyInfo(dataType string, numeric bool, min, max interface{}) *ShardInfo {
	return &ShardInfo{
		Columns:     []string{"id"},
		ColumnTypes: []string{dataType},
		Mins:        []interface{}{min},
		Maxs:        []interface{}{max},
		IsNumeric:   []bool{numeric},
	}
}

func numericKeyInfo(min, max interface{}) *ShardInfo { return keyInfo("bigint", true, min, max) }

func textKeyInfo(min, max string) *ShardInfo {
	return &ShardInfo{
		Columns:     []string{"k"},
		ColumnTypes: []string{"char"},
		Mins:        []interface{}{[]byte(min)},
		Maxs:        []interface{}{[]byte(max)},
		IsNumeric:   []bool{false},
	}
}

// boundText renders a boundary the way the query builder sees it. Values arrive
// as []byte over the text protocol, so tests must compare text, not pointers.
func boundText(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(x)
	case string:
		return x
	case shardBoundary:
		return string(x)
	default:
		return fmt.Sprintf("%v", x)
	}
}

func TestShardRangeCount(t *testing.T) {
	cases := []struct {
		threads int
		want    int
	}{
		{1, 2},
		{2, 4},
		{4, 8},
		{32, 64},
		{256, maxShardRanges},
		{0, 2},
	}
	for _, tc := range cases {
		if got := shardRangeCount(tc.threads); got != tc.want {
			t.Errorf("shardRangeCount(%d) = %d, want %d", tc.threads, got, tc.want)
		}
	}
}

func TestIsIntegerKeyType(t *testing.T) {
	for _, dt := range []string{"int", "INT", "bigint", "tinyint", "mediumint", "smallint", "integer"} {
		if !isIntegerKeyType(dt) {
			t.Errorf("isIntegerKeyType(%q) = false, want true", dt)
		}
	}
	for _, dt := range []string{"decimal", "numeric", "float", "double", "char", "varchar"} {
		if isIntegerKeyType(dt) {
			t.Errorf("isIntegerKeyType(%q) = true, want false", dt)
		}
	}
}

func TestDenseEnoughForEqualWidth(t *testing.T) {
	cases := []struct {
		name      string
		info      *ShardInfo
		rows      int64
		wantDense bool
	}{
		{"one value per row", numericKeyInfo(int64(1), int64(1000000)), 1000000, true},
		{"span is 1.5x the row count", numericKeyInfo(int64(1), int64(1500000)), 1000000, true},
		{"sparse key leaves empty stretches", numericKeyInfo(int64(1), int64(200000000)), 1000000, false},
		{"duplicate-heavy key", numericKeyInfo(int64(1), int64(100)), 1000000, true},
		{"no rows estimated", numericKeyInfo(int64(1), int64(10)), 0, false},
		{"non-numeric key has no span to read", textKeyInfo("0000", "ffff"), 1000, false},
		{
			// A BIGINT at the top of the domain must not be read through float64:
			// 2^62 minus 1 loses integer precision and would flip the answer.
			name:      "huge key span",
			info:      numericKeyInfo("1", "4611686018427387904"),
			rows:      1000000,
			wantDense: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := denseEnoughForEqualWidth(tc.info, tc.rows); got != tc.wantDense {
				t.Errorf("denseEnoughForEqualWidth = %v, want %v", got, tc.wantDense)
			}
		})
	}
}

// TestSamplingWorthwhile pins the two questions the cost check answers: can an
// equal-width split be trusted at all, and is one more index pass money back.
func TestSamplingWorthwhile(t *testing.T) {
	sparse := numericKeyInfo(int64(1), int64(200000000))
	dense := numericKeyInfo(int64(1), int64(1000000))
	decimal := keyInfo("decimal", true, "1.5", "9.5")
	cases := []struct {
		name      string
		info      *ShardInfo
		rows      int64
		avgRowLen int64
		want      bool
	}{
		{"text key is always sampled", textKeyInfo("a", "z"), 100, 100, true},
		{"continuous key is always sampled", decimal, 100, 100, true},
		{"sparse integer key is sampled", sparse, 1000000, 40, true},
		{"dense small dump is left alone", dense, 1000000, 33, false},
		{"dense large dump is sampled", dense, 10000000, 33, true},
		{"unknown row length keeps the cheap split", dense, 1000000, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := samplingWorthwhile(tc.info, tc.rows, tc.avgRowLen); got != tc.want {
				t.Errorf("samplingWorthwhile = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProbeArg(t *testing.T) {
	if got := probeArg(shardBoundary("42"), true); got != int64(42) {
		t.Errorf("probeArg integer boundary = %#v, want int64(42)", got)
	}
	if got := probeArg([]byte("42"), true); got != int64(42) {
		t.Errorf("probeArg []byte boundary = %#v, want int64(42)", got)
	}
	// Beyond signed range (BIGINT UNSIGNED): the exact text is what must go out.
	if got := probeArg(shardBoundary("18446744073709551615"), true); got != "18446744073709551615" {
		t.Errorf("probeArg out-of-range = %#v, want the exact text", got)
	}
	if got := probeArg([]byte("abc"), false); got != "abc" {
		t.Errorf("probeArg text key = %#v, want \"abc\"", got)
	}
	// A decimal boundary stays text: converting it to float64 would lose
	// precision, and the server compares it as a number whatever the driver sends.
	if got := probeArg(shardBoundary("1.25"), true); got != "1.25" {
		t.Errorf("probeArg decimal = %#v, want \"1.25\"", got)
	}
}

// TestPlanShardRangesTextKey is the sharding gap this closed: a CHAR or VARCHAR
// key has no numeric boundaries to cut, so it used to collapse to one range and
// export single-threaded whatever --threads said.
func TestPlanShardRangesTextKey(t *testing.T) {
	f := &sampleFake{
		statsRows: 500000,
		statsAvg:  200,
		probes:    []interface{}{[]byte("5555"), []byte("aaaa"), []byte("eeee")},
	}
	db := newSampleDB(t, f)

	info := textKeyInfo("0000", "ffff")
	ranges := planShardRanges(db, "bench_str2", info, 8)

	// Three values came back, so three internal boundaries: four ranges, and the
	// run stops asking once the server says there is no such row.
	if len(ranges) != 4 {
		t.Fatalf("ranges = %d, want 4 (%+v)", len(ranges), ranges)
	}
	if got := boundText(ranges[0].Mins[0]); got != "0000" {
		t.Errorf("first range starts at %q, want the key MIN", got)
	}
	for i := 1; i < len(ranges); i++ {
		if boundText(ranges[i-1].Maxs[0]) != boundText(ranges[i].Mins[0]) {
			t.Fatalf("range %d does not continue range %d: %v then %v",
				i, i-1, ranges[i-1].Maxs[0], ranges[i].Mins[0])
		}
	}
	last := ranges[len(ranges)-1]
	if !last.IsLast {
		t.Error("last range must be the inclusive one, or the highest keys are dropped")
	}
	if got := boundText(last.Maxs[0]); got != "ffff" {
		t.Errorf("last range ends at %q, want the key MAX", got)
	}

	qs := f.probeQueries()
	if len(qs) != 4 {
		t.Errorf("probes sent = %d, want 4 (three values plus the one that came back empty)", len(qs))
	}
	for _, q := range qs {
		for _, want := range []string{"`k` > ?", "FROM `bench_str2`", "ORDER BY `k`", "LIMIT 1 OFFSET "} {
			if !strings.Contains(q, want) {
				t.Errorf("probe %q missing %q", q, want)
			}
		}
	}
	args := f.probeArgs()
	if len(args) != 4 {
		t.Fatalf("probe argument lists = %d, want 4", len(args))
	}
	// Every boundary is bound, never interpolated, and every probe seeks from the
	// boundary before it: that is what keeps a wrong row estimate from opening a
	// hole in the cover.
	for i, a := range args {
		if len(a) != 1 {
			t.Fatalf("probe %d carried %d arguments, want 1", i, len(a))
		}
	}
	if got := fmt.Sprintf("%v", args[0][0]); got != "0000" {
		t.Errorf("first probe sought from %q, want the key MIN", got)
	}
	if got := fmt.Sprintf("%v", args[1][0]); got != "5555" {
		t.Errorf("second probe sought from %q, want the first sampled value", got)
	}
}

// TestPlanShardRangesSkipsDuplicateBoundaries: a value carried by more rows than
// one range can hold is one boundary, not two, or the run reads an empty range.
func TestPlanShardRangesSkipsDuplicateBoundaries(t *testing.T) {
	f := &sampleFake{
		statsRows: 500000,
		statsAvg:  200,
		probes:    []interface{}{[]byte("aaaa"), []byte("aaaa"), []byte("eeee")},
	}
	db := newSampleDB(t, f)

	ranges := planShardRanges(db, "bench_str2", textKeyInfo("0000", "ffff"), 8)
	if len(ranges) != 3 {
		t.Fatalf("ranges = %d, want 3 (%+v)", len(ranges), ranges)
	}
	if boundText(ranges[1].Maxs[0]) != "eeee" {
		t.Errorf("second range ends at %q, want the third distinct sampled value", ranges[1].Maxs[0])
	}
}

// TestPlanShardRangesKeepsEqualWidthForADenseSmallKey pins the no-overhead case:
// a dense integer key on a table whose dump is small must not pay an index pass,
// and must produce exactly the ranges it always did.
func TestPlanShardRangesKeepsEqualWidthForADenseSmallKey(t *testing.T) {
	f := &sampleFake{statsRows: 1000000, statsAvg: 33}
	db := newSampleDB(t, f)

	info := numericKeyInfo(int64(1), int64(1000000))
	got := planShardRanges(db, "bench_1m", info, 8)
	want := GenerateShardRanges(info, 8)

	if len(got) != len(want) {
		t.Fatalf("ranges = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if boundText(got[i].Mins[0]) != boundText(want[i].Mins[0]) ||
			boundText(got[i].Maxs[0]) != boundText(want[i].Maxs[0]) {
			t.Fatalf("range %d = %v..%v, want %v..%v", i,
				got[i].Mins[0], got[i].Maxs[0], want[i].Mins[0], want[i].Maxs[0])
		}
	}
	if len(f.probeQueries()) != 0 {
		t.Errorf("dense small table was probed anyway: %q", f.probeQueries())
	}
}

// TestPlanShardRangesSparseIntegerKey is the imbalance case equal-width cannot
// see: the value domain says the rows are spread over 200M keys, the table keeps
// them in two clusters, and four fifths of the rows sit in one range.
func TestPlanShardRangesSparseIntegerKey(t *testing.T) {
	f := &sampleFake{
		statsRows: 1000000,
		statsAvg:  140,
		probes: []interface{}{
			[]byte("125000"), []byte("200000"), []byte("199325001"),
			[]byte("199450001"), []byte("199575001"), []byte("199700001"),
			[]byte("199825001"),
		},
	}
	db := newSampleDB(t, f)

	info := numericKeyInfo(int64(1), int64(200000000))
	ranges := planShardRanges(db, "bench_skew2", info, 8)
	if len(ranges) != 8 {
		t.Fatalf("ranges = %d, want 8", len(ranges))
	}
	if got := boundText(ranges[0].Maxs[0]); got != "125000" {
		t.Errorf("first boundary = %q, want the sampled value 125000", got)
	}
	for i := 1; i < len(ranges); i++ {
		if boundText(ranges[i-1].Maxs[0]) != boundText(ranges[i].Mins[0]) {
			t.Fatalf("ranges %d and %d do not meet: %v | %v", i-1, i,
				ranges[i-1].Maxs[0], ranges[i].Mins[0])
		}
	}
	if !ranges[7].IsLast || boundText(ranges[7].Maxs[0]) != "200000000" {
		t.Errorf("last range = %v (IsLast=%v), want up to the real MAX inclusive",
			ranges[7].Maxs[0], ranges[7].IsLast)
	}
	// Integer boundaries are bound as integers, so the optimiser keeps a numeric
	// range on a numeric column instead of comparing against a string.
	args := f.probeArgs()
	if len(args) != 7 {
		t.Fatalf("probe argument lists = %d, want 7", len(args))
	}
	if v, ok := args[0][0].(int64); !ok || v != 1 {
		t.Errorf("first probe sought from %#v, want int64(1)", args[0][0])
	}
	if v, ok := args[6][0].(int64); !ok || v != 199700001 {
		t.Errorf("last probe sought from %#v, want int64(199700001)", args[6][0])
	}
}

func TestPlanShardRangesFallsBackToEqualWidth(t *testing.T) {
	cases := []struct {
		name string
		fake *sampleFake
		info *ShardInfo
		// wantRanges is what the caller has to work with; 1 means the exporter
		// logs its "only 1 shard range" notice and exports single-threaded.
		wantRanges int
	}{
		{
			name:       "statistics lookup fails",
			fake:       &sampleFake{statsErr: true},
			info:       numericKeyInfo(int64(1), int64(1000000)),
			wantRanges: 4,
		},
		{
			name:       "table reports no rows",
			fake:       &sampleFake{statsRows: 0},
			info:       numericKeyInfo(int64(1), int64(1000000)),
			wantRanges: 4,
		},
		{
			name:       "probe query rejected by the server",
			fake:       &sampleFake{statsRows: 1000000, statsAvg: 140, probeErr: errors.New("access denied")},
			info:       numericKeyInfo(int64(1), int64(200000000)),
			wantRanges: 4,
		},
		{
			name:       "key yields nothing but its lowest value",
			fake:       &sampleFake{statsRows: 1000000, statsAvg: 140},
			info:       textKeyInfo("abc", "abc"),
			wantRanges: 1,
		},
		{
			name:       "estimated rows below the range count",
			fake:       &sampleFake{statsRows: 3, statsAvg: 140},
			info:       textKeyInfo("abc", "xyz"),
			wantRanges: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newSampleDB(t, tc.fake)

			ranges := planShardRanges(db, "t", tc.info, 4)
			if len(ranges) != tc.wantRanges {
				t.Fatalf("ranges = %d, want %d (%+v)", len(ranges), tc.wantRanges, ranges)
			}
			// A fallback must never be a partial cover: the widest range has to
			// reach the real MAX, inclusive. A single range carries the whole key
			// space and the exporter then exports single-threaded, so only a split
			// needs the inclusive marker.
			last := ranges[len(ranges)-1]
			if len(ranges) > 1 && !last.IsLast {
				t.Error("last range of a fallback is not the inclusive one")
			}
			if boundText(last.Maxs[0]) == "" {
				t.Error("fallback range lost its upper bound")
			}
		})
	}
}

// TestPlanShardRangesSingleRangeSendsNoQueries: threads 1 never reaches the
// parallel path, and a plan for fewer than two ranges must not touch the server.
func TestPlanShardRangesSingleRangeSendsNoQueries(t *testing.T) {
	f := &sampleFake{statsRows: 1000000, statsAvg: 140}
	db := newSampleDB(t, f)

	if got := planShardRanges(db, "t", numericKeyInfo(int64(1), int64(100)), 1); len(got) != 1 {
		t.Fatalf("ranges = %d, want 1", len(got))
	}
	if len(f.queries) != 0 {
		t.Errorf("planning a single range queried the server: %q", f.queries)
	}
}

// TestRangesFromBoundsKeepsNullRowsCovered mirrors the invariant the equal-width
// path has: MIN()/MAX() ignore NULLs and no range comparison ever matches one, so
// a nullable single-column key needs the extra IS NULL range or those rows leave
// the dump entirely.
func TestRangesFromBoundsKeepsNullRowsCovered(t *testing.T) {
	t.Run("single nullable column", func(t *testing.T) {
		info := numericKeyInfo(int64(1), int64(100))
		info.Nullable = []bool{true}
		ranges := rangesFromBounds(info, []interface{}{shardBoundary("1"), shardBoundary("50")})
		if len(ranges) != 3 || !ranges[2].NullOnly {
			t.Fatalf("ranges = %d (%+v), want two value ranges plus the NULL one", len(ranges), ranges)
		}
		if ranges[0].IsLast || !ranges[1].IsLast {
			t.Error("NULL range must not shift which value range is the inclusive one")
		}
	})

	t.Run("composite nullable key", func(t *testing.T) {
		info := &ShardInfo{
			Columns:     []string{"a", "b"},
			ColumnTypes: []string{"bigint", "bigint"},
			Mins:        []interface{}{int64(1), int64(1)},
			Maxs:        []interface{}{int64(10), int64(10)},
			IsNumeric:   []bool{true, true},
			Nullable:    []bool{false, true},
		}
		if ranges := rangesFromBounds(info, []interface{}{shardBoundary("1"), shardBoundary("5")}); ranges != nil {
			t.Errorf("composite nullable key must refuse sharding, got %+v", ranges)
		}
	})

	t.Run("composite key keeps the full second column", func(t *testing.T) {
		info := &ShardInfo{
			Columns:     []string{"a", "b"},
			ColumnTypes: []string{"bigint", "bigint"},
			Mins:        []interface{}{int64(1), int64(7)},
			Maxs:        []interface{}{int64(10), int64(70)},
			IsNumeric:   []bool{true, true},
			Nullable:    []bool{false, false},
		}
		ranges := rangesFromBounds(info, []interface{}{shardBoundary("1"), shardBoundary("5")})
		if len(ranges) != 2 {
			t.Fatalf("ranges = %d, want 2", len(ranges))
		}
		for i, r := range ranges {
			if boundText(r.Mins[1]) != "7" || boundText(r.Maxs[1]) != "70" {
				t.Errorf("range %d narrowed the non-sharding column: %v..%v", i, r.Mins[1], r.Maxs[1])
			}
		}
	})
}
