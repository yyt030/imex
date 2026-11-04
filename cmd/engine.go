package cmd

import (
	"database/sql"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/imex/internal/backend"
	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
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
// Which options an engine refuses is no longer spelled here: the rule
// tables belong to the dialect (internal/backend/refusals_postgres.go and
// refusals_mysql.go), and this file folds a run's flag values into a
// backend.RunOptions, asks for the command's table, and reports the first rule the
// run trips in the CLI's own sentence. Each rule fires on the *value*, not on
// `Changed`, where the value itself is wrong (--format sql, --threads 4, --mode
// replace): a run configured by any means cannot escape it. Options whose value is
// fine and whose *presence* is the problem (--session-var) fire on Changed, folded
// into a bool here because backend cannot import cobra — exactly like the
// format-scoped options cmd/validate.go rejects, so the shipped default stays
// usable.

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

// applyEnginePort gives an engine its own port default. It runs after the
// IMEX_* fill-in (cmd/conn_env.go) so an environment-supplied port wins, and
// before validation so the range check sees the number actually used.
func applyEnginePort(cmd *cobra.Command, cfg *config.Config, e backend.Engine) {
	if cmd.Flags().Changed("port") {
		return
	}
	if strings.TrimSpace(os.Getenv("IMEX_PORT")) != "" {
		return
	}
	// The comparison is with the dialect's own default rather than with an engine
	// name, which is what keeps this engine-blind: MySQL answers 3306, the number
	// --port was registered with, so a MySQL run writes back what it already holds
	// and changes nothing, while PostgreSQL moves the run to 5432.
	if p := backend.Must(e).DefaultPort(); cfg.Port != p {
		cfg.Port = p
	}
}

// checkEngineRefusals asks the engine which of this command's options it cannot
// serve and reports the first one the run trips.
//
// The template and the engine name inside it are here rather than in the dialect
// because the message is the CLI's own voice (face ③), and the rule tables only
// carry an option label and a reason. Reporting order is the dialect's, so the
// loop stops at the first match: `--engine pg --format sql --threads 4` says the
// format, as it always did.
func checkEngineRefusals(e backend.Engine, c backend.Command, o backend.RunOptions) error {
	for _, r := range backend.Must(e).Refusals(c) {
		if r.Applies(o) {
			return common.Usagef("%s is not supported with --engine %s: %s", r.Option(o), e, r.Reason)
		}
	}
	return nil
}

// checkCharsetForEngine resolves --default-charset against the engine. MySQL
// sends SET NAMES with it; PostgreSQL takes it as the connection's
// client_encoding, which has fewer names available. validateCharset has already
// accepted the value by the time this runs, so what is reported here is a charset
// that exists but has no counterpart on the other side.
func checkCharsetForEngine(e backend.Engine, charset string) error {
	if err := backend.Must(e).ValidateCharset(charset); err != nil {
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
	o := backend.RunOptions{
		Format:     cfg.Format,
		Threads:    cfg.Threads,
		NoHexBlob:  cfg.NoHexBlob,
		SessionVar: cmd.Flags().Changed("session-var"),
	}
	if err := checkEngineRefusals(e, backend.CmdExport, o); err != nil {
		return err
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
	o := backend.RunOptions{
		Format:     cfg.Format,
		Mode:       string(cfg.Mode),
		SessionVar: cmd.Flags().Changed("session-var"),
	}
	if err := checkEngineRefusals(e, backend.CmdImport, o); err != nil {
		return err
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
	o := backend.RunOptions{Object: cfg.Object}
	if err := checkEngineRefusals(e, backend.CmdList, o); err != nil {
		return err
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
//
// The charset is not checked here: `show` validates it in validateShowConfig, after
// this refusal, and moving it would change which message a doubly-invalid run gets.
func checkShowEngineOptions(e backend.Engine, kind string) error {
	return checkEngineRefusals(e, backend.CmdShow, backend.RunOptions{ShowKind: kind})
}

// parseSessionVars runs --session-var values through the shared parser with the
// reserved-name list of the engine this run resolved. The list belongs to the
// dialect: those names are the MySQL driver's own DSN parameters, and an engine
// whose connection string carries none of them reserves none
// (Backend.IsReservedConnParam). internal/common cannot import backend, so the
// predicate travels as a value.
func parseSessionVars(engine string, pairs []string) (map[string]string, error) {
	return common.ParseSessionVars(pairs, backend.Lookup(engine).IsReservedConnParam)
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
