package list

import (
	"github.com/imex/internal/backend"
	"github.com/imex/internal/config"
)

// Object names accepted by `imex list`. They are also the subcommand names, so
// the command tree and this list cannot drift apart without a test failing.
const (
	ObjectDatabases = "databases"
	ObjectTables    = "tables"
	ObjectViews     = "views"
	ObjectColumns   = "columns"
	ObjectIndexes   = "indexes"
	ObjectTriggers  = "triggers"
	ObjectRoutines  = "routines"
	ObjectEvents    = "events"
)

var objectOrder = []string{
	ObjectDatabases, ObjectTables, ObjectViews, ObjectColumns,
	ObjectIndexes, ObjectTriggers, ObjectRoutines, ObjectEvents,
}

// Objects returns the accepted object names in help/error order.
func Objects() []string {
	out := make([]string, len(objectOrder))
	copy(out, objectOrder)
	return out
}

// queryBuilders maps one engine to the catalog that engine reads. MySQL is
// deliberately absent: it is the fallback below, not a table row, so a run whose
// engine field was never set (every library caller that builds a config by hand,
// and every command line that omits --engine) keeps the statements it has always
// emitted. A new engine adds one row here plus its own queries_<engine>.go file;
// nothing else in this package knows engine names.
//
// The key is a parsed backend.Engine and the lookup goes through
// backend.Normalize, so the spelling on the command line cannot decide which
// catalog is read: `--engine PG` and `--engine pg` build the same statement.
// Before 131 the raw config string was compared here, so `--engine PG` reached
// the PostgreSQL connection (cmd parses case-insensitively) but the MySQL
// catalog, which is a server error rather than a different answer.
var queryBuilders = map[backend.Engine]func(*config.ListConfig) (string, []any, []string, error){
	backend.EnginePG:    buildPGQuery,
	backend.EngineDM:    buildDMQuery,
	backend.EngineGauss: buildPGQuery,
}

// BuildQuery returns the statement, its bound arguments and the output column
// names for one list object. The statement always reads the system catalog and
// never interpolates user input: --database, --table and --pattern travel as
// placeholders.
//
// The engine decides which catalog is read and which placeholder style the
// statement uses, and each engine's statements live in their own file:
// queries_mysql.go and queries_pg.go. Neither builder is changed by the split:
// every statement they emit is byte-identical to the ones this file emitted
// before, so what a server sees does not depend on the refactor.
func BuildQuery(cfg *config.ListConfig) (string, []any, []string, error) {
	if build, ok := queryBuilders[backend.Normalize(cfg.Engine)]; ok {
		return build(cfg)
	}
	return buildMySQLQuery(cfg)
}
