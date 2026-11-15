package _import

import (
	"testing"

	"github.com/imex/internal/backend"
	"github.com/imex/internal/config"
)

// TestSQLDialectFollowsEngine pins the import-side reader against the export
// writer: whatever internal/export writes for a `-f sql` dump, this must read
// back as the same grammar, or the round-trip fails on the file imex itself just
// produced. GaussDB reads PostgreSQL grammar for the reason documented there
// (exporter.sqlDialect): its DDL and literals are PostgreSQL's.
//
// The import side has no --sql-dialect flag: the dump's grammar is implied by
// the engine it is being read into, so the engine is the only input.
func TestSQLDialectFollowsEngine(t *testing.T) {
	cases := []struct {
		engine backend.Engine
		want   config.SQLDialect
	}{
		{backend.EngineMySQL, config.SQLDialectMySQL},
		{backend.EnginePG, config.SQLDialectPG},
		{backend.EngineDM, config.SQLDialectDM},
		{backend.EngineGauss, config.SQLDialectPG},
	}
	for _, c := range cases {
		imp := &Importer{cfg: &config.ImportConfig{}, be: backend.Must(c.engine)}
		if got := imp.sqlDialect(); got != c.want {
			t.Errorf("import dialect for -E %s = %q, want %q", c.engine, got, c.want)
		}
	}
}
