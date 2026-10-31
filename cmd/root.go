package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/imex/internal/common"
	"github.com/spf13/cobra"
)

var (
	Version   = "dev"
	BuildTime = "unknown"
	GitCommit = "unknown"
)

// Persistent flag variables (shared across all subcommands).
var (
	errorStack    bool
	echoSQLValues bool
	quiet         bool
	logFormat     string
	noSQLTag      bool
)

// messagesPath is the --messages/-m value. The flag itself is registered on the
// export and import subcommands (see registerExportFlags/registerImportFlags),
// which both bind it here, so the root hook can open the log file before any
// command runs.
var messagesPath string

var rootCmd = &cobra.Command{
	Use:   "imex",
	Short: "MySQL logical backup and restore tool",
	Long: `imex is a table-level MySQL logical backup and restore tool.
It supports both MySQL and OceanBase MySQL mode.

Features:
  - Export data to DEL (delimited ASCII), SQL or Parquet format
  - Import data from DEL, SQL or Parquet format
  - Customizable delimiters and format options
  - Secure password encryption (AES-256-GCM)
  - Command log file (--messages)
  - Multiple import modes (insert, insert_update, replace, create)
  - Parallel export/import with pipeline optimization
  - Column mapping for flexible data import
  - Smart JOIN detection for parallel export`,
	SilenceErrors: true,
	SilenceUsage:  true,
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("imex version %s\n", common.Version)
		fmt.Printf("Git commit: %s\n", common.GitCommit)
		fmt.Printf("Build time: %s\n", common.BuildTime)
		fmt.Printf("Go version: %s\n", common.GoVersion)
	},
}

func init() {
	// Sync ldflags-injected version info to common package.
	common.Version = Version
	common.BuildTime = BuildTime
	common.GitCommit = GitCommit

	// Disable cobra auto-generated completion subcommand.
	rootCmd.CompletionOptions.DisableDefaultCmd = true

	// Every flag-parsing failure (unknown flag, bad enum/duration/bool value) is
	// a parameter problem, so it must exit 2 like the explicit validators rather
	// than being reported as a runtime failure.
	rootCmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return common.Usagef("%v", err)
	})

	// --- Persistent flags (appear on root, inherited by export/import) ---

	rootCmd.PersistentFlags().BoolVar(&errorStack, "error-stack", false,
		"Print full error call stack on failure (also enabled via IMEX_ERROR_STACK=1)")

	rootCmd.PersistentFlags().BoolVar(&echoSQLValues, "echo-sql-values", false,
		"Echo real row values in failed SQL, not redacted tags (also IMEX_ECHO_SQL_VALUES=1)")

	rootCmd.PersistentFlags().BoolVar(&quiet, "quiet", false,
		"Suppress progress logs; summaries and ERROR records still print (also IMEX_QUIET=1)")

	rootCmd.PersistentFlags().StringVar(&logFormat, "log-format", "console",
		"Log layout: console or text (slog key=value); also IMEX_LOG_FORMAT")

	rootCmd.PersistentFlags().BoolVar(&noSQLTag, "no-sql-tag", false,
		"Do not prepend the /* imex:<commit> */ comment to statements (also IMEX_NO_SQL_TAG=1)")

	// PersistentPreRunE: apply global switches before any subcommand runs.
	// This runs before flag validation in RunE, so log format is active even
	// for usage errors.
	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		// Resolve env-var overrides for persistent bool flags.
		common.ErrorStack = errorStack || envTruthy("IMEX_ERROR_STACK")
		common.EchoSQLValues = echoSQLValues || envTruthy("IMEX_ECHO_SQL_VALUES")
		common.Quiet = quiet || envTruthy("IMEX_QUIET")
		common.NoSQLTag = noSQLTag || envTruthy("IMEX_NO_SQL_TAG")

		// Resolve --log-format env override.
		if v := os.Getenv("IMEX_LOG_FORMAT"); strings.TrimSpace(v) != "" {
			logFormat = strings.TrimSpace(v)
		}
		// A typo here would otherwise silently fall back to the console layout,
		// while every other parameter mismatch is a usage error. Check it before
		// applying, so the failure itself is still reported in a valid layout.
		if err := validateLogFormat(logFormat); err != nil {
			return err
		}
		common.LogFormat = logFormat

		// --messages/-m hands the command log over to a file: with it, every
		// record goes to the file instead of to stderr, so the terminal stays
		// silent and the file is the run's only account. Opened here — before the
		// version banner, so the banner is the file's first line, and before RunE
		// validation, so even a usage error is recorded. The exit code is
		// unaffected, so a script can still tell success from failure without
		// reading the file.
		if path := strings.TrimSpace(messagesPath); path != "" {
			if err := common.OpenLogFile(path); err != nil {
				return err
			}
		}

		// Apply log format BEFORE any validation so errors use chosen style.
		common.ApplyLogFormat()

		// Version banner must be the first log line of a run — for the commands
		// that actually do work. `version` is excluded: its entire job is to
		// print those same four fields, so prefixing it with a log record would
		// duplicate the output and make `imex version` unusable in pipelines
		// and version greps.
		commandName := cmd.Name()
		if cmd.Parent() != nil && cmd.Parent().Name() == "list" {
			commandName = "list " + commandName
		}
		if commandName != "version" {
			common.LogVersion(commandName)
		}

		return nil
	}

	rootCmd.AddCommand(exportCmd)
	rootCmd.AddCommand(importCmd)
	rootCmd.AddCommand(versionCmd)
	// encryptCmd is added by encrypt.go's init()
}

// envTruthy reports whether the named env var is set to a truthy value
// (1/true/yes/on, case-insensitive, trimmed).
func envTruthy(name string) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// Execute runs the root command and surfaces errors, optionally with stack traces.
func Execute() error {
	err := rootCmd.Execute()
	if err != nil {
		if common.ErrorStack {
			common.Logger.Error("command failed", "error", fmt.Sprintf("%+v", err))
		} else {
			common.Logger.Error("command failed", "error", err.Error())
		}
	}
	// The run is what the --messages file records, so it is closed last, after
	// the failure above has been logged. CloseLogFile restores stderr first, so
	// this very record cannot be lost together with the file.
	if cerr := common.CloseLogFile(); cerr != nil {
		common.Logger.Error("failed to close message file", "error", cerr.Error())
	}
	return err
}
