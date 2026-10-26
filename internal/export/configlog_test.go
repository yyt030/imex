package export

import (
	"testing"

	"github.com/imex/internal/config"
)

// configKeys lists the field names of a resolved configuration record, and fails
// the test if the pairs are not pairs: a record with an odd number of arguments
// is a record slog will report as malformed.
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

// delLayoutKeys are the fields only a DEL run should carry; sqlLayoutKeys are the
// fields only an SQL run should carry. They mirror the format-scoped groups of the
// registry, under the key names the record uses.
var (
	delLayoutKeys = []string{
		"char_del", "col_del", "row_del", "escape", "no_escape", "no_char_del",
		"timestamp_fmt", "date_fmt", "no_col_names",
	}
	sqlLayoutKeys = []string{"create", "no_data", "statement_size", "extended_insert"}
	commonKeys    = []string{
		"host", "port", "user", "database",
		"output", "format", "query", "no_replace",
		"threads", "shard_key", "batch_size", "pipeline_buffer",
		"default_charset",
		"log_level", "progress_interval",
	}
)

// TestExportConfigRecordIsFilteredByFormat is the point of the format-scope work
// on the printing side: what an operator sees has to be what the chosen format
// reads.
func TestExportConfigRecordIsFilteredByFormat(t *testing.T) {
	for _, format := range []string{config.FormatDel, config.FormatSQL, config.FormatParquet} {
		cfg := config.NewExportConfig()
		cfg.Format = format
		keys := configKeys(t, exportConfigFields(cfg))

		for _, want := range commonKeys {
			if _, ok := keys[want]; !ok {
				t.Errorf("--format %s record is missing %q, which the format does not depend on", format, want)
			}
		}
		if got, ok := keys["format"]; !ok || got != format {
			t.Errorf("--format %s record reports format=%v", format, got)
		}

		for _, field := range delLayoutKeys {
			_, present := keys[field]
			if want := format == config.FormatDel; present != want {
				t.Errorf("--format %s record carries DEL field %q: present=%v", format, field, present)
			}
		}
		for _, field := range sqlLayoutKeys {
			_, present := keys[field]
			if want := format == config.FormatSQL; present != want {
				t.Errorf("--format %s record carries SQL field %q: present=%v", format, field, present)
			}
		}

		// Binary encoding is a layout question the parquet writer cannot answer,
		// so the record drops it there as well.
		_, hex := keys["hex_blob"]
		if want := format != config.FormatParquet; hex != want {
			t.Errorf("--format %s record carries hex_blob: %v", format, hex)
		}
	}
}

// TestExportConfigRecordCarriesTheThreeFieldsItUsedToOmit names the regression
// this change fixes on the missing side: --date-fmt, --no-escape and --no-char-del
// have always been read by the DEL writer, but only the other DEL fields were
// printed, so a run using them looked like a run using the defaults.
func TestExportConfigRecordCarriesTheThreeFieldsItUsedToOmit(t *testing.T) {
	cfg := config.NewExportConfig()
	cfg.Format = config.FormatDel
	cfg.DelModifier.DateFmt = "yyyyMMdd"
	cfg.DelModifier.NoEscape = true
	cfg.DelModifier.NoCharDel = true

	keys := configKeys(t, exportConfigFields(cfg))
	for _, field := range []string{"date_fmt", "no_escape", "no_char_del"} {
		if _, ok := keys[field]; !ok {
			t.Errorf("DEL record is missing %q", field)
		}
	}
	if keys["date_fmt"] != "yyyyMMdd" {
		t.Errorf("date_fmt = %v, want the value the run was given", keys["date_fmt"])
	}
	if keys["no_escape"] != true || keys["no_char_del"] != true {
		t.Errorf("no_escape=%v no_char_del=%v, want both true", keys["no_escape"], keys["no_char_del"])
	}
}

// TestExportConfigRecordReportsHexBlobAsApplied: the flags say what was asked for,
// the record has to say what the writer will do.
func TestExportConfigRecordReportsHexBlobAsApplied(t *testing.T) {
	auto := config.NewExportConfig()
	auto.Format = config.FormatDel
	auto.DefaultCharset = "gbk"
	if keys := configKeys(t, exportConfigFields(auto)); keys["hex_blob"] != true {
		t.Errorf("hex_blob = %v with charset gbk and neither flag given, want true", keys["hex_blob"])
	}

	forced := config.NewExportConfig()
	forced.Format = config.FormatSQL
	forced.DefaultCharset = "gbk"
	forced.NoHexBlob = true
	if keys := configKeys(t, exportConfigFields(forced)); keys["hex_blob"] != false {
		t.Errorf("hex_blob = %v with --no-hex-blob, want false: the flag wins over the charset", keys["hex_blob"])
	}

	off := config.NewExportConfig()
	off.Format = config.FormatSQL
	if keys := configKeys(t, exportConfigFields(off)); keys["hex_blob"] != false {
		t.Errorf("hex_blob = %v with the default charset, want false", keys["hex_blob"])
	}
}

// TestEffectiveHexBlob pins the resolution the writer, the shard, and the
// configuration record now share.
func TestEffectiveHexBlob(t *testing.T) {
	cases := []struct {
		hexBlob, noHexBlob bool
		charset            string
		want               bool
	}{
		{false, false, "utf8mb4", false},
		{false, false, "utf8", false},
		{false, false, "gbk", true},
		{false, false, "latin1", true},
		{true, false, "utf8mb4", true},
		{false, true, "gbk", false},
	}
	for _, tc := range cases {
		cfg := config.NewExportConfig()
		cfg.HexBlob, cfg.NoHexBlob, cfg.DefaultCharset = tc.hexBlob, tc.noHexBlob, tc.charset
		if got := effectiveHexBlob(cfg); got != tc.want {
			t.Errorf("hexBlob=%v noHexBlob=%v charset=%q: effectiveHexBlob=%v, want %v",
				tc.hexBlob, tc.noHexBlob, tc.charset, got, tc.want)
		}
	}
}
