package export

import (
	"testing"

	"github.com/imex/internal/backend"
	"github.com/imex/internal/config"
)

// TestSQLDialectAutoFollowsEngine pins --sql-dialect auto for every released
// engine. GaussDB speaks PostgreSQL grammar: before it was a released engine the
// auto case fell through to MySQL, so `-E gauss -f sql` wrote backquotes, 0x hex
// and 1/0 booleans that a GaussDB server rejects at the first CREATE TABLE. The
// import-side reader resolves the same way (importer.sqlDialect), so this table
// and the one in internal/import must stay in agreement — a dump written as PG
// that is read back as MySQL is the failure mode this pins.
func TestSQLDialectAutoFollowsEngine(t *testing.T) {
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
		e := &Exporter{config: &config.ExportConfig{SQLDialect: string(config.SQLDialectAuto)}, be: backend.Must(c.engine)}
		if got := e.sqlDialect(); got != c.want {
			t.Errorf("auto dialect for -E %s = %q, want %q", c.engine, got, c.want)
		}
	}
}

// TestSQLDialectExplicitOverridesEngine pins that an explicit --sql-dialect still
// wins over the engine, including the case an operator dumps GaussDB as MySQL on
// purpose. Only the auto branch follows the engine.
func TestSQLDialectExplicitOverridesEngine(t *testing.T) {
	cases := []struct {
		explicit string
		engine   backend.Engine
		want     config.SQLDialect
	}{
		{string(config.SQLDialectMySQL), backend.EngineGauss, config.SQLDialectMySQL},
		{string(config.SQLDialectPG), backend.EngineMySQL, config.SQLDialectPG},
		{string(config.SQLDialectDM), backend.EngineGauss, config.SQLDialectDM},
		{string(config.SQLDialectPG), backend.EngineDM, config.SQLDialectPG},
	}
	for _, c := range cases {
		e := &Exporter{config: &config.ExportConfig{SQLDialect: c.explicit}, be: backend.Must(c.engine)}
		if got := e.sqlDialect(); got != c.want {
			t.Errorf("--sql-dialect %s on -E %s = %q, want %q", c.explicit, c.engine, got, c.want)
		}
	}
}
