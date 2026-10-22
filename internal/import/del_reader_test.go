package _import

import (
	"bufio"
	"strings"
	"testing"

	"fmt"

	"github.com/gomydump/internal/common"
	"github.com/gomydump/internal/config"
)

func TestDelReader_ReadHeader(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		readCols    bool
		expected    []string
		expectError bool
	}{
		{
			name:        "read column names",
			input:       "\"id\",\"name\",\"email\"\n",
			readCols:    true,
			expected:    []string{"id", "name", "email"},
			expectError: false,
		},
		{
			name:        "skip column names",
			input:       "\"id\",\"name\",\"email\"\n",
			readCols:    false,
			expected:    nil,
			expectError: false,
		},
		{
			name:        "empty input with read columns",
			input:       "",
			readCols:    true,
			expected:    nil,
			expectError: true,
		},
		{
			name:        "unquoted column names",
			input:       "id,name,email\n",
			readCols:    true,
			expected:    []string{"id", "name", "email"},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := bufio.NewReader(strings.NewReader(tt.input))
			mod := &config.DelModifier{
				CharDel: `"`,
				ColDel:  ",",
				RowDel:  "\n",
				Escape:  `"`,
			}
			delReader := NewDelReader(reader, mod, tt.readCols)

			columns, err := delReader.ReadHeader()
			if (err != nil) != tt.expectError {
				t.Errorf("ReadHeader() error = %v, wantError %v", err, tt.expectError)
				return
			}
			if !tt.expectError {
				if len(columns) != len(tt.expected) {
					t.Errorf("ReadHeader() columns length = %d, want %d", len(columns), len(tt.expected))
					return
				}
				for i, col := range columns {
					if col != tt.expected[i] {
						t.Errorf("ReadHeader() column[%d] = %q, want %q", i, col, tt.expected[i])
					}
				}
			}
		})
	}
}

func TestDelReader_ReadRow(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		mod         *config.DelModifier
		expected    []interface{}
		expectError bool
	}{
		{
			name:  "simple quoted values",
			input: "\"Alice\",\"Bob\",\"Charlie\"\n",
			mod: &config.DelModifier{
				CharDel: `"`,
				ColDel:  ",",
				RowDel:  "\n",
				Escape:  `"`,
			},
			expected:    []interface{}{"Alice", "Bob", "Charlie"},
			expectError: false,
		},
		{
			name:  "unquoted values",
			input: "1,Alice,30\n",
			mod: &config.DelModifier{
				CharDel: `"`,
				ColDel:  ",",
				RowDel:  "\n",
				Escape:  `"`,
			},
			expected:    []interface{}{"1", "Alice", "30"},
			expectError: false,
		},
		{
			name:  "mixed quoted and unquoted",
			input: "1,\"Alice\",30.5\n",
			mod: &config.DelModifier{
				CharDel: `"`,
				ColDel:  ",",
				RowDel:  "\n",
				Escape:  `"`,
			},
			expected:    []interface{}{"1", "Alice", "30.5"},
			expectError: false,
		},
		{
			name:  "empty fields (NULL)",
			input: "1,,Alice\n",
			mod: &config.DelModifier{
				CharDel: `"`,
				ColDel:  ",",
				RowDel:  "\n",
				Escape:  `"`,
			},
			expected:    []interface{}{"1", nil, "Alice"},
			expectError: false,
		},
		{
			name:  "escaped quotes",
			input: "\"Alice \"\"The Great\"\"\",\"Bob\"\n",
			mod: &config.DelModifier{
				CharDel: `"`,
				ColDel:  ",",
				RowDel:  "\n",
				Escape:  `"`,
			},
			expected:    []interface{}{"Alice \"The Great\"", "Bob"},
			expectError: false,
		},
		{
			name:  "custom delimiters",
			input: "'Alice'|'Bob'\n",
			mod: &config.DelModifier{
				CharDel: `'`,
				ColDel:  "|",
				RowDel:  "\n",
				Escape:  `'`,
			},
			expected:    []interface{}{"Alice", "Bob"},
			expectError: false,
		},
		{
			name:  "multi-byte delimiters",
			input: "\"Alice\"||\"Bob\"\n",
			mod: &config.DelModifier{
				CharDel: `"`,
				ColDel:  "||",
				RowDel:  "\n",
				Escape:  `"`,
			},
			expected:    []interface{}{"Alice", "Bob"},
			expectError: false,
		},
		{
			name:  "multi-byte char delimiter escaped with backslash",
			input: "||a\\||b||\n",
			mod: &config.DelModifier{
				CharDel: "||",
				ColDel:  ",",
				RowDel:  "\n",
				Escape:  `\`,
			},
			expected:    []interface{}{"a||b"},
			expectError: false,
		},
		{
			name:  "multi-byte escape sequence",
			input: "||a!!||b||\n",
			mod: &config.DelModifier{
				CharDel: "||",
				ColDel:  ",",
				RowDel:  "\n",
				Escape:  "!!",
			},
			expected:    []interface{}{"a||b"},
			expectError: false,
		},
		{
			name:  "empty input",
			input: "",
			mod: &config.DelModifier{
				CharDel: `"`,
				ColDel:  ",",
				RowDel:  "\n",
				Escape:  `"`,
			},
			expected:    nil,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := bufio.NewReader(strings.NewReader(tt.input))
			delReader := NewDelReader(reader, tt.mod, false)

			values, err := delReader.ReadRow()
			if (err != nil) != tt.expectError {
				t.Errorf("ReadRow() error = %v, wantError %v", err, tt.expectError)
				return
			}
			if !tt.expectError {
				if len(values) != len(tt.expected) {
					t.Errorf("ReadRow() values length = %d, want %d", len(values), len(tt.expected))
					return
				}
				for i, val := range values {
					if val != tt.expected[i] {
						t.Errorf("ReadRow() value[%d] = %v, want %v", i, val, tt.expected[i])
					}
				}
			}
		})
	}
}

func TestDelReader_ParseLine(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		mod      *config.DelModifier
		expected []string
	}{
		{
			name: "simple quoted fields",
			line: "\"Alice\",\"Bob\",\"Charlie\"",
			mod: &config.DelModifier{
				CharDel: `"`,
				ColDel:  ",",
				Escape:  `"`,
			},
			expected: []string{"Alice", "Bob", "Charlie"},
		},
		{
			name: "unquoted fields",
			line: "1,Alice,30",
			mod: &config.DelModifier{
				CharDel: `"`,
				ColDel:  ",",
				Escape:  `"`,
			},
			expected: []string{"1", "Alice", "30"},
		},
		{
			name: "empty fields",
			line: "1,,Alice,,30",
			mod: &config.DelModifier{
				CharDel: `"`,
				ColDel:  ",",
				Escape:  `"`,
			},
			expected: []string{"1", "", "Alice", "", "30"},
		},
		{
			name: "escaped quotes with double-write",
			line: "\"Alice \"\"The Great\"\"\",\"Bob\"",
			mod: &config.DelModifier{
				CharDel: `"`,
				ColDel:  ",",
				Escape:  `"`,
			},
			expected: []string{"Alice \"The Great\"", "Bob"},
		},
		{
			name: "escaped quotes with backslash",
			line: "\"Alice \\\"The Great\\\"\",\"Bob\"",
			mod: &config.DelModifier{
				CharDel: `"`,
				ColDel:  ",",
				Escape:  `\`,
			},
			expected: []string{"Alice \"The Great\"", "Bob"},
		},
		{
			name: "multi-byte column delimiter",
			line: "\"Alice\"||\"Bob\"||\"Charlie\"",
			mod: &config.DelModifier{
				CharDel: `"`,
				ColDel:  "||",
				Escape:  `"`,
			},
			expected: []string{"Alice", "Bob", "Charlie"},
		},
		{
			name: "trailing delimiter",
			line: "\"Alice\",\"Bob\",",
			mod: &config.DelModifier{
				CharDel: `"`,
				ColDel:  ",",
				Escape:  `"`,
			},
			expected: []string{"Alice", "Bob", ""},
		},
		{
			name: "no char delimiter",
			line: "Alice,Bob,Charlie",
			mod: &config.DelModifier{
				CharDel:   `"`,
				ColDel:    ",",
				Escape:    `"`,
				NoCharDel: true,
			},
			expected: []string{"Alice", "Bob", "Charlie"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := bufio.NewReader(strings.NewReader(""))
			delReader := NewDelReader(reader, tt.mod, false)

			result, _ := delReader.parseLine(tt.line)
			if len(result) != len(tt.expected) {
				t.Errorf("parseLine() result length = %d, want %d", len(result), len(tt.expected))
				t.Errorf("result: %v", result)
				return
			}
			for i, field := range result {
				if field != tt.expected[i] {
					t.Errorf("parseLine() field[%d] = %q, want %q", i, field, tt.expected[i])
				}
			}
		})
	}
}

func TestDelReader_MultipleRows(t *testing.T) {
	input := `"id","name","email"
1,"Alice","alice@example.com"
2,"Bob","bob@example.com"
3,"Charlie","charlie@example.com"
`

	reader := bufio.NewReader(strings.NewReader(input))
	mod := &config.DelModifier{
		CharDel: `"`,
		ColDel:  ",",
		RowDel:  "\n",
		Escape:  `"`,
	}
	delReader := NewDelReader(reader, mod, true)

	// Read header
	columns, err := delReader.ReadHeader()
	if err != nil {
		t.Errorf("ReadHeader() error = %v", err)
		return
	}
	if len(columns) != 3 {
		t.Errorf("ReadHeader() columns length = %d, want 3", len(columns))
		return
	}

	// Read first row
	values1, err := delReader.ReadRow()
	if err != nil {
		t.Errorf("ReadRow() 1 error = %v", err)
		return
	}
	if len(values1) != 3 {
		t.Errorf("ReadRow() 1 values length = %d, want 3", len(values1))
		return
	}
	if values1[0] != "1" || values1[1] != "Alice" || values1[2] != "alice@example.com" {
		t.Errorf("ReadRow() 1 values = %v, want [1, Alice, alice@example.com]", values1)
	}

	// Read second row
	values2, err := delReader.ReadRow()
	if err != nil {
		t.Errorf("ReadRow() 2 error = %v", err)
		return
	}
	if values2[0] != "2" || values2[1] != "Bob" || values2[2] != "bob@example.com" {
		t.Errorf("ReadRow() 2 values = %v, want [2, Bob, bob@example.com]", values2)
	}

	// Read third row
	values3, err := delReader.ReadRow()
	if err != nil {
		t.Errorf("ReadRow() 3 error = %v", err)
		return
	}
	if values3[0] != "3" || values3[1] != "Charlie" || values3[2] != "charlie@example.com" {
		t.Errorf("ReadRow() 3 values = %v, want [3, Charlie, charlie@example.com]", values3)
	}

	// Read fourth row (should be EOF)
	_, err = delReader.ReadRow()
	if err == nil || err.Error() != "EOF" {
		t.Errorf("ReadRow() 4 expected EOF, got %v", err)
	}
}

func TestDelReader_ReverseModifiers(t *testing.T) {
	tests := []struct {
		name     string
		mod      *config.DelModifier
		kinds    []common.ColumnKind
		line     string
		expected []interface{}
	}{
		{
			name: "string column passthrough",
			mod: &config.DelModifier{
				CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `"`,
			},
			kinds:    []common.ColumnKind{common.KindUnknown},
			line:     `"a,b"`,
			expected: []interface{}{"a,b"},
		},
		{
			name: "date slash format reversed",
			mod: &config.DelModifier{
				CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `"`,
				DateFmt: "yyyy/MM/dd",
			},
			kinds:    []common.ColumnKind{common.KindDate},
			line:     `"2026/03/15"`,
			expected: []interface{}{"2026-03-15"},
		},
		{
			name: "timestamp reversed",
			mod: &config.DelModifier{
				CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `"`,
				TimestampFmt: "yyyy/MM/dd HH:mm:ss",
			},
			kinds:    []common.ColumnKind{common.KindTimestamp},
			line:     `"2026/03/15 10:30:00"`,
			expected: []interface{}{"2026-03-15 10:30:00"},
		},
		{
			name: "no kinds set - passthrough",
			mod: &config.DelModifier{
				CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `"`,
			},
			kinds:    nil,
			line:     `"1234.56"`,
			expected: []interface{}{"1234.56"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := NewDelReader(bufio.NewReader(strings.NewReader(tt.line+"\n")), tt.mod, false)
			reader.SetColumnKinds(tt.kinds)
			got, err := reader.ReadRow()
			if err != nil {
				t.Fatalf("ReadRow() error = %v", err)
			}
			if len(got) != len(tt.expected) {
				t.Fatalf("got %d fields, want %d: %#v", len(got), len(tt.expected), got)
			}
			for i := range got {
				if fmt.Sprint(got[i]) != fmt.Sprint(tt.expected[i]) {
					t.Errorf("field %d = %v, want %v", i, got[i], tt.expected[i])
				}
			}
		})
	}
}

func TestDelReader_LargeRecord(t *testing.T) {
	// Build a single DEL record of ~12 MB to verify the unbounded reader.
	const targetSize = 12 << 20
	payloadLen := targetSize - 16 // account for quotes + newline
	payload := strings.Repeat("x", payloadLen)

	input := "\"" + payload + "\"\n"
	r := bufio.NewReader(strings.NewReader(input))
	mod := &config.DelModifier{
		CharDel: `"`,
		ColDel:  ",",
		RowDel:  "\n",
		Escape:  `"`,
	}
	dr := NewDelReader(r, mod, false)

	row, err := dr.ReadRow()
	if err != nil {
		t.Fatalf("ReadRow: %v", err)
	}
	if len(row) != 1 {
		t.Fatalf("expected 1 field, got %d", len(row))
	}
	got, ok := row[0].(string)
	if !ok {
		t.Fatalf("expected string, got %T", row[0])
	}
	if len(got) != payloadLen {
		t.Fatalf("payload length: got %d, want %d", len(got), payloadLen)
	}
}
