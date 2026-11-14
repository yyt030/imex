package cmd

import (
	"context"
	"io"
	"os"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"

	"github.com/imex/internal/backend"
	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
	"github.com/imex/internal/exec"
)

// exec reads its SQL argument or --file from the caller, and stdin only when
// neither was given. A test overrides this to hand over a canned stream without
// wiring a real pipe.
var execStdin io.Reader = os.Stdin

// newExecCmd builds `imex exec`: run SQL statements against the target and report
// each result. It is the execution counterpart to `show`, and it shares the
// connection contract exactly, so the same --engine/--host/--port/--user/
// --password/--database (and the IMEX_* fill-in) configure both.
func newExecCmd() *cobra.Command {
	cfg := config.NewExecConfig()
	cmd := &cobra.Command{
		Use:   "exec [sql]",
		Short: "Execute SQL statements and report each result",
		Long: `Execute one or more SQL statements against the target and print what each did.

The SQL comes from --file, a single argument, or standard input, in that order.
Statements are split at top-level semicolons (a semicolon inside a quoted value,
a comment or a PostgreSQL dollar-quoted body does not split) and run one at a
time, stopping at the first the server rejects. A statement that returns rows
prints an aligned table; a statement that writes reports its affected rows.

  imex exec -D mydb 'CREATE TABLE t (id INT PRIMARY KEY, name TEXT)'
  imex exec -D mydb -f schema.sql
  imex show tab users -D mydb | imex exec -D otherdb

This replays the DDL that ` + "`imex show`" + ` saved, and it runs arbitrary SQL
independent of the data import/export paths. A routine definition whose body
holds its own semicolons must come from a script that switches the terminator
with a DELIMITER directive, because a bare semicolon inside the body reads as the
end of the statement.`,
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExec(cmd, cfg, args)
		},
	}
	registerExecFlags(cmd, cfg)
	return cmd
}

func registerExecFlags(cmd *cobra.Command, cfg *config.ExecConfig) {
	f := cmd.Flags()

	registerConnFlags(f, &cfg.Config, connFlagOpts{
		HelpLabel:         "help for exec",
		DatabaseUsage:     "Default schema the statements run in (also IMEX_DATABASE)",
		CharsetTarget:     &cfg.DefaultCharset,
		CharsetDefault:    config.DefaultCharset,
		CharsetUsage:      "Connection charset",
		MessagesUsage:     "Log/error file; result still goes to stdout or --output; nothing on screen",
		OutputTarget:      &cfg.Output,
		NoOverwriteTarget: &cfg.NoOverwrite,
		OutputUsage:       "Write the result to this file (default: stdout)",
	})

	f.StringVarP(&cfg.File, "file", "f", "", "Read the SQL to run from this file")
	f.BoolVar(&cfg.NoColNames, "no-col-names", false, "Do not print the header row of a result set")
}

func runExec(cmd *cobra.Command, cfg *config.ExecConfig, args []string) error {
	// Same rule as export, import and list: an unset connection field may be
	// filled from its IMEX_* variable before validation looks at the value.
	connEnv, err := applyConnEnv(cmd.Flags(), &cfg.Config)
	if err != nil {
		return err
	}

	e, err := parseEngine(cfg.Engine)
	if err != nil {
		return err
	}
	applyEnginePort(cmd, &cfg.Config, e)

	if err := validateExecConfig(cfg); err != nil {
		return err
	}

	// Resolve the SQL text before dialing: reading --file or stdin is local, and a
	// run with nothing to execute is a parameter error the operator can fix without
	// a server in the loop.
	sqlText, err := resolveExecSQL(cfg, args)
	if err != nil {
		return err
	}

	password, err := common.ResolvePassword(cfg.Password)
	if err != nil {
		return errors.Wrap(err, "failed to resolve password")
	}
	cfg.Password = password

	common.InitLogger(cfg.LogLevel)
	logConnEnv(connEnv)
	common.Logger.Info("exec config",
		"engine", cfg.Engine,
		"database", cfg.Database,
		"file", cfg.File,
		"output", cfg.Output,
	)

	db, err := openDatabase(e, &cfg.Config)
	if err != nil {
		return errors.Wrap(err, "failed to connect to database")
	}
	defer db.Close()

	// Same deadline guard as `list`: see the note in runList.
	pingCtx, cancelPing := context.WithTimeout(cmd.Context(), cfg.ConnectTimeout)
	defer cancelPing()
	if err := common.PingWithRetry(pingCtx, db, backend.Must(e).TransientError); err != nil {
		return err
	}

	queryCtx, cancelQuery := context.WithTimeout(cmd.Context(), cfg.SocketReadTimeout)
	defer cancelQuery()
	sink := newResultSink(cfg.Output, cfg.NoOverwrite, cmd.OutOrStdout())
	stats, err := exec.Run(queryCtx, db, e, sqlText, !cfg.NoColNames, sink)
	if err != nil {
		return errors.Wrap(err, "exec failed")
	}
	// The completion record belongs to the log, so it is written before the result
	// is flushed: with --messages the log file ends with this run's own summary,
	// while the result goes to stdout or --output and never into that file.
	common.Logger.Info("exec completed", "statements", stats.Statements, "rows", stats.Rows, "affected", stats.Affected)
	if err := sink.Flush(); err != nil {
		return errors.Wrap(err, "exec failed")
	}
	return nil
}

// resolveExecSQL reads the SQL to run from --file, else from a single positional
// argument, else from stdin. The three sources are mutually exclusive: a run that
// names more than one is asked to choose, because which one it meant is exactly
// the kind of ambiguity that would silently run the wrong statements.
func resolveExecSQL(cfg *config.ExecConfig, args []string) (string, error) {
	switch {
	case cfg.File != "":
		if len(args) > 0 {
			return "", common.Usagef("exec takes either --file or a SQL argument, not both")
		}
		data, err := os.ReadFile(cfg.File)
		if err != nil {
			return "", errors.Wrapf(err, "failed to read SQL file %s", cfg.File)
		}
		return string(data), nil
	case len(args) == 1:
		return args[0], nil
	case len(args) > 1:
		return "", common.Usagef("exec takes at most one SQL argument (use --file for a multi-statement script)")
	default:
		data, err := io.ReadAll(execStdin)
		if err != nil {
			return "", errors.Wrap(err, "failed to read SQL from stdin")
		}
		return string(data), nil
	}
}

func validateExecConfig(cfg *config.ExecConfig) error {
	// The engine is resolved here as well as in runExec: the charset check below is
	// validation, and a caller that builds an ExecConfig by hand must hit the same
	// refusal as one that came through the command line. runExec needs it anyway to
	// pick the backend it dials.
	e, err := parseEngine(cfg.Engine)
	if err != nil {
		return err
	}
	if err := checkCharsetForEngine(e, cfg.DefaultCharset); err != nil {
		return err
	}
	if cfg.Host == "" {
		return common.Usagef("--host is required")
	}
	if cfg.User == "" {
		return common.Usagef("--user is required")
	}
	if cfg.Password == "" {
		return common.Usagef("--password is required (or set IMEX_PASSWORD)")
	}
	if err := validatePort(cfg.Port); err != nil {
		return err
	}
	if err := validatePositiveDuration("connect-timeout", cfg.ConnectTimeout); err != nil {
		return err
	}
	if err := validatePositiveDuration("socket-read-timeout", cfg.SocketReadTimeout); err != nil {
		return err
	}
	if err := validatePositiveDuration("socket-write-timeout", cfg.SocketWriteTimeout); err != nil {
		return err
	}
	if err := validateCharset(cfg.DefaultCharset, "exec", backend.Must(e)); err != nil {
		return err
	}
	if err := validateLogLevel(string(cfg.LogLevel)); err != nil {
		return err
	}
	if err := validateDistinctPaths("output", cfg.Output, true); err != nil {
		return err
	}
	return nil
}

func init() {
	rootCmd.AddCommand(newExecCmd())
}
