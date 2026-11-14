package cmd

import (
	"strings"

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

	registerConnFlags(f, &exportCfg.Config, connFlagOpts{
		HelpLabel:         "help for export",
		DatabaseUsage:     "Database name (also IMEX_DATABASE)",
		CharsetTarget:     &exportDefaultCharset,
		CharsetDefault:    "",
		CharsetUsage:      "Connection & output-file charset (utf8mb4 default)",
		MessagesUsage:     "Redirect the command log to this file; with it, nothing is printed to stderr",
		OutputTarget:      &exportCfg.Output,
		NoOverwriteTarget: &exportCfg.NoOverwrite,
		OutputUsage:       "Output file path (required)",
	})

	// Progress cadence
	f.DurationVar(&exportCfg.ProgressInterval, "progress-interval", config.DefaultProgressInterval,
		"Periodic progress log interval (min 100ms)")

	// Session variables (repeatable)
	f.StringArrayVar(&exportCfg.SessionVars, "session-var", nil,
		"Set a session variable (repeatable), e.g. sql_mode=STRICT_TRANS_TABLES")

	// Export-specific options
	f.StringVarP(&exportCfg.Format, "format", "f", "del",
		"Output file format ("+enumNames(config.Formats())+")")
	f.StringVarP(&exportCfg.Query, "query", "q", "", "SQL SELECT statement, or @file to read it from a file")

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
		"SQL dump grammar ("+enumNames(config.SQLDialects())+")")

	// Parquet format modifiers
	f.StringVar(&exportCfg.ParquetCompression, "parquet-compression", config.ParquetCompressionNone,
		"Compression: "+strings.Join(config.ParquetCompressions(), ", "))

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

	// Append each format-scoped option's scope to its own help text, from the
	// registry cmd/validate.go rejects with.
	annotateOptionScopes(cmd, config.ExportOptionScopes())
}
