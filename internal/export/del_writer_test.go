package export

import (
	"bytes"
	"database/sql"

	"github.com/gomydump/internal/common"
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
