package export

import (
	"github.com/gomydump/internal/common"
	"github.com/gomydump/internal/config"
)

// effectiveHexBlob resolves the binary-column encoding for a run: --hex-blob
// forces it on, --no-hex-blob forces it off, and neither flag means automatic,
// which enables hex whenever the charset is not byte-safe.
//
// The single-threaded path, a parallel shard, and the configuration record all
// need the same answer, so it lives in one place here.
func effectiveHexBlob(cfg *config.ExportConfig) bool {
	return cfg.HexBlob || (!cfg.NoHexBlob && config.HexBlobForCharset(cfg.DefaultCharset))
}

// logExportConfig writes the one record that says what the run is about to do.
//
// It is filtered by --format through the same registry cmd/validate.go rejects
// with (config.ExportOptionScopes), so a field appears exactly when the chosen
// format reads it: a --format sql run no longer prints the DEL delimiters it
// ignored, and a --format del run no longer prints --statement-size, which only
// the SQL writer bounds. Because the rejection check runs first, the fields left
// here are the ones whose value the operator actually chose.
//
// One record per command is the contract the --messages file layout depends on.
func logExportConfig(cfg *config.ExportConfig) {
	common.Logger.Info("export config", exportConfigFields(cfg)...)
}

// exportConfigFields resolves the record's key/value pairs, in reading order:
// connection, input and output, parallelism and hand-off, charset, the format's
// own layout, logging. Splitting it out of the log call keeps the format filter
// assertable without a logger.
func exportConfigFields(cfg *config.ExportConfig) []interface{} {
	hex := effectiveHexBlob(cfg)

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

		// Input and output
		{"output", "output", cfg.Output},
		{"format", "format", cfg.Format},
		{"query", "query", cfg.Query},
		{"no-replace", "no_replace", cfg.NoReplace},

		// Parallelism and hand-off
		{"threads", "threads", cfg.Threads},
		{"shard-key", "shard_key", cfg.ShardKey},
		{"batch-size", "batch_size", cfg.BatchSize},
		{"pipeline-buffer", "pipeline_buffer", cfg.PipelineBuffer},

		// Charset
		{"default-charset", "default_charset", cfg.DefaultCharset},

		// SQL layout
		{"no-create", "no_create", cfg.NoCreate},
		{"no-data", "no_data", cfg.NoData},
		{"statement-size", "statement_size", cfg.StatementSize},
		{"extended-insert", "extended_insert", cfg.ExtendedInsert},

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

		// Binary encoding, as the run will actually apply it: the flag pair says
		// what was asked for, this says what the writer will do.
		{"hex-blob", "hex_blob", hex},

		// Logging
		{"log-level", "log_level", cfg.LogLevel},
		{"progress-interval", "progress_interval", cfg.EffectiveProgressInterval()},
	}

	scopes := config.ExportOptionScopes()
	args := make([]interface{}, 0, len(fields)*2)
	for _, f := range fields {
		if !scopes.AppliesTo(f.flag, cfg.Format) {
			continue
		}
		args = append(args, f.key, f.value)
	}
	return args
}
