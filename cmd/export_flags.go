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

	// Connection options
	f.StringVarP(&exportCfg.Host, "host", "h", "127.0.0.1", "Database host address")
	f.IntVarP(&exportCfg.Port, "port", "P", 3306, "TCP/IP port")
	f.StringVarP(&exportCfg.User, "user", "u", "root", "Database username")
	f.StringVarP(&exportCfg.Password, "password", "p", "", "Database password")
	f.StringVarP(&exportCfg.Socket, "socket", "S", "", "UNIX domain socket path")
	f.StringVarP(&exportCfg.Database, "database", "D", "", "Database name")

	// Timeouts
	f.DurationVar(&exportCfg.ConnectTimeout, "connect-timeout", 5*time.Second, "TCP connect timeout (maps to JDBC connectTimeout)")
	f.DurationVar(&exportCfg.SocketReadTimeout, "socket-read-timeout", 5*time.Minute, "Per-read socket timeout (maps to JDBC socketTimeout)")
	f.DurationVar(&exportCfg.SocketWriteTimeout, "socket-write-timeout", 5*time.Minute, "Per-write socket timeout (Go-only; no JDBC equivalent)")

	// Progress cadence
	f.DurationVar(&exportCfg.ProgressInterval, "progress-interval", config.DefaultProgressInterval,
		"How often to write the periodic progress record (must be positive; --quiet drops the records entirely)")

	// Session variables (repeatable)
	f.StringArrayVar(&exportCfg.SessionVars, "session-var", nil,
		"Set a MySQL/OceanBase session variable before export (repeatable), e.g. --session-var \"sql_mode=STRICT_TRANS_TABLES\" --session-var foreign_key_checks=0")

	// Charset (no default shown in oracle; struct keeps utf8mb4 from NewExportConfig)
	f.StringVar(&exportDefaultCharset, "default-charset", "",
		"Connection & output-file charset (utf8mb4 default; also utf8, latin1, ascii, gbk, gb18030)")

	// Export-specific options
	f.StringVarP(&exportCfg.Output, "output", "o", "", "Output file path (required)")
	f.StringVarP(&exportCfg.Format, "format", "f", "del", "Output file format (del/sql/parquet)")
	f.StringVarP(&exportCfg.Query, "query", "q", "", "SQL SELECT statement (mutually exclusive with --query-file)")
	f.StringVar(&exportCfg.QueryFile, "query-file", "", "Read the SQL SELECT statement from this file (mutually exclusive with -q)")
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
	f.IntVar(&exportCfg.StatementSize, "statement-size", config.DefaultStatementSize, "Max bytes per INSERT statement (only bounds merged statements: without --extended-insert a longer row just warns)")
	f.BoolVar(&exportCfg.ExtendedInsert, "extended-insert", false, "Merge rows into multi-row INSERT statements bounded by --statement-size")
	f.BoolVar(&exportCfg.Create, "create", false, "Output a CREATE TABLE statement (SQL export is data-only by default)")
	f.BoolVar(&exportCfg.NoData, "no-data", false, "Do not output INSERT statements")

	// Binary column encoding
	f.BoolVar(&exportCfg.HexBlob, "hex-blob", false, "Encode binary columns as 0x hexadecimal literals (auto-on for non-utf8mb4 charset)")
	f.BoolVar(&exportCfg.NoHexBlob, "no-hex-blob", false, "Force raw binary passthrough even for non-utf8mb4 charset (may corrupt binary data)")

	// Pipeline hand-off controls. Both are read only by the single-threaded path;
	// a parallel shard sizes its look-ahead from a fixed byte budget divided
	// across shards, so exportParallel reports when these were moved.
	f.IntVar(&exportCfg.BatchSize, "batch-size", config.DefaultBatchSize, "Rows per reader->writer hand-off batch (single-threaded export only; passing it with --threads>1 is a usage error)")
	f.IntVar(&exportCfg.PipelineBuffer, "pipeline-buffer", config.DefaultPipelineBuffer, "Hand-off queue depth, in batches (single-threaded export only; passing it with --threads>1 is a usage error)")

	// Parallel export
	f.IntVar(&exportCfg.Threads, "threads", 1, "Number of parallel export tasks (1 = no parallelism)")
	f.StringVar(&exportCfg.ShardKey, "shard-key", "", "Column name for sharding (auto-detect primary key if empty)")

	// Log level (local, not persistent)
	f.StringVar((*string)(&exportCfg.LogLevel), "log-level", "info", "Log level (debug/info/warn/error)")

	// Append each format-scoped option's scope to its own help text, from the
	// registry cmd/validate.go rejects with.
	annotateOptionScopes(cmd, config.ExportOptionScopes())
}
