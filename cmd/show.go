package cmd

import (
	"context"
	"strings"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"

	"github.com/imex/internal/backend"
	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
	"github.com/imex/internal/show"
)

// newShowCmd builds the show command tree: one subcommand per object kind, each
// printing the definition of a single named object. The tree is rebuilt per kind
// so `imex show tab --help` reads on its own, the way `imex list tab --help`
// already does.
func newShowCmd() *cobra.Command {
	showCmd := &cobra.Command{
		Use:   "show",
		Short: "Show the definition of one object",
		Long: `Show the CREATE statement the server holds for one table, view, trigger,
event, procedure or function.

The definition goes to --output, or to stdout when neither --output nor
--messages is given. With --messages and no --output the log and the definition
share that file and nothing is printed to the screen.

  imex show tab users -D mydb
  imex show vws v_orders -D mydb -o /backup/v_orders.sql

A name may carry its own schema qualifier (mydb.users); otherwise --database
supplies it, so one of the two is required. Batches come from ` + "`imex list`" + `.`,
		Args:         cobra.ArbitraryArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return common.Usagef("show requires a kind: %s", strings.Join(show.ShortNames(), ", "))
			}
			return common.Usagef("unknown kind %q: expected one of %s", args[0], strings.Join(show.ShortNames(), ", "))
		},
	}

	for _, kind := range show.Kinds() {
		showCmd.AddCommand(newShowKindCmd(kind))
	}
	return showCmd
}

func newShowKindCmd(kind show.Kind) *cobra.Command {
	cfg := config.NewShowConfig()
	cmd := &cobra.Command{
		Use:          kind.Short,
		Aliases:      []string{kind.Full},
		Short:        "Show the definition of one " + kind.Full,
		Args:         exactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShow(cmd, cfg, kind, args[0])
		},
	}
	registerShowFlags(cmd, cfg)
	return cmd
}

func registerShowFlags(cmd *cobra.Command, cfg *config.ShowConfig) {
	f := cmd.Flags()

	// Same rule as export, import and list: -h belongs to --host, so help gets ?.
	f.BoolP("help", "?", false, "help for show "+cmd.Use)

	f.StringVar(&cfg.Engine, "engine", "mysql", "Database engine: mysql, pg, dm")
	f.StringVarP(&cfg.Host, "host", "h", "127.0.0.1", "Database host address (also IMEX_HOST)")
	f.IntVarP(&cfg.Port, "port", "P", 3306, "TCP/IP port (also IMEX_PORT)")
	f.StringVarP(&cfg.User, "user", "u", "root", "Database username (also IMEX_USER)")
	f.StringVarP(&cfg.Password, "password", "p", "", "Database password (also IMEX_PASSWORD)")
	f.StringVarP(&cfg.Database, "database", "D", "",
		"Schema of the object (required unless qualified; also IMEX_DATABASE)")

	f.DurationVar(&cfg.ConnectTimeout, "connect-timeout", config.DefaultConnectTimeout, "TCP connect timeout")
	f.DurationVar(&cfg.SocketReadTimeout, "socket-read-timeout", config.DefaultSocketTimeout, "Per-read socket timeout")
	f.DurationVar(&cfg.SocketWriteTimeout, "socket-write-timeout", config.DefaultSocketTimeout, "Per-write socket timeout")
	f.StringVar(&cfg.DefaultCharset, "default-charset", config.DefaultCharset, "Connection charset")

	f.StringVarP(&cfg.Output, "output", "o", "", "Write the result to this file (default: stdout, or --messages)")
	f.BoolVar(&cfg.NoOverwrite, "no-overwrite", false, "Refuse to overwrite an existing output file (default: overwrite)")
	f.StringVarP(&messagesPath, "messages", "m", "", "Log file; without --output the result is appended here; nothing on screen")
	f.StringVar((*string)(&cfg.LogLevel), "log-level", "info", "Log level (debug/info/warn/error)")
}

func runShow(cmd *cobra.Command, cfg *config.ShowConfig, kind show.Kind, ref string) error {
	cfg.Ref = ref
	cfg.Schema, cfg.Object = show.SplitName(ref)
	if cfg.Object == "" {
		return common.Usagef("invalid object name %q: the part after the schema qualifier is empty", ref)
	}

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
	if err := checkShowEngineOptions(e, kind.Short); err != nil {
		return err
	}

	if err := validateShowConfig(cfg); err != nil {
		return err
	}

	password, err := common.ResolvePassword(cfg.Password)
	if err != nil {
		return errors.Wrap(err, "failed to resolve password")
	}
	cfg.Password = password

	common.InitLogger(cfg.LogLevel)
	logConnEnv(connEnv)
	common.Logger.Info("show config",
		"engine", cfg.Engine,
		"kind", kind.Short,
		"schema", cfg.Schema,
		"name", cfg.Object,
		"output", cfg.Output,
	)

	db, err := openDatabase(e, &cfg.Config)
	if err != nil {
		return errors.Wrap(err, "failed to connect to database")
	}
	defer db.Close()

	// Same deadline guard as `list`: see the note in runList. Without a
	// cancellable context the Dameng driver never interrupts a stuck handshake
	// or catalog read.
	pingCtx, cancelPing := context.WithTimeout(cmd.Context(), cfg.ConnectTimeout)
	defer cancelPing()
	if err := common.PingWithRetry(pingCtx, db, backend.Must(e).TransientError); err != nil {
		return err
	}

	queryCtx, cancelQuery := context.WithTimeout(cmd.Context(), cfg.SocketReadTimeout)
	defer cancelQuery()
	sink := newResultSink(cfg.Output, cfg.NoOverwrite, cmd.OutOrStdout())
	written, err := show.Run(queryCtx, db, backend.Must(e), kind, cfg, sink)
	if err != nil {
		return errors.Wrapf(err, "show %s failed", kind.Short)
	}
	// Ordered like list: the completion record belongs to the log, the definition
	// is the payload that comes last in the file the two share.
	common.Logger.Info("show completed", "kind", kind.Short, "bytes", written)
	if err := sink.Flush(); err != nil {
		return errors.Wrapf(err, "show %s failed", kind.Short)
	}
	return nil
}

func validateShowConfig(cfg *config.ShowConfig) error {
	// Resolved here for the same reason list does it: runShow needs the engine to
	// dial, the validator needs it to reject a charset PostgreSQL cannot name.
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
	if err := validateCharset(cfg.DefaultCharset, "show", backend.Must(e)); err != nil {
		return err
	}
	if err := validateLogLevel(string(cfg.LogLevel)); err != nil {
		return err
	}
	if cfg.Schema == "" && cfg.Database == "" {
		return common.Usagef("--database is required for show unless the object name carries a schema qualifier")
	}
	return validateDistinctPaths("output", cfg.Output, true)
}

func init() {
	rootCmd.AddCommand(newShowCmd())
}
