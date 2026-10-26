package _import

import (
	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

// logImportConfig writes the one record that says what the run is about to do.
//
// It is filtered by --format through the same registry cmd/validate.go rejects
// with (config.ImportOptionScopes), so the DEL layout only appears for a reader
// that parses a delimited file. The fields that survive for every format are not
// format-specific by accident: --skip-count, --row-count and --commit-count act
// on the row stream once the reader has produced it, and --statement-size bounds
// the batches whatever the input file looks like.
//
// One record per command is the contract the --messages file layout depends on.
func logImportConfig(cfg *config.ImportConfig) {
	common.Logger.Info("import config", importConfigFields(cfg)...)
}

// importConfigFields resolves the record's key/value pairs, in reading order:
// connection, input and target, the row-stream controls, parallelism and
// hand-off, charset, the format's own layout, logging. Splitting it out of the
// log call keeps the format filter assertable without a logger.
func importConfigFields(cfg *config.ImportConfig) []interface{} {
	fields := []struct {
		flag  string // the command-line option this field reports
		key   string
		value interface{}
	}{
		// Connection
		{"host", "host", cfg.Host},
		{"port", "port", cfg.Port},
		{"user", "user", cfg.User},
		{"database", "database", cfg.Database},

		// Input file and target table, plus the file as it sits on disk
		{"input", "input", cfg.Input},
		{"input", "bytes", common.FileSize(cfg.Input)},
		{"format", "format", cfg.Format},
		{"table", "table", cfg.Table},
		{"columns", "columns", cfg.Columns},
		{"mode", "mode", cfg.Mode},

		// Row-stream controls
		{"skip-count", "skip_count", cfg.SkipCount},
		{"row-count", "row_count", cfg.RowCount},
		{"commit-count", "commit_count", cfg.CommitCount},

		// Parallelism and hand-off
		{"threads", "threads", cfg.ImportThreads},
		{"batch-size", "batch_size", cfg.BatchSize},
		{"pipeline-buffer", "pipeline_buffer", cfg.PipelineBuffer},
		{"statement-size", "statement_size", cfg.StatementSize},

		// Charset
		{"default-charset", "default_charset", cfg.DefaultCharset},

		// DEL layout
		{"char-del", "char_del", cfg.DelModifier.CharDel},
		{"col-del", "col_del", cfg.DelModifier.ColDel},
		{"row-del", "row_del", cfg.DelModifier.RowDel},
		{"escape", "escape", cfg.DelModifier.Escape},
		{"no-escape", "no_escape", cfg.DelModifier.NoEscape},
		{"no-char-del", "no_char_del", cfg.DelModifier.NoCharDel},
		{"timestamp-fmt", "timestamp_fmt", cfg.DelModifier.TimestampFmt},
		{"date-fmt", "date_fmt", cfg.DelModifier.DateFmt},
		{"no-col-names", "no_col_names", cfg.DelModifier.NoColNames},

		// Logging
		{"log-level", "log_level", cfg.LogLevel},
		{"progress-interval", "progress_interval", cfg.EffectiveProgressInterval()},
	}

	scopes := config.ImportOptionScopes()
	args := make([]interface{}, 0, len(fields)*2)
	for _, f := range fields {
		if !scopes.AppliesTo(f.flag, cfg.Format) {
			continue
		}
		args = append(args, f.key, f.value)
	}
	return args
}
