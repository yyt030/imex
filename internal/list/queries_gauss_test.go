package list

import (
	"strings"
	"testing"

	"github.com/imex/internal/backend"
	"github.com/imex/internal/config"
)

// TestBuildGaussIndexesAvoidsLateral pins the one list object that needed its
// own statement. The shape was PREPARE/EXECUTE-tested on openGauss-lite 5.0.3:
// a 9.2-era kernel rejects `CROSS JOIN LATERAL unnest(...) WITH ORDINALITY`,
// while a SELECT-list generate_subscripts over the same int2vector is parsed.
func TestBuildGaussIndexesAvoidsLateral(t *testing.T) {
	cfg := config.NewListConfig()
	cfg.Engine = "gauss"
	cfg.Database = "omm"
	cfg.Table = "imex_dst"
	cfg.Object = ObjectIndexes

	query, args, cols, err := BuildQuery(cfg)
	if err != nil {
		t.Fatalf("BuildQuery(gauss indexes): %v", err)
	}
	for _, forbidden := range []string{"LATERAL", "WITH ORDINALITY"} {
		if strings.Contains(query, forbidden) {
			t.Errorf("gauss indexes query must not use %q: %s", forbidden, query)
		}
	}
	for _, required := range []string{"generate_subscripts(ix.indkey, 1)", "s.indkey[s.ord]", "pg_catalog.pg_attribute", "ORDER BY s.indexrelname, s.ord"} {
		if !strings.Contains(query, required) {
			t.Errorf("gauss indexes query must read %q: %s", required, query)
		}
	}
	if len(args) != 2 || args[0] != "omm" || args[1] != "imex_dst" {
		t.Errorf("gauss indexes args = %v, want [omm imex_dst]", args)
	}
	if want := []string{"database", "table", "index", "non_unique", "seq", "column", "cardinality", "index_type"}; strings.Join(cols, ",") != strings.Join(want, ",") {
		t.Errorf("gauss indexes columns = %v, want %v", cols, want)
	}

	// The PostgreSQL engine keeps its original statement: face ⑤ unchanged.
	pgCfg := *cfg
	pgCfg.Engine = "pg"
	pgQuery, _, _, err := BuildQuery(&pgCfg)
	if err != nil {
		t.Fatalf("BuildQuery(pg indexes): %v", err)
	}
	if !strings.Contains(pgQuery, "CROSS JOIN LATERAL unnest(ix.indkey) WITH ORDINALITY") {
		t.Error("pg indexes query must still use its original LATERAL/WITH ORDINALITY form")
	}
	if pgQuery == query {
		t.Error("gauss and pg indexes queries must differ")
	}
}

// TestGaussDelegatesOtherObjects pins that the other eight objects are not
// re-implemented: they are the PostgreSQL statements, measured unchanged on the
// live Gauss bed.
func TestGaussDelegatesOtherObjects(t *testing.T) {
	for _, object := range []string{
		ObjectDatabases, ObjectSchemas, ObjectTables, ObjectViews,
		ObjectColumns, ObjectTriggers, ObjectRoutines,
	} {
		newCfg := func(engine string) *config.ListConfig {
			c := config.NewListConfig()
			c.Engine = engine
			c.Database = "omm"
			c.Table = "imex_src"
			c.Object = object
			return c
		}
		gaussCfg := newCfg("gauss")
		pgCfg := newCfg("pg")

		gq, ga, gc, gerr := BuildQuery(gaussCfg)
		pq, pa, pc, perr := BuildQuery(pgCfg)
		if (gerr == nil) != (perr == nil) {
			t.Errorf("%s: gauss err=%v pg err=%v", object, gerr, perr)
			continue
		}
		if gerr != nil {
			continue
		}
		if gq != pq || strings.Join(gc, ",") != strings.Join(pc, ",") || len(ga) != len(pa) {
			t.Errorf("%s: gauss must delegate to the pg statement verbatim", object)
		}
	}
}

// TestGaussQueryBuilderIsWired pins the dispatch row that used to point gauss
// straight at the PostgreSQL builder; if it drifts back, the LATERAL-free query
// above silently stops being used.
func TestGaussQueryBuilderIsWired(t *testing.T) {
	if queryBuilders[backend.EngineGauss] == nil {
		t.Fatal("gauss has no query builder")
	}
	if queryBuilders[backend.EnginePG] == nil || queryBuilders[backend.EngineDM] == nil {
		t.Fatal("the released engines lost their query builders")
	}
}
