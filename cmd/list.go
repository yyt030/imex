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

The result is written to stdout and logs go to stderr, so stdout carries data only:
  imex list tables -D mydb

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
			f.StringVarP(&cfg.Table, "table", "t", "", "Table to inspect")
		}),
		newListObjectCmd(list.ObjectIndexes, "List indexes of a table", func(cfg *config.ListConfig, f *pflag.FlagSet) {
			f.StringVarP(&cfg.Table, "table", "t", "", "Table to inspect")
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

	f.StringVarP(&cfg.Host, "host", "h", "127.0.0.1", "Database host address")
	f.IntVarP(&cfg.Port, "port", "P", 3306, "TCP/IP port")
	f.StringVarP(&cfg.User, "user", "u", "root", "Database username")
	f.StringVarP(&cfg.Password, "password", "p", "", "Database password")
	f.StringVarP(&cfg.Socket, "socket", "S", "", "UNIX domain socket path")
	f.StringVarP(&cfg.Database, "database", "D", "", "Database to inspect (required for columns and indexes)")

	f.DurationVar(&cfg.ConnectTimeout, "connect-timeout", config.DefaultConnectTimeout, "TCP connect timeout")
	f.DurationVar(&cfg.SocketReadTimeout, "socket-read-timeout", config.DefaultSocketTimeout, "Per-read socket timeout")
	f.DurationVar(&cfg.SocketWriteTimeout, "socket-write-timeout", config.DefaultSocketTimeout, "Per-write socket timeout")
	f.StringVar(&cfg.DefaultCharset, "default-charset", config.DefaultCharset, "Connection charset")

	f.StringVar(&cfg.Pattern, "pattern", "", "LIKE pattern for object names (% and _ are wildcards)")
	f.StringVarP(&cfg.Format, "format", "f", config.ListFormatTable, "Output format: table")
	f.BoolVar(&cfg.NoColNames, "no-col-names", false, "Do not print the header row")
	f.StringVarP(&messagesPath, "messages", "m", "", "Redirect the command log to this file; with it, nothing is printed to stderr")
	f.StringVar((*string)(&cfg.LogLevel), "log-level", "info", "Log level (debug/info/warn/error)")
}

func runList(cmd *cobra.Command, cfg *config.ListConfig, object string) error {
	cfg.Object = object
	if err := validateListConfig(cfg); err != nil {
		return err
	}

	password, err := common.ResolvePassword(cfg.Password)
	if err != nil {
		return errors.Wrap(err, "failed to resolve password")
	}
	cfg.Password = password

	common.InitLogger(cfg.LogLevel)
	common.Logger.Info("list config",
		"object", object,
		"database", cfg.Database,
		"table", cfg.Table,
		"pattern", cfg.Pattern,
		"format", cfg.Format,
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

	rows, err := list.Run(cmd.Context(), db, cfg, cmd.OutOrStdout())
	if err != nil {
		return errors.Wrapf(err, "list %s failed", object)
	}
	common.Logger.Info("list completed", "object", object, "rows", rows)
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
		return common.Usagef("password is required")
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
	if !config.ValidListFormat(cfg.Format) {
		return common.Usagef("invalid --format %q: only table is supported", cfg.Format)
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
