package cmd

import (
	"strings"
	"time"

	"github.com/gomydump/internal/common"
	"github.com/gomydump/internal/config"
	"github.com/spf13/cobra"
)

// validatePort checks that the port is in the valid range 1–65535.
func validatePort(port int) error {
	if port < 1 || port > 65535 {
		return common.Usagef("invalid --port %d: must be between 1 and 65535", port)
	}
	return nil
}

// validateThreads checks that the thread count is in the valid range 1–256.
func validateThreads(threads int) error {
	if threads < 1 || threads > 256 {
		return common.Usagef("invalid --threads %d: must be between 1 and 256", threads)
	}
	return nil
}

// validateBatchSize checks that the batch size is in the valid range 1–100000.
func validateBatchSize(size int) error {
	if size < 1 || size > 100000 {
		return common.Usagef("invalid --batch-size %d: must be >= 1 and <= 100000", size)
	}
	return nil
}

// validatePipelineBuffer checks that the pipeline buffer is in the valid range 0–1024.
// 0 is legal and means synchronous hand-off.
func validatePipelineBuffer(buffer int) error {
	if buffer < 0 || buffer > 1024 {
		return common.Usagef("invalid --pipeline-buffer %d: must be >= 0 and <= 1024", buffer)
	}
	return nil
}

// validateImportStatementSize bounds the per-statement byte ceiling the importer
// may apply. 0 is legal here and means "no extra cap", which is why this is not
// validateStatementSize: on export the value is a real budget with a non-zero
// default, on import the default is the absence of a cap.
func validateImportStatementSize(size int) error {
	if size < 0 || size > 268435456 {
		return common.Usagef("invalid --statement-size %d: must be between 0 and 268435456 (0 means no extra cap)", size)
	}
	return nil
}

// validateStatementSize checks that the statement size is in the valid range >0 and ≤268435456.
func validateStatementSize(size int) error {
	if size <= 0 || size > 268435456 {
		return common.Usagef("invalid --statement-size %d: must be > 0 and <= 268435456", size)
	}
	return nil
}

// validatePositiveDuration checks that a timeout duration is >0.
func validatePositiveDuration(name string, d time.Duration) error {
	if d <= 0 {
		return common.Usagef("invalid --%s %s: must be > 0", name, d)
	}
	return nil
}

// validateCommitCount checks that the commit count is ≥1.
func validateCommitCount(count int) error {
	if count < 1 {
		return common.Usagef("invalid --commit-count %d: must be >= 1", count)
	}
	return nil
}

// validateSkipCount checks that the skip count is ≥0.
func validateSkipCount(count int) error {
	if count < 0 {
		return common.Usagef("invalid --skip-count %d: must be >= 0", count)
	}
	return nil
}

// validateRowCount checks that the row count is ≥0.
func validateRowCount(count int) error {
	if count < 0 {
		return common.Usagef("invalid --row-count %d: must be >= 0", count)
	}
	return nil
}

// validateLogLevel checks that the log level is one of debug/info/warn/error.
func validateLogLevel(level string) error {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug", "info", "warn", "error":
		return nil
	default:
		return common.Usagef("invalid --log-level %q: must be one of debug/info/warn/error", level)
	}
}

// validateLogFormat checks that the log format is valid.
func validateLogFormat(format string) error {
	if !common.ValidLogFormat(format) {
		return common.Usagef("invalid --log-format %q: must be one of java/text", format)
	}
	return nil
}

// exactArgs validates the number of positional arguments and, unlike cobra's
// own ExactArgs, reports a mismatch as a UsageError. main.go only escalates
// usage-classified errors to exit code 2, so a plain error from the args check
// would look like a runtime failure to scripts and to the exit-code contract.
func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			return common.Usagef("%s accepts %d argument(s), received %d", cmd.CommandPath(), n, len(args))
		}
		return nil
	}
}

// normalizeCharset lowers/trims a charset string for comparison.
func normalizeCharset(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// knownCharsets is the full set of accepted --default-charset values (aliases included).
var knownCharsets = map[string]bool{
	"utf8mb4": true, "utf8": true, "utf-8": true, "utf8mb3": true,
	"ascii": true, "us-ascii": true, "384": true,
	"latin1": true, "iso-8859-1": true, "819": true,
	"gbk": true, "936": true, "1386": true,
	"gb18030": true, "5488": true, "1392": true,
}

// sqlSafeCharsets are byte-escape-safe for SQL format.
var sqlSafeCharsets = map[string]bool{
	"utf8mb4": true, "utf8": true, "utf-8": true, "utf8mb3": true,
	"ascii": true, "us-ascii": true, "384": true,
	"latin1": true, "iso-8859-1": true, "819": true,
}

// validateCharset checks that the charset is recognized and compatible with the format.
func validateCharset(charset string, format string) error {
	cs := normalizeCharset(charset)

	if !knownCharsets[cs] {
		return common.Usagef("invalid --default-charset %q: unrecognized charset", charset)
	}

	switch format {
	case "parquet":
		if cs != "utf8mb4" {
			return common.Usagef("invalid --default-charset %q with --format parquet: parquet only supports utf8mb4", charset)
		}
	case "sql":
		if !sqlSafeCharsets[cs] {
			return common.Usagef("invalid --default-charset %q with --format sql: not byte-escape safe, use --format del instead", charset)
		}
	}
	return nil
}

// validateHexBlob checks that --hex-blob and --no-hex-blob are mutually exclusive.
func validateHexBlob(hexBlob, noHexBlob bool) error {
	if hexBlob && noHexBlob {
		return common.Usagef("--hex-blob and --no-hex-blob are mutually exclusive")
	}
	return nil
}

// validateInsertLayout keeps the two spellings of the SQL statement layout from
// asking for different things at once. One row per INSERT is the default now, so
// --single-row-insert is accepted and inert; --extended-insert opts back into
// merged statements. Passing both would leave the layout to flag order, which is
// exactly the kind of silent disagreement that a usage error exists to prevent.
func validateInsertLayout(cmd *cobra.Command) error {
	if cmd.Flags().Changed("single-row-insert") && cmd.Flags().Changed("extended-insert") {
		return common.Usagef("--single-row-insert and --extended-insert cannot be used together")
	}
	return nil
}

// validateOptionScopes rejects a format-scoped flag the user explicitly passed
// together with a format that does not read it. Which flags are scoped, the order
// a mismatch is reported in, the wording of the message and the marker the flag's
// own help text carries all come from one registry (internal/config/formatscope.go),
// which the `export/import config` echo reads as well. Silently ignoring such a
// flag would let a user believe the file was laid out or encoded differently than
// it is, so every one of them is a usage error instead.
func validateOptionScopes(cmd *cobra.Command, scopes config.OptionScopes, format string) error {
	if msg, bad := scopes.Violation(cmd.Flags().Changed, format); bad {
		return common.Usagef("%s", msg)
	}
	return nil
}

// validateFormat checks that the format is one of del/sql/parquet.
func validateFormat(format string) error {
	switch format {
	case "del", "sql", "parquet":
		return nil
	default:
		return common.Usagef("invalid --format %q: must be one of del/sql/parquet", format)
	}
}

// validateMode checks that the import mode is valid and compatible with the format.
func validateMode(mode string, format string) error {
	switch mode {
	case "insert", "insert_update", "replace", "create":
	default:
		return common.Usagef("invalid --mode %q: must be one of insert/insert_update/replace/create", mode)
	}
	if mode == "create" && format != "sql" {
		return common.Usagef("--mode create can only be used with --format sql")
	}
	return nil
}

// validateDelimiters parses and validates all delimiter options.
func validateDelimiters(charDel, colDel, rowDel, escape string) error {
	if _, err := common.ParseDelimiter(charDel); err != nil {
		return common.Usagef("invalid --char-del: %v", err)
	}
	if _, err := common.ParseDelimiter(colDel); err != nil {
		return common.Usagef("invalid --col-del: %v", err)
	}
	if _, err := common.ParseDelimiter(rowDel); err != nil {
		return common.Usagef("invalid --row-del: %v", err)
	}
	if _, err := common.ParseDelimiter(escape); err != nil {
		return common.Usagef("invalid --escape: %v", err)
	}
	return nil
}

// validateExportConfig performs all validation for export.
// Order: threads → batch-size → pipeline-buffer → rest.
func validateExportConfig(cmd *cobra.Command, cfg *config.ExportConfig) error {
	if err := validateThreads(cfg.Threads); err != nil {
		return err
	}
	if err := validateBatchSize(cfg.BatchSize); err != nil {
		return err
	}
	if err := validatePipelineBuffer(cfg.PipelineBuffer); err != nil {
		return err
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
	if err := validatePositiveDuration("progress-interval", cfg.ProgressInterval); err != nil {
		return err
	}
	if err := validateLogLevel(string(cfg.LogLevel)); err != nil {
		return err
	}
	if err := validateFormat(cfg.Format); err != nil {
		return err
	}
	if err := validateCharset(cfg.DefaultCharset, cfg.Format); err != nil {
		return err
	}
	if err := validateStatementSize(cfg.StatementSize); err != nil {
		return err
	}
	if err := validateHexBlob(cfg.HexBlob, cfg.NoHexBlob); err != nil {
		return err
	}
	if err := validateInsertLayout(cmd); err != nil {
		return err
	}
	if err := validateOptionScopes(cmd, config.ExportOptionScopes(), cfg.Format); err != nil {
		return err
	}
	return validateDelimiters(cfg.DelModifier.CharDel, cfg.DelModifier.ColDel, cfg.DelModifier.RowDel, cfg.DelModifier.Escape)
}

// validateImportConfig performs all validation for import.
// Order: threads → batch-size → pipeline-buffer → rest.
func validateImportConfig(cmd *cobra.Command, cfg *config.ImportConfig) error {
	if err := validateThreads(cfg.ImportThreads); err != nil {
		return err
	}
	if err := validateBatchSize(cfg.BatchSize); err != nil {
		return err
	}
	if err := validatePipelineBuffer(cfg.PipelineBuffer); err != nil {
		return err
	}
	if err := validateImportStatementSize(cfg.StatementSize); err != nil {
		return err
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
	if err := validatePositiveDuration("progress-interval", cfg.ProgressInterval); err != nil {
		return err
	}
	if err := validateLogLevel(string(cfg.LogLevel)); err != nil {
		return err
	}
	if err := validateFormat(cfg.Format); err != nil {
		return err
	}
	if err := validateCharset(cfg.DefaultCharset, cfg.Format); err != nil {
		return err
	}
	if err := validateMode(string(cfg.Mode), cfg.Format); err != nil {
		return err
	}
	if err := validateCommitCount(cfg.CommitCount); err != nil {
		return err
	}
	if err := validateSkipCount(cfg.SkipCount); err != nil {
		return err
	}
	if err := validateRowCount(cfg.RowCount); err != nil {
		return err
	}
	if err := validateOptionScopes(cmd, config.ImportOptionScopes(), cfg.Format); err != nil {
		return err
	}
	return validateDelimiters(cfg.DelModifier.CharDel, cfg.DelModifier.ColDel, cfg.DelModifier.RowDel, cfg.DelModifier.Escape)
}
