package _import

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"
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

func TestSQLReader_SkipsCreateBeforeInsert(t *testing.T) {
	// imex replays no DDL, so a CREATE TABLE line a foreign dump carries is
	// skipped and the reader advances to the INSERT after it.
	input := "CREATE TABLE `t` (`id` INT);\nINSERT INTO `t` (`id`) VALUES (1);\n"
	sr := NewSQLReader(bufio.NewReader(strings.NewReader(input)))

	cols, err := sr.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if len(cols) != 1 || cols[0] != "id" {
		t.Fatalf("expected column [id] from the INSERT after the skipped CREATE, got %v", cols)
	}
	row, err := sr.ReadRow()
	if err != nil {
		t.Fatalf("ReadRow: %v", err)
	}
	if len(row) != 1 || row[0] != "1" {
		t.Fatalf("expected row [1], got %v", row)
	}
}

func TestSQLReader_CreateOnlyIsEOF(t *testing.T) {
	// A definition-only file — `imex show tab` output, or a mysqldump --no-data
	// dump — carries CREATE statements but no INSERTs. With create mode gone the
	// reader no longer stops to apply that structure: it skips the DDL and reports
	// end of file, so the importer treats it as an empty dump rather than a
	// create-table run. A CREATE is still recognised text, so OnlyPreamble stays
	// true and the empty-file no-op path is chosen over a loud failure.
	input := "-- Table structure\nCREATE TABLE `t` (`id` INT) ENGINE=InnoDB;\n"
	sr := NewSQLReader(bufio.NewReader(strings.NewReader(input)))

	if _, err := sr.ReadHeader(); err != io.EOF {
		t.Fatalf("expected io.EOF for a create-only file, got %v", err)
	}
	if !sr.OnlyPreamble() {
		t.Fatal("a skipped CREATE must not mark the file unrecognized")
	}

	// A file with neither CREATE nor INSERT must also be an EOF error.
	empty := NewSQLReader(bufio.NewReader(strings.NewReader("SET NAMES utf8mb4;\n")))
	if _, err := empty.ReadHeader(); err != io.EOF {
		t.Fatalf("expected io.EOF for contentless file, got %v", err)
	}
}

// TestSQLReader_OnlyPreamble separates an empty dump from a file the reader
// does not understand: both end in io.EOF, but only the first may be imported as
// a successful zero-row run.
func TestSQLReader_OnlyPreamble(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"set header only", "/*!40101 SET NAMES utf8mb4 */;\n", true},
		{"several preamble statements", "SET NAMES utf8mb4;\nLOCK TABLES `t` WRITE;\nUNLOCK TABLES;\n", true},
		{"lower case and comments", "-- dump\nset autocommit=0;\n", true},
		{"unknown text without terminator", "hello world\nnot sql\n", false},
		{"unknown statement with terminator", "SELECT 1;\n", false},
		{"keyword prefix is not a preamble", "SETX 1;\n", false},
		{"empty file", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sr := NewSQLReader(bufio.NewReader(strings.NewReader(c.input)))
			_, err := sr.ReadHeader()
			if err != io.EOF && err != nil {
				t.Fatalf("ReadHeader: %v", err)
			}
			if got := sr.OnlyPreamble(); got != c.want {
				t.Fatalf("OnlyPreamble() = %v, want %v", got, c.want)
			}
		})
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

func TestSQLReader_ValuesSurviveStatementBufferReuse(t *testing.T) {
	// Values are substrings of a per-statement copy, and plain values share the
	// row arena. Reading on to the next statement reuses the recordReader's line
	// buffer; the earlier row must still hold its own bytes.
	input := "INSERT INTO `t` (`a`,`b`) VALUES ('keep-me', 42);\n" +
		"INSERT INTO `t` (`a`,`b`) VALUES ('second', NULL),('third', 0x41);\n"
	sr := NewSQLReader(bufio.NewReader(strings.NewReader(input)))

	cols, err := sr.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if len(cols) != 2 {
		t.Fatalf("columns: %v", cols)
	}

	first, err := sr.ReadRow()
	if err != nil {
		t.Fatalf("ReadRow first: %v", err)
	}
	second, err := sr.ReadRow()
	if err != nil {
		t.Fatalf("ReadRow second: %v", err)
	}
	third, err := sr.ReadRow()
	if err != nil {
		t.Fatalf("ReadRow third: %v", err)
	}

	if got := first[0]; got != "keep-me" {
		t.Errorf("first[0] after buffer reuse = %v, want keep-me", got)
	}
	if got := first[1]; got != "42" {
		t.Errorf("first[1] after buffer reuse = %v, want 42", got)
	}
	if got := second[0]; got != "second" || second[1] != nil {
		t.Errorf("second = %v, want [second <nil>]", second)
	}
	if got := third[0]; got != "third" {
		t.Errorf("third[0] = %v, want third", got)
	}
	if got, ok := third[1].([]byte); !ok || !bytes.Equal(got, []byte("A")) {
		t.Errorf("third[1] = %v, want A from 0x41", third[1])
	}
}

func TestSQLReader_StatementSpansChunks(t *testing.T) {
	// One INSERT holds more tuples than a parse chunk, so the reader has to keep
	// going inside the same statement and hand out the rows in order. The tricky
	// values sit exactly on the chunk boundaries, where the parser restarts,
	// because that is where a re-entry bug would first show up.
	tricky := map[int]string{
		0:                  "'dq''uo'",
		rowChunkRows - 1:   "'bs\\\\slash'",
		rowChunkRows:       "0x41",
		2*rowChunkRows - 1: "'nl\\nnl'",
		2 * rowChunkRows:   "NULL",
	}
	total := 3*rowChunkRows + 5

	var sb strings.Builder
	sb.WriteString("INSERT INTO `t` (`a`,`b`) VALUES ")
	for i := 0; i < total; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		v, ok := tricky[i]
		if !ok {
			v = "'v" + strconv.Itoa(i) + "'"
		}
		sb.WriteByte('(')
		sb.WriteString(v)
		sb.WriteByte(',')
		sb.WriteString(strconv.Itoa(i))
		sb.WriteByte(')')
	}
	sb.WriteString(";")

	sr := NewSQLReader(bufio.NewReader(strings.NewReader(sb.String())))
	cols, err := sr.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if len(cols) != 2 {
		t.Fatalf("columns: %v", cols)
	}

	var first, mid []interface{}
	want := make([]string, total)
	for i := 0; i < total; i++ {
		row, err := sr.ReadRow()
		if err != nil {
			t.Fatalf("ReadRow %d: %v", i, err)
		}
		if len(row) != 2 {
			t.Fatalf("row %d has %d values, want 2", i, len(row))
		}
		if got := fmt.Sprint(row[1]); got != strconv.Itoa(i) {
			t.Fatalf("row %d index value = %q, want %q (chunk boundary dropped or reordered a row)", i, got, strconv.Itoa(i))
		}
		want[i] = fmt.Sprint(row[0])
		if i == 0 {
			first = row
		}
		if i == rowChunkRows {
			mid = row
		}
	}
	if _, err := sr.ReadRow(); err != io.EOF {
		t.Fatalf("after the last row got %v, want EOF", err)
	}

	// The first row was parsed several chunks ago and must still hold its bytes.
	if got := fmt.Sprint(first[0]); got != want[0] {
		t.Errorf("first row value after later chunks = %q, want %q", got, want[0])
	}
	if got := fmt.Sprint(mid[0]); got != want[rowChunkRows] {
		t.Errorf("boundary row value = %q, want %q", got, want[rowChunkRows])
	}
	// Spot-check the boundary values themselves: doubled quote, backslash escape,
	// hex literal, embedded newline and NULL.
	if got := want[0]; got != "dq'uo" {
		t.Errorf("doubled quote at chunk start = %q, want dq'uo", got)
	}
	if got := want[rowChunkRows-1]; got != `bs\slash` {
		t.Errorf("backslash escape at chunk end = %q, want bs\\slash", got)
	}
	if got := want[rowChunkRows]; got != "[65]" {
		t.Errorf("hex literal at chunk start = %q, want [65]", got)
	}
	if got := want[2*rowChunkRows-1]; got != "nl\nnl" {
		t.Errorf("escaped newline = %q, want nl\\nnl", got)
	}
	if want[2*rowChunkRows] != "<nil>" {
		t.Errorf("NULL at chunk start = %q, want <nil>", want[2*rowChunkRows])
	}
}
