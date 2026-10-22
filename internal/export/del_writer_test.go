package export

import (
	"bytes"
	"database/sql"

	"github.com/gomydump/internal/common"
	"strings"
	"testing"
	"time"

	"github.com/gomydump/internal/config"
)

func TestDelWriter_WriteHeader(t *testing.T) {
	tests := []struct {
		name      string
		columns   []string
		writeCols bool
		expected  string
	}{
		{
			name:      "write column names",
			columns:   []string{"id", "name", "email"},
			writeCols: true,
			expected:  "\"id\",\"name\",\"email\"\n",
		},
		{
			name:      "skip column names",
			columns:   []string{"id", "name", "email"},
			writeCols: false,
			expected:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			mod := &config.DelModifier{
				CharDel: `"`,
				ColDel:  ",",
				RowDel:  "\n",
				Escape:  `"`,
			}
			writer := NewDelWriter(&buf, mod, tt.writeCols, nil)

			err := writer.WriteHeader(tt.columns)
			if err != nil {
				t.Errorf("WriteHeader() error = %v", err)
				return
			}
			writer.WriteFooter()

			if buf.String() != tt.expected {
				t.Errorf("WriteHeader() = %q, want %q", buf.String(), tt.expected)
			}
		})
	}
}

func TestDelWriter_WriteRow(t *testing.T) {
	tests := []struct {
		name     string
		values   []interface{}
		mod      *config.DelModifier
		expected string
	}{
		{
			name:   "string values with default delimiters",
			values: []interface{}{"Alice", "Bob", "Charlie"},
			mod: &config.DelModifier{
				CharDel: `"`,
				ColDel:  ",",
				RowDel:  "\n",
				Escape:  `"`,
			},
			expected: "\"Alice\",\"Bob\",\"Charlie\"\n",
		},
		{
			name:   "mixed types",
			values: []interface{}{1, "Alice", 3.14, nil, true},
			mod: &config.DelModifier{
				CharDel: `"`,
				ColDel:  ",",
				RowDel:  "\n",
				Escape:  `"`,
			},
			expected: "1,\"Alice\",3.14,,1\n",
		},
		{
			name:   "custom delimiters",
			values: []interface{}{"Alice", "Bob"},
			mod: &config.DelModifier{
				CharDel: `'`,
				ColDel:  "|",
				RowDel:  "\r\n",
				Escape:  `'`,
			},
			expected: "'Alice'|'Bob'\r\n",
		},
		{
			name:   "no char delimiter",
			values: []interface{}{"Alice", "Bob"},
			mod: &config.DelModifier{
				CharDel:   `"`,
				ColDel:    ",",
				RowDel:    "\n",
				Escape:    `"`,
				NoCharDel: true,
			},
			expected: "Alice,Bob\n",
		},
		{
			name:   "NULL values",
			values: []interface{}{nil, "test", nil},
			mod: &config.DelModifier{
				CharDel: `"`,
				ColDel:  ",",
				RowDel:  "\n",
				Escape:  `"`,
			},
			expected: ",\"test\",\n",
		},
		{
			name:   "escape double quotes",
			values: []interface{}{`Alice "The Great"`},
			mod: &config.DelModifier{
				CharDel: `"`,
				ColDel:  ",",
				RowDel:  "\n",
				Escape:  `"`,
			},
			expected: "\"Alice \"\"The Great\"\"\"\n",
		},
		{
			name:   "time value",
			values: []interface{}{time.Date(2026, 1, 15, 10, 30, 45, 0, time.UTC)},
			mod: &config.DelModifier{
				CharDel:      `"`,
				ColDel:       ",",
				RowDel:       "\n",
				Escape:       `"`,
				TimestampFmt: "yyyy-MM-dd HH:mm:ss",
			},
			expected: "\"2026-01-15 10:30:45\"\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			writer := NewDelWriter(&buf, tt.mod, false, nil)

			err := writer.WriteRow(tt.values)
			if err != nil {
				t.Errorf("WriteRow() error = %v", err)
				return
			}
			writer.Flush()

			if buf.String() != tt.expected {
				t.Errorf("WriteRow() = %q, want %q", buf.String(), tt.expected)
			}
		})
	}
}

func TestDelWriter_FormatFloat(t *testing.T) {
	tests := []struct {
		name     string
		value    float64
		expected string
	}{
		{
			name:     "positive float",
			value:    3.14159,
			expected: "3.14159",
		},
		{
			name:     "negative float",
			value:    -0.5,
			expected: "-0.5",
		},
		{
			name:     "integer float",
			value:    42.0,
			expected: "42",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			mod := &config.DelModifier{}
			writer := NewDelWriter(&buf, mod, false, nil)

			writer.formatFloat(tt.value)
			writer.bw.Flush()
			result := buf.String()
			if result != tt.expected {
				t.Errorf("formatFloat() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestConvertTimeFormat(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "standard timestamp format",
			input:    "yyyy-MM-dd HH:mm:ss",
			expected: "2006-01-02 15:04:05",
		},
		{
			name:     "date only",
			input:    "yyyy-MM-dd",
			expected: "2006-01-02",
		},
		{
			name:     "time only",
			input:    "HH:mm:ss",
			expected: "15:04:05",
		},
		{
			name:     "custom format with slashes",
			input:    "yyyy/MM/dd HH:mm:ss",
			expected: "2006/01/02 15:04:05",
		},
		{
			name:     "short year format",
			input:    "yy-MM-dd",
			expected: "06-01-02",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := convertTimeFormat(tt.input)
			if result != tt.expected {
				t.Errorf("convertTimeFormat() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestDelWriter_EscapeHandling(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		mod      *config.DelModifier
		expected string
	}{
		{
			name:  "double-write escape (default)",
			input: `test "quoted" value`,
			mod: &config.DelModifier{
				CharDel: `"`,
				Escape:  `"`,
			},
			expected: `"test ""quoted"" value"`,
		},
		{
			name:  "backslash escape",
			input: `test "quoted" value`,
			mod: &config.DelModifier{
				CharDel: `"`,
				Escape:  `\`,
			},
			expected: `"test \"quoted\" value"`,
		},
		{
			name:  "no escape",
			input: `test "quoted" value`,
			mod: &config.DelModifier{
				CharDel:  `"`,
				Escape:   `"`,
				NoEscape: true,
			},
			expected: `"test "quoted" value"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			writer := NewDelWriter(&buf, tt.mod, false, nil)

			writer.formatString(tt.input)
			writer.bw.Flush()
			result := buf.String()
			if result != tt.expected {
				t.Errorf("formatString() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestDelWriter_MultiByteDelimiters(t *testing.T) {
	tests := []struct {
		name     string
		values   []interface{}
		mod      *config.DelModifier
		expected string
	}{
		{
			name:   "multi-byte column delimiter",
			values: []interface{}{"Alice", "Bob"},
			mod: &config.DelModifier{
				CharDel: `"`,
				ColDel:  "||",
				RowDel:  "\n",
				Escape:  `"`,
			},
			expected: "\"Alice\"||\"Bob\"\n",
		},
		{
			name:   "multi-byte row delimiter",
			values: []interface{}{"Alice", "Bob"},
			mod: &config.DelModifier{
				CharDel: `"`,
				ColDel:  ",",
				RowDel:  "\r\n",
				Escape:  `"`,
			},
			expected: "\"Alice\",\"Bob\"\r\n",
		},
		{
			name:   "complex multi-byte delimiters",
			values: []interface{}{"Alice", "Bob"},
			mod: &config.DelModifier{
				CharDel: `<<<`,
				ColDel:  ":::",
				RowDel:  ">>>END>>>\n",
				Escape:  `<<<`,
			},
			expected: "<<<Alice<<<:::<<<Bob<<<>>>END>>>\n",
		},
		{
			name:   "multi-byte char delimiter escaped with backslash",
			values: []interface{}{"a||b", `c\d`},
			mod: &config.DelModifier{
				CharDel: "||",
				ColDel:  ",",
				RowDel:  "\n",
				Escape:  `\`,
			},
			expected: "||a\\||b||,||c\\\\d||\n",
		},
		{
			name:   "multi-byte escape sequence escapes itself",
			values: []interface{}{"a||b", "c!!d"},
			mod: &config.DelModifier{
				CharDel: "||",
				ColDel:  ",",
				RowDel:  "\n",
				Escape:  "!!",
			},
			expected: "||a!!||b||,||c!!!!d||\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			writer := NewDelWriter(&buf, tt.mod, false, nil)

			err := writer.WriteRow(tt.values)
			if err != nil {
				t.Errorf("WriteRow() error = %v", err)
				return
			}
			writer.Flush()

			if buf.String() != tt.expected {
				t.Errorf("WriteRow() = %q, want %q", buf.String(), tt.expected)
			}
		})
	}
}

func TestClassifyColumnKind(t *testing.T) {
	cases := map[string]common.ColumnKind{
		"DECIMAL":   common.KindNumber,
		"int":       common.KindNumber,
		"BIGINT":    common.KindNumber,
		"DATE":      common.KindDate,
		"TIME":      common.KindTime,
		"DATETIME":  common.KindTimestamp,
		"TIMESTAMP": common.KindTimestamp,
		"YEAR":      common.KindYear,
		"VARCHAR":   common.KindUnknown,
		"BLOB":      common.KindUnknown,
		"TINYTEXT":  common.KindUnknown,
		"ENUM":      common.KindUnknown,
	}
	for name, want := range cases {
		if got := common.ClassifyColumnKind(name); got != want {
			t.Errorf("ClassifyColumnKind(%q) = %v, want %v", name, got, want)
		}
	}
	if !common.IsBinaryTypeName("LONGBLOB") || !common.IsBinaryTypeName("VARBINARY") || common.IsBinaryTypeName("TEXT") {
		t.Error("IsBinaryTypeName misclassification")
	}
}

func TestDelWriter_RawRowWithKinds(t *testing.T) {
	tests := []struct {
		name     string
		kinds    []common.ColumnKind
		mod      *config.DelModifier
		values   []sql.RawBytes
		expected string
	}{
		{
			name:  "number wire text preserved",
			kinds: []common.ColumnKind{common.KindNumber},
			mod: &config.DelModifier{
				CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `"`,
			},
			values:   []sql.RawBytes{sql.RawBytes("1234.56")},
			expected: "\"1234.56\"\n",
		},
		{
			name:  "date reformat",
			kinds: []common.ColumnKind{common.KindDate},
			mod: &config.DelModifier{
				CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `"`,
				DateFmt: "yyyy/MM/dd",
			},
			values:   []sql.RawBytes{sql.RawBytes("2026-03-15")},
			expected: "\"2026/03/15\"\n",
		},
		{
			name:  "date default unchanged",
			kinds: []common.ColumnKind{common.KindDate},
			mod: &config.DelModifier{
				CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `"`,
				DateFmt: "yyyy-MM-dd",
			},
			values:   []sql.RawBytes{sql.RawBytes("2026-03-15")},
			expected: "\"2026-03-15\"\n",
		},
		{
			name:  "timestamp reformat",
			kinds: []common.ColumnKind{common.KindTimestamp},
			mod: &config.DelModifier{
				CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `"`,
				TimestampFmt: "yyyy/MM/dd HH:mm:ss",
			},
			values:   []sql.RawBytes{sql.RawBytes("2026-03-15 10:30:00")},
			expected: "\"2026/03/15 10:30:00\"\n",
		},
		{
			name:  "unparseable timestamp passthrough",
			kinds: []common.ColumnKind{common.KindTimestamp},
			mod: &config.DelModifier{
				CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `"`,
				TimestampFmt: "yyyy/MM/dd HH:mm:ss",
			},
			values:   []sql.RawBytes{sql.RawBytes("0000-00-00 00:00:00")},
			expected: "\"0000-00-00 00:00:00\"\n",
		},
		{
			name:  "time and year passthrough unmodified",
			kinds: []common.ColumnKind{common.KindTime, common.KindYear},
			mod: &config.DelModifier{
				CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `"`,
				TimestampFmt: "yyyy/MM/dd HH:mm:ss", DateFmt: "yyyy/MM/dd",
			},
			values:   []sql.RawBytes{sql.RawBytes("838:59:59"), sql.RawBytes("0000")},
			expected: "\"838:59:59\",\"0000\"\n",
		},
		{
			name:  "blob stays quoted text (legacy safe)",
			kinds: []common.ColumnKind{common.KindUnknown},
			mod: &config.DelModifier{
				CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `"`,
			},
			values:   []sql.RawBytes{[]byte("bin\x00data")},
			expected: "\"bin\x00data\"\n",
		},
		{
			name:  "nil kinds keeps legacy passthrough for numbers",
			kinds: nil,
			mod: &config.DelModifier{
				CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `"`,
			},
			values:   []sql.RawBytes{sql.RawBytes("1234.56")},
			expected: "\"1234.56\"\n",
		},
		{
			name:  "mixed row honors per-column kinds",
			kinds: []common.ColumnKind{common.KindNumber, common.KindDate, common.KindUnknown},
			mod: &config.DelModifier{
				CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `"`,
				DateFmt: "yyyy/MM/dd",
			},
			values:   []sql.RawBytes{sql.RawBytes("1.5"), sql.RawBytes("2026-01-31"), nil},
			expected: "\"1.5\",\"2026/01/31\",\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			writer := NewDelWriter(&buf, tt.mod, false, tt.kinds)
			if err := writer.WriteRawRow(tt.values); err != nil {
				t.Fatalf("WriteRawRow() error = %v", err)
			}
			writer.Flush()
			if buf.String() != tt.expected {
				t.Errorf("WriteRawRow() = %q, want %q", buf.String(), tt.expected)
			}
		})
	}
}

func TestDelWriter_BinaryHexEncoding(t *testing.T) {
	tests := []struct {
		name     string
		flags    []bool
		hexBlob  bool
		values   []sql.RawBytes
		expected string
	}{
		{
			name:     "hex enabled non-empty binary",
			flags:    []bool{true},
			hexBlob:  true,
			values:   []sql.RawBytes{sql.RawBytes([]byte{0xCA, 0xFE})},
			expected: "0xcafe\n",
		},
		{
			name:     "hex enabled empty binary writes 0x",
			flags:    []bool{true},
			hexBlob:  true,
			values:   []sql.RawBytes{sql.RawBytes([]byte{})},
			expected: "0x\n",
		},
		{
			name:     "hex enabled NULL writes empty",
			flags:    []bool{true},
			hexBlob:  true,
			values:   []sql.RawBytes{nil},
			expected: "\n",
		},
		{
			name:     "hex disabled passthrough",
			flags:    []bool{true},
			hexBlob:  false,
			values:   []sql.RawBytes{sql.RawBytes("raw")},
			expected: "\"raw\"\n",
		},
		{
			name:     "mixed binary and text columns",
			flags:    []bool{false, true, false},
			hexBlob:  true,
			values:   []sql.RawBytes{sql.RawBytes("a"), sql.RawBytes([]byte{0x01, 0x02}), sql.RawBytes("b")},
			expected: "\"a\",0x0102,\"b\"\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			mod := &config.DelModifier{CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `"`}
			w := NewDelWriter(&buf, mod, false, nil)
			w.SetBinaryFormat(tt.flags, tt.hexBlob)
			if err := w.WriteRawRow(tt.values); err != nil {
				t.Fatalf("WriteRawRow() error = %v", err)
			}
			w.Flush()
			if buf.String() != tt.expected {
				t.Errorf("WriteRawRow() = %q, want %q", buf.String(), tt.expected)
			}
		})
	}
}

func TestDelWriter_NumericWireTextUnchanged(t *testing.T) {
	var buf bytes.Buffer
	mod := &config.DelModifier{CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `"`}
	w := NewDelWriter(&buf, mod, false, []common.ColumnKind{common.KindNumber})
	w.WriteRawRow([]sql.RawBytes{sql.RawBytes("1234.56")})
	w.Flush()
	if buf.String() != "\"1234.56\"\n" {
		t.Errorf("numeric wire text changed: got %q", buf.String())
	}
}

func TestHexBlobForCharset(t *testing.T) {
	tests := []struct {
		charset string
		want    bool
	}{
		{"utf8mb4", false},
		{"utf8", false},
		{"UTF8MB4", false},
		{"UTF8", false},
		{"gbk", true},
		{"gb18030", true},
		{"latin1", true},
		{"ascii", true},
	}
	for _, tt := range tests {
		if got := config.HexBlobForCharset(tt.charset); got != tt.want {
			t.Errorf("HexBlobForCharset(%q) = %v, want %v", tt.charset, got, tt.want)
		}
	}
}

func TestHexBlobEffectiveValue(t *testing.T) {
	type testCase struct {
		hexBlob   bool
		noHexBlob bool
		charset   string
		want      bool
	}
	cases := []testCase{
		{false, false, "utf8mb4", false},
		{false, false, "gbk", true},
		{true, false, "utf8mb4", true},
		{true, false, "gbk", true},
		{false, true, "utf8mb4", false},
		{false, true, "gbk", false},
	}
	for _, tc := range cases {
		hex := tc.hexBlob || (!tc.noHexBlob && config.HexBlobForCharset(tc.charset))
		if hex != tc.want {
			t.Errorf("hex(HexBlob=%v, NoHexBlob=%v, charset=%q) = %v, want %v",
				tc.hexBlob, tc.noHexBlob, tc.charset, hex, tc.want)
		}
	}
}

func TestDelWriter_NoEscapeRejectsDelimiters(t *testing.T) {
	base := func() *config.DelModifier {
		return &config.DelModifier{
			CharDel:  `"`,
			ColDel:   ",",
			RowDel:   "\n",
			Escape:   `"`,
			NoEscape: true,
		}
	}

	t.Run("character delimiter in value", func(t *testing.T) {
		var buf bytes.Buffer
		w := NewDelWriter(&buf, base(), false, nil)
		err := w.WriteRow([]interface{}{"ok", `bad "quote"`})
		if err == nil {
			t.Fatal("WriteRow() succeeded, want error")
		}
		if !strings.Contains(err.Error(), "column 2") || !strings.Contains(err.Error(), "character delimiter") {
			t.Errorf("error = %v, want column 2 / character delimiter", err)
		}
		if buf.Len() != 0 {
			t.Errorf("output was buffered despite the error: %q", buf.String())
		}
	})

	t.Run("no char delimiter, column delimiter in value", func(t *testing.T) {
		mod := base()
		mod.NoCharDel = true
		var buf bytes.Buffer
		w := NewDelWriter(&buf, mod, false, nil)
		err := w.WriteRow([]interface{}{"a,b"})
		if err == nil || !strings.Contains(err.Error(), "column delimiter") {
			t.Fatalf("WriteRow() error = %v, want column delimiter error", err)
		}
	})

	t.Run("empty char delimiter, row delimiter in value", func(t *testing.T) {
		mod := base()
		mod.CharDel = ""
		mod.NoCharDel = true
		var buf bytes.Buffer
		w := NewDelWriter(&buf, mod, false, nil)
		err := w.WriteRow([]interface{}{"line1\nline2"})
		if err == nil || !strings.Contains(err.Error(), "row delimiter") {
			t.Fatalf("WriteRow() error = %v, want row delimiter error", err)
		}
	})

	t.Run("delimiters outside quoted field are allowed", func(t *testing.T) {
		var buf bytes.Buffer
		w := NewDelWriter(&buf, base(), false, nil)
		// With quoting in effect a comma and newline inside the field are safe.
		if err := w.WriteRow([]interface{}{"a,b\nc"}); err != nil {
			t.Fatalf("WriteRow() error = %v", err)
		}
		w.Flush()
		if got, want := buf.String(), "\"a,b\nc\"\n"; got != want {
			t.Errorf("output = %q, want %q", got, want)
		}
	})

	t.Run("raw bytes validated", func(t *testing.T) {
		var buf bytes.Buffer
		w := NewDelWriter(&buf, base(), false, nil)
		err := w.WriteRawRow([]sql.RawBytes{[]byte("a"), []byte(`b"c`)})
		if err == nil || !strings.Contains(err.Error(), "column 2") {
			t.Fatalf("WriteRawRow() error = %v, want column 2 error", err)
		}
		if buf.Len() != 0 {
			t.Errorf("output was buffered despite the error: %q", buf.String())
		}
	})

	t.Run("hex blob binary may contain delimiters", func(t *testing.T) {
		var buf bytes.Buffer
		mod := base()
		w := NewDelWriter(&buf, mod, false, nil)
		w.SetBinaryFormat([]bool{false, true}, true)
		if err := w.WriteRawRow([]sql.RawBytes{[]byte("x"), []byte(`y"z`)}); err != nil {
			t.Fatalf("WriteRawRow() error = %v", err)
		}
		w.Flush()
		if got, want := buf.String(), `"x",0x79227a`+"\n"; got != want {
			t.Errorf("output = %q, want %q", got, want)
		}
	})

	t.Run("escaping enabled keeps the quote", func(t *testing.T) {
		mod := base()
		mod.NoEscape = false
		var buf bytes.Buffer
		w := NewDelWriter(&buf, mod, false, nil)
		if err := w.WriteRow([]interface{}{`a"b`}); err != nil {
			t.Fatalf("WriteRow() error = %v", err)
		}
		w.Flush()
		if got, want := buf.String(), "\"a\"\"b\"\n"; got != want {
			t.Errorf("output = %q, want %q", got, want)
		}
	})
}

// The reader only honours escapes inside quoted fields, so with --no-char-del
// (or an empty character delimiter) every field is written verbatim even when
// escaping is still enabled. A delimiter inside such a bare field silently
// produced an unparseable file; it must be rejected like --no-escape does.
func TestDelWriter_NoCharDelRejectsDelimiters(t *testing.T) {
	bare := func() *config.DelModifier {
		return &config.DelModifier{
			CharDel:   `"`,
			NoCharDel: true,
			ColDel:    "\t",
			RowDel:    "\n",
			Escape:    `\`,
		}
	}

	t.Run("column delimiter in bare field", func(t *testing.T) {
		var buf bytes.Buffer
		w := NewDelWriter(&buf, bare(), false, nil)
		err := w.WriteRow([]interface{}{"a\tb"})
		if err == nil || !strings.Contains(err.Error(), "column delimiter") {
			t.Fatalf("error = %v, want column delimiter error", err)
		}
		if !strings.Contains(err.Error(), "--no-char-del") {
			t.Errorf("error = %v, want it to name --no-char-del", err)
		}
		if buf.Len() != 0 {
			t.Errorf("output was buffered despite the error: %q", buf.String())
		}
	})

	t.Run("row delimiter in bare field", func(t *testing.T) {
		var buf bytes.Buffer
		w := NewDelWriter(&buf, bare(), false, nil)
		err := w.WriteRawRow([]sql.RawBytes{[]byte("line1\nline2")})
		if err == nil || !strings.Contains(err.Error(), "row delimiter") {
			t.Fatalf("WriteRawRow() error = %v, want row delimiter error", err)
		}
	})

	t.Run("other delimiters stay exportable", func(t *testing.T) {
		var buf bytes.Buffer
		w := NewDelWriter(&buf, bare(), false, nil)
		if err := w.WriteRow([]interface{}{"a,b", "c|d\re"}); err != nil {
			t.Fatalf("WriteRow() error = %v", err)
		}
		w.Flush()
		if got, want := buf.String(), "a,b\tc|d\re\n"; got != want {
			t.Errorf("output = %q, want %q", got, want)
		}
	})
}

func TestDelWriter_HeaderNotHexEncodedWithHexBlob(t *testing.T) {
	var buf bytes.Buffer
	mod := &config.DelModifier{CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `"`}
	w := NewDelWriter(&buf, mod, true, []common.ColumnKind{common.KindUnknown, common.KindUnknown})
	w.SetBinaryFormat([]bool{false, true}, true)

	if err := w.WriteHeader([]string{"id", "vb"}); err != nil {
		t.Fatalf("WriteHeader() error = %v", err)
	}
	if err := w.WriteRawRow([]sql.RawBytes{[]byte("1"), []byte{0x76, 0x62}}); err != nil {
		t.Fatalf("WriteRawRow() error = %v", err)
	}
	w.Flush()

	// The header must keep the identifier; only the data row is hex-encoded.
	if got, want := buf.String(), "\"id\",\"vb\"\n\"1\",0x7662\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}
