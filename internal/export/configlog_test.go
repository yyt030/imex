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
		"char_del", "col_del", "row_del", "escape",
		"timestamp_fmt", "date_fmt", "no_col_names",
	}
	sqlLayoutKeys = []string{"statement_size", "extended_insert"}
	// parquetLayoutKeys mirrors the parquet-only scope entry: the codec name is
	// the only layout question the Parquet writer answers.
	parquetLayoutKeys = []string{"parquet_compression"}
	commonKeys        = []string{
		"host", "port", "user", "database",
		"output", "format", "query", "no_overwrite",
		"threads", "shard_key", "batch_size", "pipeline_buffer", "snapshot_consistency",
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
		for _, field := range parquetLayoutKeys {
			_, present := keys[field]
			if want := format == config.FormatParquet; present != want {
				t.Errorf("--format %s record carries Parquet field %q: present=%v", format, field, present)
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

// TestExportConfigRecordCarriesDateFmtAndLiteralModes names the regression this
// change fixes on the missing side: --date-fmt has always been read by the DEL
// writer, but only the other DEL fields were printed, so a run using it looked
// like a run using the defaults. Both "write the value as it arrived" modes are
// now visible as an empty value of the option that selects them: char_del ""
// (was --no-char-del) and escape "" (was --no-escape).
func TestExportConfigRecordCarriesDateFmtAndLiteralModes(t *testing.T) {
	cfg := config.NewExportConfig()
	cfg.Format = config.FormatDel
	cfg.DelModifier.DateFmt = "yyyyMMdd"
	cfg.DelModifier.Escape = ""
	cfg.DelModifier.CharDel = ""

	keys := configKeys(t, exportConfigFields(cfg))
	for _, field := range []string{"date_fmt", "char_del", "escape"} {
		if _, ok := keys[field]; !ok {
			t.Errorf("DEL record is missing %q", field)
		}
	}
	if keys["date_fmt"] != "yyyyMMdd" {
		t.Errorf("date_fmt = %v, want the value the run was given", keys["date_fmt"])
	}
	if keys["char_del"] != "" || keys["escape"] != "" {
		t.Errorf("char_del=%q escape=%q, want both empty: the run selects bare, unescaped values",
			keys["char_del"], keys["escape"])
	}
	for _, gone := range []string{"no_char_del", "no_escape"} {
		if _, ok := keys[gone]; ok {
			t.Errorf("the record still prints %q after the flag was folded into a delimiter value", gone)
		}
	}
}

// TestExportConfigRecordReportsHexBlobAsApplied: the option says what was asked for,
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
	forced.BlobEncoding = string(config.BlobEncodingRaw)
	if keys := configKeys(t, exportConfigFields(forced)); keys["hex_blob"] != false {
		t.Errorf("hex_blob = %v with --blob-encoding raw, want false: the value wins over the charset", keys["hex_blob"])
	}

	off := config.NewExportConfig()
	off.Format = config.FormatSQL
	if keys := configKeys(t, exportConfigFields(off)); keys["hex_blob"] != false {
		t.Errorf("hex_blob = %v with the default charset, want false", keys["hex_blob"])
	}
}

// TestEffectiveHexBlob pins the resolution the writer, the shard, and the
// configuration record now share. auto is the old "neither flag given", hex is
// the old --hex-blob, raw is the old --no-hex-blob; an empty encoding is a
// library caller's auto.
//
// The engine rows are the ones the answer moved out of: which engine must dump
// binary columns as 0x hex is the backend's fact (HexBlobMandatory), and the
// lookup that resolves it normalises the spelling — the last two rows are the
// 131 ruling, `--engine PG` and `--engine "  Pg  "` get the same answer as
// `--engine pg` instead of MySQL's charset rule.
func TestEffectiveHexBlob(t *testing.T) {
	cases := []struct {
		engine   string
		encoding string
		charset  string
		want     bool
	}{
		{"", "auto", "utf8mb4", false},
		{"", "auto", "utf8", false},
		{"", "auto", "gbk", true},
		{"", "auto", "latin1", true},
		{"", "", "utf8mb4", false},
		{"", "", "gbk", true},
		{"", "hex", "utf8mb4", true},
		{"", "hex", "gbk", true},
		{"", "raw", "utf8mb4", false},
		{"", "raw", "gbk", false},
		{"pg", "auto", "utf8mb4", true},
		{"pg", "auto", "gbk", true},
		{"pg", "raw", "utf8mb4", false},
		{"PG", "auto", "utf8mb4", true},
		{"  Pg  ", "auto", "utf8mb4", true},
	}
	for _, tc := range cases {
		cfg := config.NewExportConfig()
		cfg.Engine, cfg.BlobEncoding, cfg.DefaultCharset = tc.engine, tc.encoding, tc.charset
		if got := effectiveHexBlob(cfg); got != tc.want {
			t.Errorf("engine=%q blob-encoding=%q charset=%q: effectiveHexBlob=%v, want %v",
				tc.engine, tc.encoding, tc.charset, got, tc.want)
		}
	}
}
