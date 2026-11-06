package cmd

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/imex/internal/config"
)

// exportDefaultCharset is a temp variable for the --default-charset flag.
// The struct field exportCfg.DefaultCharset already has "utf8mb4" from NewExportConfig().
// We use "" as the flag default so cobra doesn't show "(default ...)" in help.
// In RunE, if this is non-empty, we override the struct field.
var exportDefaultCharset string

// registerExportFlags registers all flags for the export subcommand.
// Descriptions and defaults follow docs/cli-oracle.txt apart from the
// deltas registered in that file's header.
func registerExportFlags(cmd *cobra.Command) {
	f := cmd.Flags()

	// Override default help flag to free up -h for --host.
	f.BoolP("help", "?", false, "help for export")

	// Connection options; each also reads IMEX_* when the flag is not given.
	f.StringVar(&exportCfg.Engine, "engine", "mysql", "Database engine: mysql, pg, dm")
	f.StringVarP(&exportCfg.Host, "host", "h", "127.0.0.1", "Database host address (also IMEX_HOST)")
	f.IntVarP(&exportCfg.Port, "port", "P", 3306, "TCP/IP port (also IMEX_PORT)")
	f.StringVarP(&exportCfg.User, "user", "u", "root", "Database username (also IMEX_USER)")
	f.StringVarP(&exportCfg.Password, "password", "p", "", "Database password (also IMEX_PASSWORD)")
	f.StringVarP(&exportCfg.Database, "database", "D", "", "Database name (also IMEX_DATABASE)")

	// Timeouts
	f.DurationVar(&exportCfg.ConnectTimeout, "connect-timeout", 5*time.Second, "TCP connect timeout")
	f.DurationVar(&exportCfg.SocketReadTimeout, "socket-read-timeout", 5*time.Minute, "Per-read socket timeout")
	f.DurationVar(&exportCfg.SocketWriteTimeout, "socket-write-timeout", 5*time.Minute, "Per-write socket timeout")

	// Progress cadence
	f.DurationVar(&exportCfg.ProgressInterval, "progress-interval", config.DefaultProgressInterval,
		"Periodic progress log interval (min 100ms)")

	// Session variables (repeatable)
	f.StringArrayVar(&exportCfg.SessionVars, "session-var", nil,
		"Set a session variable (repeatable), e.g. sql_mode=STRICT_TRANS_TABLES")

	// Charset (no default shown in oracle; struct keeps utf8mb4 from NewExportConfig)
	f.StringVar(&exportDefaultCharset, "default-charset", "",
		"Connection & output-file charset (utf8mb4 default)")

	// Export-specific options
	f.StringVarP(&exportCfg.Output, "output", "o", "", "Output file path (required)")
	f.StringVarP(&exportCfg.Format, "format", "f", "del", "Output file format (del/sql/parquet)")
	f.StringVarP(&exportCfg.Query, "query", "q", "", "SQL SELECT statement, or @file to read it from a file")
	f.StringVarP(&messagesPath, "messages", "m", "", "Redirect the command log to this file; with it, nothing is printed to stderr")
	f.BoolVar(&exportCfg.NoOverwrite, "no-overwrite", false, "Refuse to overwrite an existing output file (default: overwrite)")

	// DEL format modifiers
	f.StringVar(&exportCfg.DelModifier.CharDel, "char-del", "\"", "String delimiter")
	f.StringVar(&exportCfg.DelModifier.ColDel, "col-del", ",", "Column delimiter")
	f.StringVar(&exportCfg.DelModifier.RowDel, "row-del", "\n", "Row delimiter")
	f.StringVar(&exportCfg.DelModifier.Escape, "escape", "\"", "Escape character")
	f.BoolVar(&exportCfg.DelModifier.NoEscape, "no-escape", false, "Disable escaping")
	f.StringVar(&exportCfg.DelModifier.TimestampFmt, "timestamp-fmt", "yyyy-MM-dd HH:mm:ss", "Timestamp format")
	f.StringVar(&exportCfg.DelModifier.DateFmt, "date-fmt", "yyyy-MM-dd", "Date format")
	f.BoolVar(&exportCfg.DelModifier.NoCharDel, "no-char-del", false, "Do not use string delimiter")
	f.BoolVar(&exportCfg.DelModifier.NoColNames, "no-col-names", false, "Do not output column names")

	// SQL format modifiers
	f.IntVar(&exportCfg.StatementSize, "statement-size", config.DefaultStatementSize, "Max bytes per merged INSERT")
	f.BoolVar(&exportCfg.ExtendedInsert, "extended-insert", false, "Merge rows into multi-row INSERTs")
	f.StringVar(&exportCfg.SQLDialect, "sql-dialect", string(config.SQLDialectAuto),
		"SQL dump grammar (auto/mysql/pg)")

	// Parquet format modifiers
	f.StringVar(&exportCfg.ParquetCompression, "parquet-compression", config.ParquetCompressionNone,
		"Compression: none, snappy, zstd")

	// Binary column encoding
	f.BoolVar(&exportCfg.HexBlob, "hex-blob", false, "Binary columns as 0x hex literals")
	f.BoolVar(&exportCfg.NoHexBlob, "no-hex-blob", false, "Force raw binary passthrough (may corrupt data)")

	// Pipeline hand-off controls. Both are read only by the single-threaded path;
	// a parallel shard sizes its look-ahead from a fixed byte budget divided
	// across shards, so exportParallel reports when these were moved.
	f.IntVar(&exportCfg.BatchSize, "batch-size", config.DefaultBatchSize, "Rows per hand-off batch; with --threads>1 it is a usage error")
	f.IntVar(&exportCfg.PipelineBuffer, "pipeline-buffer", config.DefaultPipelineBuffer, "Hand-off queue depth in batches; --threads>1 is a usage error")

	// Parallel export
	f.IntVar(&exportCfg.Threads, "threads", 1, "Number of parallel export tasks (1 = no parallelism)")
	f.StringVar(&exportCfg.ShardKey, "shard-key", "", "Column used to shard a parallel export; defaults to the primary key")

	// Log level (local, not persistent)
	f.StringVar((*string)(&exportCfg.LogLevel), "log-level", "info", "Log level (debug/info/warn/error)")

	// Append each format-scoped option's scope to its own help text, from the
	// registry cmd/validate.go rejects with.
	annotateOptionScopes(cmd, config.ExportOptionScopes())
}
