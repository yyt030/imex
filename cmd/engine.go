package cmd

import (
	"database/sql"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/imex/internal/backend"
	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
	"github.com/imex/internal/list"
)

// This file is the whole of imex's --engine contract at the command line: the
// value is parsed here, the port default follows it, and every option the engine
// cannot honour is refused here rather than discovered mid-run by a server error.
//
// Refusing is deliberate. The alternative — translating MySQL grammar into
// PostgreSQL's — would mean the tool emits statements nobody has measured against
// a real server, and a wrong `ON CONFLICT` target or a mis-shaded CREATE TABLE
// costs the user a table, not a message. An exit-2 refusal with the reason and the
// flag to drop is the honest version of the same run, and it keeps contract face ③
// (exit codes and messages) in the tool's own words instead of the server's.
//
// Each check fires on the *value*, not on `Changed`, where the value itself is
// wrong (--format sql, --threads 4, --mode replace): a run configured by any
// means cannot escape it. Options whose value is fine and whose *presence* is the
// problem (--session-var) fire on Changed, exactly like the format-scoped options
// cmd/validate.go rejects, so the shipped default stays usable.

// resolveEngine validates --engine and applies what follows from it.
//
// The port is the only option whose sensible value the engine decides, and it is
// adjusted only when the run named neither --port nor IMEX_PORT: --port's own
// default is 3306 and its help text says so, so changing that globally to suit
// PostgreSQL would break every MySQL run's expectation for the price of one flag.
// An operator who did state a port keeps it, even a MySQL-looking one pointed at
// 5432's neighbour, because the number they typed is the number they meant.
//
// The adjustment is not logged here: the logger does not exist yet at validation
// time (see cmd/export.go's ordering). `export config` and `import config` print
// the resolved `port` field, so a data run shows what it settled on; the `list
// config` and `show` records carry no connection fields at all, so on those two
// commands the default simply applies without being echoed.
func parseEngine(value string) (backend.Engine, error) {
	e, err := backend.Parse(value)
	if err != nil {
		// backend.Parse words this as an invalid-value message; it must still exit
		// 2 like every other parameter problem.
		return "", common.Usagef("%v", err)
	}
	return e, nil
}

// applyEnginePort gives --engine pg its port default. It runs after the
// IMEX_* fill-in (cmd/conn_env.go) so an environment-supplied port wins, and
// before validation so the range check sees the number actually used.
func applyEnginePort(cmd *cobra.Command, cfg *config.Config, e backend.Engine) {
	if e != backend.EnginePG || cmd.Flags().Changed("port") {
		return
	}
	if strings.TrimSpace(os.Getenv("IMEX_PORT")) != "" {
		return
	}
	cfg.Port = backend.Must(e).DefaultPort()
}

// pgRefusal is the one shape every engine rejection shares: the option, the
// reason, and the alternative. Keeping it in one function is what makes the
// messages grep-able for a user who has several of them.
func pgRefusal(option, reason string) error {
	return common.Usagef("%s is not supported with --engine pg: %s", option, reason)
}

// checkCharsetForEngine resolves --default-charset against the engine. MySQL
// sends SET NAMES with it; PostgreSQL takes it as the connection's
// client_encoding, which has fewer names available. validateCharset has already
// accepted the value by the time this runs, so what is reported here is a charset
// that exists but has no counterpart on the other side.
func checkCharsetForEngine(e backend.Engine, charset string) error {
	if e != backend.EnginePG {
		return nil
	}
	if _, err := backend.ClientEncoding(charset); err != nil {
		// Re-labelled as a usage error: the name lives in the backend's vocabulary,
		// but a bad option value has to exit 2 like every other parameter problem,
		// and this check runs before validateCharset.
		return common.Usagef("%v", err)
	}
	return nil
}

// checkExportEngineOptions rejects the export options --engine pg cannot serve, in
// the order a run reports them: the output format first (it decides which writer
// the other options belong to), then parallelism, then session settings.
//
// What is left working is the point of the increment: a single-threaded DEL or
// Parquet dump of any SELECT, with the tool's own delimiters, escaping and NULL
// spelling, read through the same sql.RawBytes pipeline MySQL uses.
func checkExportEngineOptions(cmd *cobra.Command, e backend.Engine, cfg *config.ExportConfig) error {
	if e != backend.EnginePG {
		return nil
	}
	if cfg.Format == "sql" {
		return pgRefusal("--format sql", "the SQL writer emits MySQL INSERT text (backquoted names and MySQL escaping) that no PostgreSQL server accepts, use --format del or --format parquet")
	}
	if cfg.Threads > 1 {
		return pgRefusal("--threads", "parallel export shards the query with MySQL-only statements and has not been measured against PostgreSQL, use --threads 1")
	}
	if cfg.NoHexBlob {
		return pgRefusal("--no-hex-blob", "a PostgreSQL server rejects a NUL inside any text value (SQLSTATE 22021), so bytea columns must go out as 0x hex; imex always does it on this engine")
	}
	if cmd.Flags().Changed("session-var") {
		return pgRefusal("--session-var", "SET <name>=<value> is MySQL session grammar and imex does not translate it, put PostgreSQL settings in the server or role configuration")
	}
	return checkCharsetForEngine(e, cfg.DefaultCharset)
}

// checkImportEngineOptions rejects the import options --engine pg cannot serve.
//
// Only --mode create is refused, and for a reason the mode switch cannot fix: the
// DDL comes out of the MySQL dump reader, which this engine refuses on the other
// side of the same chain (see checkShowEngineOptions). --mode replace empties the
// table with DELETE, which both engines parse, and the referential-check toggle
// that MySQL wraps around it is now the backend's to supply — PostgreSQL has no
// such session setting and gets no statement rather than a failing one.
// --mode insert_update builds an ON CONFLICT whose target the importer reads out
// of the catalogue instead of guessing.
func checkImportEngineOptions(cmd *cobra.Command, e backend.Engine, cfg *config.ImportConfig) error {
	if e != backend.EnginePG {
		return nil
	}
	if cfg.Format == "sql" {
		return pgRefusal("--format sql", "a SQL dump is parsed with the MySQL statement grammar and carries MySQL INSERT text, import a --format del or --format parquet file instead")
	}
	if cfg.Mode == config.ImportModeCreate {
		return pgRefusal("--mode create", "it runs CREATE TABLE text out of a MySQL dump, which PostgreSQL does not accept, and imex cannot export that DDL on this engine either; create the table with pg_dump or your own DDL and use --mode insert")
	}
	if cmd.Flags().Changed("session-var") {
		return pgRefusal("--session-var", "SET <name>=<value> is MySQL session grammar and imex does not translate it, put PostgreSQL settings in the server or role configuration")
	}
	return checkCharsetForEngine(e, cfg.DefaultCharset)
}

// checkListEngineOptions rejects the object types --engine pg cannot enumerate.
//
// The other seven come from the PostgreSQL catalogue instead of
// information_schema (internal/list/queries_pg.go), with the same column names and
// the same --pattern and --all behaviour. Events are the one object MySQL has that
// PostgreSQL does not have at all, so there is nothing to report rather than an
// empty list that reads like a database without schedules.
func checkListEngineOptions(e backend.Engine, cfg *config.ListConfig) error {
	if e != backend.EnginePG {
		return nil
	}
	if cfg.Object == list.ObjectEvents {
		return pgRefusal("list events", "PostgreSQL has no event scheduler, so there is nothing to list; scheduled work lives in cron or pgAgent")
	}
	return checkCharsetForEngine(e, cfg.DefaultCharset)
}

// checkShowEngineOptions refuses every `imex show` kind on --engine pg.
//
// A definition is not a row: SHOW CREATE TABLE produces MySQL DDL, and the
// PostgreSQL equivalents (pg_get_functiondef, pg_index, a view's stored definition)
// would have to be re-imported by --mode create through the SQL reader this engine
// also refuses. Until both halves exist, the honest answer is that the tool does
// not do it, and pg_dump does.
func checkShowEngineOptions(e backend.Engine, kind string) error {
	if e != backend.EnginePG {
		return nil
	}
	return pgRefusal("show "+kind, "definitions are read with SHOW CREATE, which PostgreSQL does not have, and imex cannot import the DDL it would produce; use pg_dump")
}

// openDatabase dials the engine the run named, for the commands that hold a
// connection themselves (`list`, `show`). export and import go through their own
// packages, which resolve the same backend from the same config field, so a
// connection is built in exactly one place per engine.
//
// The pool is one connection with no ageing, which is what both commands need: a
// metadata read that must not be interrupted by a reconnect, and no parallelism to
// size a pool for.
func openDatabase(e backend.Engine, cfg *config.Config) (*sql.DB, error) {
	be, err := backend.Get(e)
	if err != nil {
		return nil, err
	}
	db, err := be.Open(cfg)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}
