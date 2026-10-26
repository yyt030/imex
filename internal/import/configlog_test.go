package _import

import (
	"testing"

	"github.com/gomydump/internal/config"
)

// configKeys lists the field names of a resolved configuration record, and fails
// the test if the pairs are not pairs: a record with an odd number of arguments is
// a record slog will report as malformed.
func configKeys(t *testing.T, args []interface{}) map[string]interface{} {
	t.Helper()
	if len(args)%2 != 0 {
		t.Fatalf("configuration record has %d arguments, which is not a whole number of key/value pairs", len(args))
	}
	out := make(map[string]interface{}, len(args)/2)
	for i := 0; i < len(args); i += 2 {
		key, ok := args[i].(string)
		if !ok {
			t.Fatalf("argument %d of the configuration record is %T, want a key string", i, args[i])
		}
		if _, dup := out[key]; dup {
			t.Errorf("configuration record prints %q twice", key)
		}
		out[key] = args[i+1]
	}
	return out
}

// delLayoutKeys are the fields only a DEL input reader has use for; alwaysKeys are
// the fields every format is given, including the ones an import applies to the
// row stream rather than to a file.
var (
	delLayoutKeys = []string{
		"char_del", "col_del", "row_del", "escape", "no_escape", "no_char_del",
		"timestamp_fmt", "date_fmt", "no_col_names",
	}
	alwaysKeys = []string{
		"host", "port", "user", "database",
		"input", "bytes", "format", "table", "columns", "mode",
		"skip_count", "row_count", "commit_count",
		"threads", "batch_size", "pipeline_buffer", "statement_size",
		"default_charset",
		"log_level", "progress_interval",
	}
)

func TestImportConfigRecordIsFilteredByFormat(t *testing.T) {
	for _, format := range []string{config.FormatDel, config.FormatSQL, config.FormatParquet} {
		cfg := config.NewImportConfig()
		cfg.Format = format
		keys := configKeys(t, importConfigFields(cfg))

		for _, want := range alwaysKeys {
			if _, ok := keys[want]; !ok {
				t.Errorf("--format %s record is missing %q, which every input format goes through", format, want)
			}
		}
		if got := keys["format"]; got != format {
			t.Errorf("--format %s record reports format=%v", format, got)
		}

		for _, field := range delLayoutKeys {
			_, present := keys[field]
			if want := format == config.FormatDel; present != want {
				t.Errorf("--format %s record carries DEL field %q: present=%v", format, field, present)
			}
		}
	}
}

// TestImportStatementSizeIsAlwaysReported keeps the option added for the importer
// visible for every input format: it bounds the statements this command builds,
// not the file it reads, so filtering it by format would hide a value that is in
// effect for a SQL or Parquet import as well.
func TestImportStatementSizeIsAlwaysReported(t *testing.T) {
	for _, format := range []string{config.FormatDel, config.FormatSQL, config.FormatParquet} {
		cfg := config.NewImportConfig()
		cfg.Format = format
		cfg.StatementSize = 65536
		keys := configKeys(t, importConfigFields(cfg))
		if keys["statement_size"] != 65536 {
			t.Errorf("--format %s record reports statement_size=%v, want 65536", format, keys["statement_size"])
		}
	}
}

// TestImportConfigRecordCarriesTheThreeFieldsItUsedToOmit names the regression on
// the missing side: --date-fmt, --no-escape and --no-char-del are read by the DEL
// reader, but the record never printed them, so a run that moved them looked
// indistinguishable from one that did not.
func TestImportConfigRecordCarriesTheThreeFieldsItUsedToOmit(t *testing.T) {
	cfg := config.NewImportConfig()
	cfg.DelModifier.DateFmt = "yyyyMMdd"
	cfg.DelModifier.NoEscape = true
	cfg.DelModifier.NoCharDel = true

	keys := configKeys(t, importConfigFields(cfg))
	for _, field := range []string{"date_fmt", "no_escape", "no_char_del"} {
		if _, ok := keys[field]; !ok {
			t.Errorf("DEL record is missing %q", field)
		}
	}
	if keys["date_fmt"] != "yyyyMMdd" {
		t.Errorf("date_fmt = %v, want the value the run was given", keys["date_fmt"])
	}
}
