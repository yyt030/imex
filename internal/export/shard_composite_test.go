package export

import (
	"strings"
	"testing"

	"github.com/imex/internal/backend"
)

// A composite shard key is bounded by a row-value comparison, "(a, b) >= (1, 2)"
// — which Dameng does not parse at all (measured on DM8: Error -2007 nearby [15],
// for integers and decimals, in WHERE and in ON). The DM render therefore
// expands the same order into per-column tests; every other engine keeps the
// tuple form it has always sent.

func compositeInfo() *ShardInfo {
	return &ShardInfo{
		Columns:   []string{"A", "B"},
		IsNumeric: []bool{true, true},
	}
}

func TestBuildShardedQuery_CompositeTupleFormStaysOnRowValueEngines(t *testing.T) {
	rng := ShardRange{
		Mins: []interface{}{shardBoundary("15"), shardBoundary("1")},
		Maxs: []interface{}{shardBoundary("30"), shardBoundary("100")},
	}
	want := "select * from t where (t.A, t.B) >= (15, 1) and (t.A, t.B) < (30, 100)"
	for _, eng := range []backend.Engine{backend.EngineMySQL, backend.EnginePG} {
		p := NewSQLParser()
		got, err := p.BuildShardedQueryFor("SELECT * FROM t", compositeInfo(), rng, backend.Must(eng))
		if err != nil {
			t.Fatalf("%s: %v", eng, err)
		}
		if got != want {
			t.Errorf("%s shard query = %q, want the tuple form %q", eng, got, want)
		}
	}

	// The MySQL bytes must not depend on the new parameter at all.
	p := NewSQLParser()
	legacy, err := p.BuildShardedQuery("SELECT * FROM t", compositeInfo(), rng)
	if err != nil {
		t.Fatal(err)
	}
	if legacy != want {
		t.Errorf("BuildShardedQuery (mysql) changed: %q", legacy)
	}
}

func TestBuildShardedQuery_CompositeExpandsLexicographicallyForDameng(t *testing.T) {
	p := NewSQLParser()

	middle, err := p.BuildShardedQueryFor("SELECT * FROM t", compositeInfo(), ShardRange{
		Mins: []interface{}{shardBoundary("15"), shardBoundary("1")},
		Maxs: []interface{}{shardBoundary("30"), shardBoundary("100")},
	}, backend.Must(backend.EngineDM))
	if err != nil {
		t.Fatal(err)
	}
	wantMiddle := "select * from t where (t.A > 15 or t.A = 15 and t.B >= 1) and (t.A < 30 or t.A = 30 and t.B < 100)"
	if middle != wantMiddle {
		t.Errorf("dm shard query = %q, want %q", middle, wantMiddle)
	}

	last, err := p.BuildShardedQueryFor("SELECT * FROM t", compositeInfo(), ShardRange{
		Mins:   []interface{}{shardBoundary("45"), shardBoundary("1")},
		Maxs:   []interface{}{shardBoundary("120"), shardBoundary("100")},
		IsLast: true,
	}, backend.Must(backend.EngineDM))
	if err != nil {
		t.Fatal(err)
	}
	wantLast := "select * from t where (t.A > 45 or t.A = 45 and t.B >= 1) and (t.A < 120 or t.A = 120 and t.B <= 100)"
	if last != wantLast {
		t.Errorf("dm last shard = %q, want %q", last, wantLast)
	}
}

// Three key columns: each disjunct pins the prefix equalities before carrying
// the comparison, so the chain still reads as one lexicographic order.
func TestBuildShardedQuery_CompositeThreeColumnExpansion(t *testing.T) {
	p := NewSQLParser()
	info := &ShardInfo{Columns: []string{"A", "B", "C"}, IsNumeric: []bool{true, true, true}}
	got, err := p.BuildShardedQueryFor("SELECT * FROM t", info, ShardRange{
		Mins: []interface{}{shardBoundary("1"), shardBoundary("2"), shardBoundary("3")},
		Maxs: []interface{}{shardBoundary("4"), shardBoundary("5"), shardBoundary("6")},
	}, backend.Must(backend.EngineDM))
	if err != nil {
		t.Fatal(err)
	}
	want := "select * from t where (t.A > 1 or t.A = 1 and t.B > 2 or t.A = 1 and t.B = 2 and t.C >= 3)" +
		" and (t.A < 4 or t.A = 4 and t.B < 5 or t.A = 4 and t.B = 5 and t.C < 6)"
	if got != want {
		t.Errorf("dm three-column expansion = %q, want %q", got, want)
	}
}

// A text key carries quoted boundary values into the expansion too, and the
// quoting has to be the target's, as everywhere else on this path.
func TestBuildShardedQuery_CompositeTextKeyBoundaryQuoting(t *testing.T) {
	p := NewSQLParser()
	info := &ShardInfo{Columns: []string{"A", "NAME"}, IsNumeric: []bool{false, false}}
	got, err := p.BuildShardedQueryFor("SELECT * FROM t", info, ShardRange{
		Mins: []interface{}{"1", "O'Brien"},
		Maxs: []interface{}{"9", "z"},
	}, backend.Must(backend.EngineDM))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, `\'`) || strings.ContainsRune(got, '`') {
		t.Errorf("dm composite render kept MySQL quoting: %q", got)
	}
	want := "select * from t where (t.A > '1' or t.A = '1' and t.NAME >= 'O''Brien')" +
		" and (t.A < '9' or t.A = '9' and t.NAME < 'z')"
	if got != want {
		t.Errorf("dm composite text key = %q, want %q", got, want)
	}
}

func TestShardRowValueComparisonIsPerEngine(t *testing.T) {
	for _, eng := range []backend.Engine{backend.EngineMySQL, backend.EnginePG, backend.EngineGauss} {
		if !shardRowValueComparison(backend.Must(eng)) {
			t.Errorf("%s reads row-value comparisons; it must keep the tuple form", eng)
		}
	}
	if shardRowValueComparison(backend.Must(backend.EngineDM)) {
		t.Error("dm rejects (a,b) >= (1,2) and must take the expansion")
	}
	if !shardRowValueComparison(nil) {
		t.Error("a caller with no engine means MySQL")
	}
}
