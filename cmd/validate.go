package cmd

import (
	"strings"
	"time"

	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
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

// validateParquetCompression checks the codec name before the writer sees it, so
// a typo is a usage error (exit 2) rather than an error raised after the file has
// been created.
func validateParquetCompression(codec string) error {
	if !config.ValidParquetCompression(codec) {
		return common.Usagef("invalid --parquet-compression %q: must be one of none/snappy/zstd", codec)
	}
	return nil
}

// validateLogFormat checks that the log format is valid.
func validateLogFormat(format string) error {
	if !common.ValidLogFormat(format) {
		return common.Usagef("invalid --log-format %q: must be one of console/text", format)
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

// validateProgressInterval checks the periodic progress cadence. A non-positive
// value would make time.NewTicker panic and a zero default would silently disable
// the heartbeat, so both are refused; the lower bound exists because the ticker
// only fires as fast as its consumer drains it, and an interval below a batch
// turns the progress record into a per-batch log line (measured: 200,000 rows
// import in 200 batches, ~7ms each; the 1ns case wrote 780 records and 132 KB
// for a 10-row, 4ms import).
func validateProgressInterval(d time.Duration) error {
	if d <= 0 {
		return common.Usagef("invalid --progress-interval %s: must be > 0", d)
	}
	if d < config.MinProgressInterval {
		return common.Usagef("invalid --progress-interval %s: must be >= %s", d, config.MinProgressInterval)
	}
	return nil
}

// validateDelimiters parses the four delimiter options and refuses pairs whose
// equality makes the layout ambiguous. Each option is checked on its own first,
// because an unparseable value must be reported as that value's own error.
//
// The collisions are refused rather than warned about for the same reason the
// format-scoped options are: the tool would otherwise write a file it cannot read
// back. --col-del "," with --row-del "," produced a 4-row dump that was one long
// line and imported with error "target table has no column(s)", and --char-del ","
// produced ",id,,,v," — both exit 0 with no diagnostic, so the failure surfaced
// only later, in whoever tried to use the file. --escape equal to --char-del stays
// legal: it is the shipped default, and a delimiter that also escapes its own
// string quote is how MySQL doubles it.
func validateDelimiters(charDel, colDel, rowDel, escape string, noCharDel, noEscape bool) error {
	// Slice, not map: the order an unparseable delimiter is reported in is part of
	// the error-text contract.
	names := []string{"char-del", "col-del", "row-del", "escape"}
	raws := []string{charDel, colDel, rowDel, escape}
	chars := make(map[string]string, len(names))
	for i, name := range names {
		v, err := common.ParseDelimiter(raws[i])
		if err != nil {
			return common.Usagef("invalid --%s: %v", name, err)
		}
		chars[name] = v
	}

	conflicts := []struct{ a, b string }{
		{"col-del", "row-del"},
		{"char-del", "col-del"},
		{"char-del", "row-del"},
		{"escape", "col-del"},
		{"escape", "row-del"},
	}
	for _, c := range conflicts {
		if chars[c.a] != chars[c.b] {
			continue
		}
		// A disabled option is not part of the layout, so it cannot collide.
		if (c.a == "char-del" && noCharDel) || (c.a == "escape" && noEscape) {
			continue
		}
		return common.Usagef("invalid --%s %q: it is the same as --%s, which makes the file layout ambiguous",
			c.a, chars[c.a], c.b)
	}
	return nil
}

// validateParallelExportOptions rejects the two hand-off options on a run that
// will not read them. The parallel export path serialises each shard straight
// into its pipe: there is no reader->writer batch to size and no queue to
// deepen, so accepting the flags would mean storing values nothing reads and
// reporting success to a user whose instruction was silently dropped. Only an
// explicitly passed flag counts, so the defaults stay usable together with
// --threads.
//
// The library still logs this case (see pipelineOptionsIgnoredByParallel),
// because a caller building ExportConfig by hand has no "was this set" to ask.
func validateParallelExportOptions(cmd *cobra.Command, threads int) error {
	if threads <= 1 {
		return nil
	}
	for _, name := range parallelExportIgnoredOptions {
		if cmd.Flags().Changed(name) {
			return common.Usagef("--%s only applies to a single-threaded export: with --threads %d every shard streams its own rows straight to the output, so the option changes nothing. Drop --%s, or use --threads 1",
				name, threads, name)
		}
	}
	return nil
}

// parallelExportIgnoredOptions are the export flags the sharded path never
// reads, in the order they are reported.
var parallelExportIgnoredOptions = []string{"batch-size", "pipeline-buffer"}

// validateExportConfig performs all validation for export.
// Order: threads → batch-size → pipeline-buffer → parallel-path conflicts → rest.
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
	if err := validateParallelExportOptions(cmd, cfg.Threads); err != nil {
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
	if err := validateProgressInterval(cfg.ProgressInterval); err != nil {
		return err
	}
	if err := validateLogLevel(string(cfg.LogLevel)); err != nil {
		return err
	}
	if err := validateFormat(cfg.Format); err != nil {
		return err
	}
	if err := validateParquetCompression(cfg.ParquetCompression); err != nil {
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
	if err := validateOptionScopes(cmd, config.ExportOptionScopes(), cfg.Format); err != nil {
		return err
	}
	return validateDelimiters(cfg.DelModifier.CharDel, cfg.DelModifier.ColDel, cfg.DelModifier.RowDel,
		cfg.DelModifier.Escape, cfg.DelModifier.NoCharDel, cfg.DelModifier.NoEscape)
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
	if err := validateProgressInterval(cfg.ProgressInterval); err != nil {
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
	return validateDelimiters(cfg.DelModifier.CharDel, cfg.DelModifier.ColDel, cfg.DelModifier.RowDel,
		cfg.DelModifier.Escape, cfg.DelModifier.NoCharDel, cfg.DelModifier.NoEscape)
}
