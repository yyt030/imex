package cmd

import (
	"context"
	"strings"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/imex/internal/backend"
	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
	"github.com/imex/internal/list"
)

// newListCmd builds the list command tree with a fresh ListConfig per object.
// The tree is rebuilt instead of sharing one package-level config because each
// object has its own flag defaults (tables default to base, routines to all) and
// flag parsing writes those defaults into the bound struct.
func newListCmd() *cobra.Command {
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List databases and schema objects",
		Long: `List databases, schemas, tables, views, columns, indexes, triggers, routines or events.

The result goes to --output when it names a file, and to stdout otherwise.
--messages only takes the log and the errors; it never receives the result:
  imex list tables -D mydb
  imex list tables -D mydb -o /backup/tables.txt

list databases names the databases the server holds (one row on Dameng, where an
instance owns a single database); list schemas names the schemas inside them.
Without --database, schema-scoped lists cover every non-system schema and include
a database column; --all adds the system schemas instead of hiding them. columns
and indexes need both --database and --table.`,
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return common.Usagef("list requires an object: %s", strings.Join(list.ShortNames(), ", "))
			}
			return common.Usagef("unknown object %q: expected one of %s", args[0], strings.Join(list.ShortNames(), ", "))
		},
	}

	listCmd.AddCommand(
		newListObjectCmd(list.ObjectDatabases, "List databases", nil),
		newListObjectCmd(list.ObjectSchemas, "List schemas", func(cfg *config.ListConfig, f *pflag.FlagSet) {
			f.BoolVar(&cfg.All, "all", false, "Include system schemas when --database is not given")
		}),
		newListObjectCmd(list.ObjectTables, "List tables", func(cfg *config.ListConfig, f *pflag.FlagSet) {
			f.StringVar(&cfg.Type, "type", "base", "Object type: base, view or all")
			f.BoolVar(&cfg.All, "all", false, "Include system schemas when --database is not given")
		}),
		newListObjectCmd(list.ObjectViews, "List views", func(cfg *config.ListConfig, f *pflag.FlagSet) {
			f.BoolVar(&cfg.All, "all", false, "Include system schemas when --database is not given")
		}),
		newListObjectCmd(list.ObjectColumns, "List columns of a table", func(cfg *config.ListConfig, f *pflag.FlagSet) {
			f.StringVarP(&cfg.Table, "table", "t", "", "Table to inspect (required)")
		}),
		newListObjectCmd(list.ObjectIndexes, "List indexes of a table", func(cfg *config.ListConfig, f *pflag.FlagSet) {
			f.StringVarP(&cfg.Table, "table", "t", "", "Table to inspect (required)")
		}),
		newListObjectCmd(list.ObjectTriggers, "List triggers", func(cfg *config.ListConfig, f *pflag.FlagSet) {
			f.StringVarP(&cfg.Table, "table", "t", "", "Only list triggers of this table")
			f.BoolVar(&cfg.All, "all", false, "Include system schemas when --database is not given")
		}),
		newListObjectCmd(list.ObjectRoutines, "List stored procedures and functions", func(cfg *config.ListConfig, f *pflag.FlagSet) {
			f.StringVar(&cfg.Type, "type", "all", "Routine type: procedure, function or all")
			f.BoolVar(&cfg.All, "all", false, "Include system schemas when --database is not given")
		}),
		newListObjectCmd(list.ObjectEvents, "List scheduled events", func(cfg *config.ListConfig, f *pflag.FlagSet) {
			f.BoolVar(&cfg.All, "all", false, "Include system schemas when --database is not given")
		}),
	)
	return listCmd
}

func newListObjectCmd(object, short string, extra func(*config.ListConfig, *pflag.FlagSet)) *cobra.Command {
	cfg := config.NewListConfig()
	cmd := &cobra.Command{
		// Aligned with `show`: the three-letter name is canonical (it is what
		// `imex list --help` lists) and the long name stays a valid alias, so
		// both `imex list tab -D mydb` and `imex list tables -D mydb` keep
		// parsing to the same leaf. RunE receives the long `object`, which is
		// what the query builders key on, so nothing downstream changes.
		Use:          list.ShortName(object),
		Aliases:      []string{object},
		Short:        short,
		Args:         exactArgs(0),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runList(cmd, cfg, object)
		},
	}
	registerListFlags(cmd, cfg)
	if extra != nil {
		extra(cfg, cmd.Flags())
	}
	return cmd
}

func registerListFlags(cmd *cobra.Command, cfg *config.ListConfig) {
	f := cmd.Flags()

	// Override the default help flag so -h stays available for --host.
	f.BoolP("help", "?", false, "help for list "+cmd.Use)

	f.StringVar(&cfg.Engine, "engine", "mysql", "Database engine: mysql, pg, dm")
	f.StringVarP(&cfg.Host, "host", "h", "127.0.0.1", "Database host address (also IMEX_HOST)")
	f.IntVarP(&cfg.Port, "port", "P", 3306, "TCP/IP port (also IMEX_PORT)")
	f.StringVarP(&cfg.User, "user", "u", "root", "Database username (also IMEX_USER)")
	f.StringVarP(&cfg.Password, "password", "p", "", "Database password (also IMEX_PASSWORD)")
	f.StringVarP(&cfg.Database, "database", "D", "",
		"Database to inspect (required for columns and indexes; also IMEX_DATABASE)")

	f.DurationVar(&cfg.ConnectTimeout, "connect-timeout", config.DefaultConnectTimeout, "TCP connect timeout")
	f.DurationVar(&cfg.SocketReadTimeout, "socket-read-timeout", config.DefaultSocketTimeout, "Per-read socket timeout")
	f.DurationVar(&cfg.SocketWriteTimeout, "socket-write-timeout", config.DefaultSocketTimeout, "Per-write socket timeout")
	f.StringVar(&cfg.DefaultCharset, "default-charset", config.DefaultCharset, "Connection charset")

	f.StringVar(&cfg.Pattern, "pattern", "", "LIKE pattern for object names (% and _ are wildcards)")
	f.BoolVar(&cfg.NoColNames, "no-col-names", false, "Do not print the header row")
	f.StringVarP(&cfg.Output, "output", "o", "", "Write the result to this file (default: stdout)")
	f.BoolVar(&cfg.NoOverwrite, "no-overwrite", false, "Refuse to overwrite an existing output file (default: overwrite)")
	f.StringVarP(&messagesPath, "messages", "m", "", "Log/error file; result still goes to stdout or --output; nothing on screen")
	f.StringVar((*string)(&cfg.LogLevel), "log-level", "info", "Log level (debug/info/warn/error)")
}

func runList(cmd *cobra.Command, cfg *config.ListConfig, object string) error {
	cfg.Object = object

	// Same rule as export and import: an unset connection field may be filled
	// from its IMEX_* variable before validation looks at the value.
	connEnv, err := applyConnEnv(cmd.Flags(), &cfg.Config)
	if err != nil {
		return err
	}

	e, err := parseEngine(cfg.Engine)
	if err != nil {
		return err
	}
	applyEnginePort(cmd, &cfg.Config, e)

	if err := validateListConfig(cfg); err != nil {
		return err
	}

	password, err := common.ResolvePassword(cfg.Password)
	if err != nil {
		return errors.Wrap(err, "failed to resolve password")
	}
	cfg.Password = password

	common.InitLogger(cfg.LogLevel)
	logConnEnv(connEnv)
	common.Logger.Info("list config",
		"engine", cfg.Engine,
		"object", object,
		"database", cfg.Database,
		"table", cfg.Table,
		"pattern", cfg.Pattern,
		"output", cfg.Output,
	)

	connCfg := connConfigFor(e, object, cfg)
	db, err := openDatabase(e, &connCfg)
	if err != nil {
		return errors.Wrap(err, "failed to connect to database")
	}
	defer db.Close()

	// Dameng's driver sets no socket deadline of its own and only interrupts a
	// blocked login/read when the context is cancellable. Bound the handshake by
	// --connect-timeout and the metadata read by --socket-read-timeout, so an
	// unresponsive server reports an error instead of hanging the run forever.
	pingCtx, cancelPing := context.WithTimeout(cmd.Context(), cfg.ConnectTimeout)
	defer cancelPing()
	if err := common.PingWithRetry(pingCtx, db, backend.Must(e).TransientError); err != nil {
		return err
	}

	queryCtx, cancelQuery := context.WithTimeout(cmd.Context(), cfg.SocketReadTimeout)
	defer cancelQuery()
	sink := newResultSink(cfg.Output, cfg.NoOverwrite, cmd.OutOrStdout())
	rows, err := list.Run(queryCtx, db, cfg, sink)
	if err != nil {
		return errors.Wrapf(err, "list %s failed", object)
	}
	// The completion record is written before the result is delivered so that, in
	// the --messages-without---output channel, the file reads as the run's log
	// first and its result last.
	common.Logger.Info("list completed", "object", object, "rows", rows)
	if err := sink.Flush(); err != nil {
		return errors.Wrapf(err, "list %s failed", object)
	}
	return nil
}

// connConfigFor returns the connection options this run dials with.
//
// On DM, --database means a schema name on the object lists: it is written into
// the DSN's schema parameter, and the server rejects an unknown schema during
// login (Error -2103). The two namespace commands answer a different question --
// "list databases" filters the single V$DATABASE row, "list schemas" filters the
// schema list -- so on those two the value must stay out of the DSN; otherwise
// "list databases -D DAMENG" or "list schemas -D nosuch" would fail at login for
// a name that is valid to filter by (and would report 0 rows nowhere).
func connConfigFor(e backend.Engine, object string, cfg *config.ListConfig) config.Config {
	connCfg := cfg.Config
	if e == backend.EngineDM && (object == list.ObjectDatabases || object == list.ObjectSchemas) {
		connCfg.Database = ""
	}
	return connCfg
}

func validateListConfig(cfg *config.ListConfig) error {
	// The engine is resolved here as well as in runList: the option checks below
	// are validation, and a caller that builds a ListConfig by hand must hit the
	// same refusal as one that came through the command line. runList needs the
	// value anyway, to pick the backend it dials.
	e, err := parseEngine(cfg.Engine)
	if err != nil {
		return err
	}
	if err := checkListEngineOptions(e, cfg); err != nil {
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
	if err := validateCharset(cfg.DefaultCharset, "list", backend.Must(e)); err != nil {
		return err
	}
	if err := validateLogLevel(string(cfg.LogLevel)); err != nil {
		return err
	}
	if err := validateDistinctPaths("output", cfg.Output, true); err != nil {
		return err
	}
	switch cfg.Object {
	case list.ObjectDatabases, list.ObjectSchemas, list.ObjectViews, list.ObjectTriggers, list.ObjectEvents:
		return nil
	case list.ObjectTables:
		switch cfg.Type {
		case "base", "view", "all":
			return nil
		default:
			return common.Usagef("invalid --type %q: must be one of base/view/all", cfg.Type)
		}
	case list.ObjectRoutines:
		switch cfg.Type {
		case "procedure", "function", "all":
			return nil
		default:
			return common.Usagef("invalid --type %q: must be one of procedure/function/all", cfg.Type)
		}
	case list.ObjectColumns, list.ObjectIndexes:
		if cfg.Database == "" {
			return common.Usagef("--database is required for list %s", cfg.Object)
		}
		if cfg.Table == "" {
			return common.Usagef("--table is required for list %s", cfg.Object)
		}
		return nil
	default:
		return common.Usagef("unknown object %q: expected one of %s", cfg.Object, strings.Join(list.ShortNames(), ", "))
	}
}

func init() {
	rootCmd.AddCommand(newListCmd())
}
