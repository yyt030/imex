package cmd

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/imex/internal/config"
)

// importDefaultCharset is a temp variable for the --default-charset flag.
// See exportDefaultCharset for rationale.
var importDefaultCharset string

// registerImportFlags registers all flags for the import subcommand.
// Descriptions and defaults follow docs/cli-oracle.txt apart from the
// deltas registered in that file's header.
func registerImportFlags(cmd *cobra.Command) {
	f := cmd.Flags()

	// Override default help flag to free up -h for --host.
	f.BoolP("help", "?", false, "help for import")

	// Connection options
	f.StringVarP(&importCfg.Host, "host", "h", "127.0.0.1", "Database host address")
	f.IntVarP(&importCfg.Port, "port", "P", 3306, "TCP/IP port")
	f.StringVarP(&importCfg.User, "user", "u", "root", "Database username")
	f.StringVarP(&importCfg.Password, "password", "p", "", "Database password")
	f.StringVarP(&importCfg.Socket, "socket", "S", "", "UNIX domain socket path")
	f.StringVarP(&importCfg.Database, "database", "D", "", "Database name")

	// Timeouts
	f.DurationVar(&importCfg.ConnectTimeout, "connect-timeout", 5*time.Second, "TCP connect timeout (maps to JDBC connectTimeout)")
	f.DurationVar(&importCfg.SocketReadTimeout, "socket-read-timeout", 5*time.Minute, "Per-read socket timeout (maps to JDBC socketTimeout)")
	f.DurationVar(&importCfg.SocketWriteTimeout, "socket-write-timeout", 5*time.Minute, "Per-write socket timeout (Go-only; no JDBC equivalent)")

	// Progress cadence
	f.DurationVar(&importCfg.ProgressInterval, "progress-interval", config.DefaultProgressInterval,
		"Periodic progress log interval (min 100ms; --quiet drops it)")

	// Session variables (repeatable)
	f.StringArrayVar(&importCfg.SessionVars, "session-var", nil,
		"Set a session variable (repeatable), e.g. sql_mode=STRICT_TRANS_TABLES")

	// Charset
	f.StringVar(&importDefaultCharset, "default-charset", "",
		"Connection & input-file charset (utf8mb4 default)")

	// Import-specific options
	f.StringVarP(&importCfg.Input, "input", "i", "", "Input file path (required)")
	f.StringVarP(&importCfg.Format, "format", "f", "del", "Input file format (del/sql/parquet)")
	f.StringVarP((*string)(&importCfg.Mode), "mode", "M", "insert", "insert, insert_update, replace (clears the table), create")
	f.StringVarP(&importCfg.Table, "table", "t", "", "Target table name")
	f.StringVarP(&importCfg.Columns, "columns", "c", "", "Target columns (one per input column) or file=table mapping (subset ok)")
	f.StringVarP(&messagesPath, "messages", "m", "", "Redirect the command log to this file; with it, nothing is printed to stderr")

	// DEL format modifiers
	f.StringVar(&importCfg.DelModifier.CharDel, "char-del", "\"", "String delimiter")
	f.StringVar(&importCfg.DelModifier.ColDel, "col-del", ",", "Column delimiter")
	f.StringVar(&importCfg.DelModifier.RowDel, "row-del", "\n", "Row delimiter")
	f.StringVar(&importCfg.DelModifier.Escape, "escape", "\"", "Escape character")
	f.BoolVar(&importCfg.DelModifier.NoEscape, "no-escape", false, "Disable escaping")
	f.StringVar(&importCfg.DelModifier.TimestampFmt, "timestamp-fmt", "yyyy-MM-dd HH:mm:ss", "Timestamp format")
	f.StringVar(&importCfg.DelModifier.DateFmt, "date-fmt", "yyyy-MM-dd", "Date format")
	f.BoolVar(&importCfg.DelModifier.NoCharDel, "no-char-del", false, "Do not use string delimiter")
	f.BoolVar(&importCfg.DelModifier.NoColNames, "no-col-names", false, "First line is not column names")

	// Import-specific DEL options
	f.IntVar(&importCfg.SkipCount, "skip-count", 0, "Skip first n data rows (header is consumed separately)")
	f.IntVar(&importCfg.RowCount, "row-count", 0, "Import at most n rows (0=unlimited)")
	f.IntVar(&importCfg.CommitCount, "commit-count", 1000, "Commit every n rows")

	// Pipeline hand-off controls
	f.IntVar(&importCfg.BatchSize, "batch-size", config.DefaultBatchSize, "Rows per INSERT batch")
	f.IntVar(&importCfg.PipelineBuffer, "pipeline-buffer", config.DefaultPipelineBuffer, "Hand-off queue depth in batches (0 = synchronous)")
	f.IntVar(&importCfg.StatementSize, "statement-size", 0, "Byte ceiling per generated INSERT (0 = server packet budget)")

	// Parallel import
	f.IntVar(&importCfg.ImportThreads, "threads", 1, "Number of parallel import tasks (1 = no parallelism)")

	// Log level (local, not persistent)
	f.StringVar((*string)(&importCfg.LogLevel), "log-level", "info", "Log level (debug/info/warn/error)")

	// Append each format-scoped option's scope to its own help text, from the
	// registry cmd/validate.go rejects with.
	annotateOptionScopes(cmd, config.ImportOptionScopes())
}
