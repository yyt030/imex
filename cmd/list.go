package cmd

import (
	"database/sql"
	"strings"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	_ "github.com/go-sql-driver/mysql"

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
		Long: `List databases, tables, views, columns, indexes, triggers, routines or events.

The result goes to --output when it names a file, and to stdout when it does not
and no --messages file was given. With --messages and no --output the log and
the result share that file and nothing is printed to the screen:
  imex list tables -D mydb
  imex list tables -D mydb -o /backup/tables.txt

Without --database, object lists cover every non-system schema and include a
database column; --all adds information_schema, mysql, performance_schema and
sys. columns and indexes need both --database and --table.`,
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return common.Usagef("list requires an object: %s", strings.Join(list.Objects(), ", "))
			}
			return common.Usagef("unknown object %q: expected one of %s", args[0], strings.Join(list.Objects(), ", "))
		},
	}

	listCmd.AddCommand(
		newListObjectCmd(list.ObjectDatabases, "List databases", nil),
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
		Use:          object,
		Aliases:      list.Aliases(object),
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

	f.StringVarP(&cfg.Host, "host", "h", "127.0.0.1", "Database host address (also IMEX_HOST)")
	f.IntVarP(&cfg.Port, "port", "P", 3306, "TCP/IP port (also IMEX_PORT)")
	f.StringVarP(&cfg.User, "user", "u", "root", "Database username (also IMEX_USER)")
	f.StringVarP(&cfg.Password, "password", "p", "", "Database password (also IMEX_PASSWORD)")
	f.StringVarP(&cfg.Socket, "socket", "S", "", "UNIX domain socket path (also IMEX_SOCKET)")
	f.StringVarP(&cfg.Database, "database", "D", "",
		"Database to inspect (required for columns and indexes; also IMEX_DATABASE)")

	f.DurationVar(&cfg.ConnectTimeout, "connect-timeout", config.DefaultConnectTimeout, "TCP connect timeout")
	f.DurationVar(&cfg.SocketReadTimeout, "socket-read-timeout", config.DefaultSocketTimeout, "Per-read socket timeout")
	f.DurationVar(&cfg.SocketWriteTimeout, "socket-write-timeout", config.DefaultSocketTimeout, "Per-write socket timeout")
	f.StringVar(&cfg.DefaultCharset, "default-charset", config.DefaultCharset, "Connection charset")

	f.StringVar(&cfg.Pattern, "pattern", "", "LIKE pattern for object names (% and _ are wildcards)")
	f.BoolVar(&cfg.NoColNames, "no-col-names", false, "Do not print the header row")
	f.StringVarP(&cfg.Output, "output", "o", "", "Write the result to this file (default: stdout, or --messages)")
	f.BoolVar(&cfg.NoOverwrite, "no-overwrite", false, "Refuse to overwrite an existing output file (default: overwrite)")
	f.StringVarP(&messagesPath, "messages", "m", "", "Log file; without --output the result is appended here; nothing on screen")
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
		"object", object,
		"database", cfg.Database,
		"table", cfg.Table,
		"pattern", cfg.Pattern,
		"output", cfg.Output,
	)

	dsn := common.BuildDSN(&cfg.Config)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return errors.Wrap(err, "failed to connect to database")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := common.PingWithRetry(db); err != nil {
		return err
	}

	sink := newResultSink(cfg.Output, cfg.NoOverwrite, cmd.OutOrStdout())
	rows, err := list.Run(cmd.Context(), db, cfg, sink)
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

func validateListConfig(cfg *config.ListConfig) error {
	if cfg.Host == "" && cfg.Socket == "" {
		return common.Usagef("--host or --socket is required")
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
	if err := validateCharset(cfg.DefaultCharset, "list"); err != nil {
		return err
	}
	if err := validateLogLevel(string(cfg.LogLevel)); err != nil {
		return err
	}
	if err := validateDistinctPaths("output", cfg.Output, true); err != nil {
		return err
	}
	switch cfg.Object {
	case list.ObjectDatabases, list.ObjectViews, list.ObjectTriggers, list.ObjectEvents:
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
		return common.Usagef("unknown object %q: expected one of %s", cfg.Object, strings.Join(list.Objects(), ", "))
	}
}

func init() {
	rootCmd.AddCommand(newListCmd())
}
