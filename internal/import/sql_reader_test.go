package _import

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestSQLReader_LargeStatement(t *testing.T) {
	// Build a single INSERT line of ~12 MB with two value tuples, no trailing
	// newline (covers the EOF-without-newline path in scanLines).
	const targetSize = 12 << 20
	// Each tuple: ('aaa...aaa', 'bbb...bbb')
	// Overhead: "INSERT INTO `t` (`a`,`b`) VALUES " = 37 bytes
	// Per tuple: "('','')," = 7 bytes overhead + 2 * payloadLen
	// Two tuples total: 7*2 + 7*2 = 28 bytes overhead (but last has no comma+paren)
	// Simpler: just make payload big enough.
	payloadLen := (targetSize) / 4

	var sb strings.Builder
	sb.WriteString("INSERT INTO `t` (`a`,`b`) VALUES ")
	aVal := strings.Repeat("a", payloadLen)
	bVal := strings.Repeat("b", payloadLen)
	sb.WriteString("('")
	sb.WriteString(aVal)
	sb.WriteString("','")
	sb.WriteString(bVal)
	sb.WriteString("'),('")
	sb.WriteString(aVal)
	sb.WriteString("','")
	sb.WriteString(bVal)
	sb.WriteString("');")
	// Deliberately no trailing \n — tests EOF path.

	line := sb.String()
	if len(line) < targetSize {
		t.Fatalf("generated line too small: %d bytes (wanted >= %d)", len(line), targetSize)
	}

	r := bufio.NewReader(strings.NewReader(line))
	sr := NewSQLReader(r)

	cols, err := sr.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if len(cols) != 2 || cols[0] != "a" || cols[1] != "b" {
		t.Fatalf("unexpected columns: %v", cols)
	}

	row1, err := sr.ReadRow()
	if err != nil {
		t.Fatalf("ReadRow 1: %v", err)
	}
	if row1[0] != aVal || row1[1] != bVal {
		t.Fatalf("row 1 mismatch: len(a)=%d len(b)=%d", len(row1[0].(string)), len(row1[1].(string)))
	}

	row2, err := sr.ReadRow()
	if err != nil {
		t.Fatalf("ReadRow 2: %v", err)
	}
	if row2[0] != aVal || row2[1] != bVal {
		t.Fatalf("row 2 mismatch")
	}

	// Next read should be EOF.
	_, err = sr.ReadRow()
	if err == nil {
		t.Fatal("expected EOF, got nil")
	}
}

func TestSQLReader_HexLiteral(t *testing.T) {
	input := "INSERT INTO `t` (`data`) VALUES (0xDEADBEEF);\n"
	r := bufio.NewReader(strings.NewReader(input))
	sr := NewSQLReader(r)

	cols, err := sr.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if len(cols) != 1 || cols[0] != "data" {
		t.Fatalf("unexpected columns: %v", cols)
	}

	row, err := sr.ReadRow()
	if err != nil {
		t.Fatalf("ReadRow: %v", err)
	}
	got, ok := row[0].([]byte)
	if !ok {
		t.Fatalf("expected []byte, got %T: %v", row[0], row[0])
	}
	want := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	if !bytes.Equal(got, want) {
		t.Fatalf("hex decode: got %x, want %x", got, want)
	}
}

func TestSQLReader_EmptyHexLiteral(t *testing.T) {
	// Isolated "0x" (zero-length hex body) must decode to []byte{}, not nil.
	input := "INSERT INTO `t` (`data`) VALUES (0x);\n"
	r := bufio.NewReader(strings.NewReader(input))
	sr := NewSQLReader(r)

	_, err := sr.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}

	row, err := sr.ReadRow()
	if err != nil {
		t.Fatalf("ReadRow: %v", err)
	}
	got, ok := row[0].([]byte)
	if !ok {
		t.Fatalf("expected []byte, got %T: %v", row[0], row[0])
	}
	if len(got) != 0 {
		t.Fatalf("expected empty []byte, got %x (len=%d)", got, len(got))
	}
}

func TestSQLReader_NullValue(t *testing.T) {
	input := "INSERT INTO `t` (`a`,`b`) VALUES (NULL,'hello');\n"
	r := bufio.NewReader(strings.NewReader(input))
	sr := NewSQLReader(r)

	_, err := sr.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	row, err := sr.ReadRow()
	if err != nil {
		t.Fatalf("ReadRow: %v", err)
	}
	if row[0] != nil {
		t.Fatalf("expected nil for NULL, got %v", row[0])
	}
	if row[1] != "hello" {
		t.Fatalf("expected 'hello', got %v", row[1])
	}
}

func TestSQLReader_CreateStatements(t *testing.T) {
	input := "CREATE TABLE `t` (`id` INT);\nINSERT INTO `t` (`id`) VALUES (1);\n"
	r := bufio.NewReader(strings.NewReader(input))
	sr := NewSQLReader(r)

	_, err := sr.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	stmts := sr.GetCreateStatements()
	if len(stmts) != 1 {
		t.Fatalf("expected 1 CREATE stmt, got %d", len(stmts))
	}
	if !strings.Contains(stmts[0], "CREATE TABLE") {
		t.Fatalf("unexpected CREATE stmt: %s", stmts[0])
	}
}

func TestSQLReader_StructureOnly(t *testing.T) {
	// A --no-data export carries CREATE statements but no INSERTs. ReadHeader
	// must report the structure without erroring (the importer's create mode
	// applies it), and isStructureOnlySQL must recognize that state.
	input := "-- Table structure\nCREATE TABLE `t` (`id` INT) ENGINE=InnoDB;\n"
	sr := NewSQLReader(bufio.NewReader(strings.NewReader(input)))

	cols, err := sr.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader on structure-only file: %v", err)
	}
	if len(cols) != 0 {
		t.Fatalf("expected no columns, got %v", cols)
	}
	if sr.HasData() {
		t.Fatal("HasData true for structure-only file")
	}
	if !isStructureOnlySQL(sr) {
		t.Fatal("isStructureOnlySQL false for captured CREATEs without rows")
	}

	// A file with neither CREATE nor INSERT must still be an EOF error, so the
	// importer reports "no importable data" rather than silently succeeding.
	empty := NewSQLReader(bufio.NewReader(strings.NewReader("SET NAMES utf8mb4;\n")))
	if _, err := empty.ReadHeader(); err != io.EOF {
		t.Fatalf("expected io.EOF for contentless file, got %v", err)
	}
	if isStructureOnlySQL(empty) {
		t.Fatal("isStructureOnlySQL true without CREATE statements")
	}
}

func TestDecodeHexField(t *testing.T) {
	cases := []struct {
		token  string
		want   []byte
		wantOK bool
	}{
		{"0xdead", []byte{0xde, 0xad}, true},
		{"0XDEAD", []byte{0xde, 0xad}, true},
		{"0x", []byte{}, true},
		{"0x0a0b", []byte{0x0a, 0x0b}, true},
		// Rejected: callers must leave these as text instead of mangling them.
		{"0xabc", nil, false},    // odd length
		{"0xzz", nil, false},     // not hex
		{"dead", nil, false},     // no 0x prefix
		{"0", nil, false},        // prefix incomplete
		{`"0xdead"`, nil, false}, // quotes are not part of a literal
	}
	for _, c := range cases {
		got, ok := decodeHexField(c.token)
		if ok != c.wantOK {
			t.Errorf("decodeHexField(%q) ok = %v, want %v", c.token, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if got == nil {
			t.Errorf("decodeHexField(%q) returned nil slice, want non-nil", c.token)
			continue
		}
		if string(got) != string(c.want) {
			t.Errorf("decodeHexField(%q) = %x, want %x", c.token, got, c.want)
		}
	}
}

// drainRows reads every row after ReadHeader and returns their first column,
// rendering NULL as an empty string so a case can assert on all of them at once.
func drainRows(t *testing.T, sr *SQLReader) []string {
	t.Helper()
	var got []string
	for {
		row, err := sr.ReadRow()
		if err == io.EOF {
			return got
		}
		if err != nil {
			t.Fatalf("ReadRow: %v", err)
		}
		switch v := row[0].(type) {
		case nil:
			got = append(got, "")
		case string:
			got = append(got, v)
		case []byte:
			got = append(got, string(v))
		default:
			t.Fatalf("unexpected value type %T (%v)", row[0], row[0])
		}
	}
}

func TestSQLReader_MultiLineStatement(t *testing.T) {
	// One statement spread over many physical lines: the tuple separators of a
	// wrapped multi-row INSERT, plus the cases that make line reassembly hard —
	// a real newline inside a literal, a ';' inside a literal, and a doubled
	// quote. The quoting state has to survive every line break, and each tuple
	// must still come out as its own row with its content intact.
	input := "CREATE TABLE `t` (`v` VARCHAR(64));\n" +
		"INSERT INTO `t` (`v`)\n" +
		"VALUES\n" +
		"('first'),\n" +
		"('two\nlines'),\n" +
		"('semi;colon'),\n" +
		"('dbl''quote')\n" +
		";\n" +
		"INSERT INTO `t` (`v`) VALUES ('after');\n"
	sr := NewSQLReader(bufio.NewReader(strings.NewReader(input)))

	cols, err := sr.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if len(cols) != 1 || cols[0] != "v" {
		t.Fatalf("columns: %v", cols)
	}

	want := []string{"first", "two\nlines", "semi;colon", "dbl'quote", "after"}
	got := drainRows(t, sr)
	if len(got) != len(want) {
		t.Fatalf("rows = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSQLReader_KeywordCaseAndNonASCII(t *testing.T) {
	// Keywords are matched case-insensitively on bytes, and folding must stay in
	// the ASCII range: a value holding a code point whose upper-case form has a
	// different byte length (U+0131 dotless i) must not shift the parser.
	// Quoted "null" stays data; only the bare NULL token is nil.
	input := "insert into `t` (`v`) values ('ı'), ('中文'), ('null'), (NULL);\n"
	sr := NewSQLReader(bufio.NewReader(strings.NewReader(input)))

	cols, err := sr.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader on lower-case statement: %v", err)
	}
	if len(cols) != 1 || cols[0] != "v" {
		t.Fatalf("columns: %v", cols)
	}

	want := []string{"ı", "中文", "null", ""}
	got := drainRows(t, sr)
	if len(got) != len(want) {
		t.Fatalf("rows = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %q, want %q (only the bare NULL token folds to nil)", i, got[i], want[i])
		}
	}
}
