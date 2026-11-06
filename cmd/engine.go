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

// checkCharsetForEngine asks the engine whether it accepts --default-charset.
// The accepted names and the rejection message are the dialect's
// (Backend.ValidateCharset): MySQL owns its 16-name SET NAMES vocabulary, pg
// resolves the same option to a client_encoding. This check runs before cmd's
// format rules, so the engine gets to name an unknown charset even when the
// format would also reject it.
func checkCharsetForEngine(e backend.Engine, charset string) error {
	if err := backend.Must(e).ValidateCharset(charset); err != nil {
		// Re-labelled as a usage error: the name lives in the backend's vocabulary,
		// but a bad option value has to exit 2 like every other parameter problem.
		return common.Usagef("%v", err)
	}
	return nil
}

// checkExportEngineOptions rejects the export options --engine pg cannot serve, in
// the order a run reports them: the output format first (it decides which writer
// the other options belong to), then parallelism, then session settings.
//
// The rules, their order and their reasons live in the dialect
// (refusals_postgres.go); this function only folds the run's flags into one
// backend.RunOptions, because cobra's flag state cannot cross into that package.
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

// checkImportEngineOptions folds the import run into backend.RunOptions and asks
// the dialect which option it refuses.
//
// On the pg path nothing trips a rule now: --session-var is honoured like MySQL's
// (each name reaching a SET on the connection, or the reserved-name rejection in
// parseSessionVars), --format sql is read by the dialect-blind reader, --mode
// create is gone engine-blind, and the two remaining modes are what the port
// measures. What is deliberately still allowed by design is the modes
// themselves: --mode replace empties the table with DELETE, which both engines
// parse, and the referential-check toggle around it is the backend's to supply or
// omit; --mode insert_update builds an ON CONFLICT whose target the importer
// reads out of the catalogue instead of guessing.
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

// checkListEngineOptions folds the requested object into backend.RunOptions. One
// object is refused on PostgreSQL (events), and the reason travels with that rule
// in refusals_postgres.go; the other seven are answered from the PostgreSQL
// catalogue in internal/list/queries_pg.go with the same columns and the same
// --pattern/--all behaviour.
func checkListEngineOptions(e backend.Engine, cfg *config.ListConfig) error {
	o := backend.RunOptions{Object: cfg.Object}
	if err := checkEngineRefusals(e, backend.CmdList, o); err != nil {
		return err
	}
	return checkCharsetForEngine(e, cfg.DefaultCharset)
}

// checkShowEngineOptions refuses every `imex show` kind on --engine pg. The reason
// is one rule in refusals_postgres.go, next to the other dialect-owned refusals.
//
// The charset is not checked here: `show` validates it in validateShowConfig, after
// this refusal, and moving it would change which message a doubly-invalid run gets.
func checkShowEngineOptions(e backend.Engine, kind string) error {
	return checkEngineRefusals(e, backend.CmdShow, backend.RunOptions{ShowKind: kind})
}

// parseSessionVars runs --session-var values through the shared parser with the
// reserved-name list of the engine this run resolved. The list belongs to the
// dialect: those names are the ones the connection string or the tool already
// owns — the MySQL driver's DSN parameters, and on pg the driver keywords plus
// the client_encoding/statement_timeout the tool sets itself
// (Backend.IsReservedConnParam). An engine with no such names would reserve none;
// every engine here has some. internal/common cannot import backend, so the
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
