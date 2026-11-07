package export

import (
	"bytes"
	"database/sql"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

// captureWriterWarnings redirects the package logger into a buffer for the
// duration of a test so the layout warnings can be asserted instead of eyeballed.
func captureWriterWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	orig := common.Logger
	common.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	t.Cleanup(func() { common.Logger = orig })
	return &logs
}

func TestSQLWriter_NoPreamble(t *testing.T) {
	// The dump carries no preamble line at all: it used to open with a MySQL
	// version-conditional `/*!40101 SET NAMES <charset> */;`, which is MySQL
	// session grammar and cannot ride in a file meant to replay unchanged on
	// every engine. Whatever the connection charset, the product now starts at
	// the first INSERT.
	for _, charset := range []string{"utf8mb4", "latin1", "utf8"} {
		var buf bytes.Buffer
		w := NewSQLWriter(&buf, "t", []string{"id"}, config.DefaultStatementSize)
		if err := w.WriteHeader([]string{"id"}); err != nil {
			t.Fatalf("WriteHeader(%s): %v", charset, err)
		}
		if err := w.WriteRow([]interface{}{1}); err != nil {
			t.Fatalf("WriteRow(%s): %v", charset, err)
		}
		if err := w.WriteFooter(); err != nil {
			t.Fatalf("WriteFooter(%s): %v", charset, err)
		}
		out := buf.String()
		if strings.Contains(out, "SET NAMES") || strings.Contains(out, "40101") {
			t.Errorf("preamble must be gone for %s, got:\n%s", charset, out)
		}
		if !strings.HasPrefix(out, "INSERT INTO") {
			t.Errorf("expected INSERT as the first line for %s, got:\n%s", charset, out)
		}
	}
}

// TestSQLWriter_HeaderEmitsNothing pins the whole-file invariant at the empty
// end: an export with only a header and footer — no row — is a zero-byte file.
// It used to open with `/*!40101 SET NAMES utf8mb4 */;`; that preamble is gone,
// so a data-only dump of an empty table is exactly empty, and the importer's
// only-preamble handling reads it as the no-op it is. Table definitions still
// never appear here: they come only from `imex show tab`.
func TestSQLWriter_HeaderEmitsNothing(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id"}, config.DefaultStatementSize)
	if err := w.WriteHeader([]string{"id"}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFooter(); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "" {
		t.Errorf("header = %q, want the empty string (no preamble)", got)
	}
}

// TestSQLWriter_NeverEmitsDDL keeps the whole-file invariant, not just the
// empty end: after a row has been written the product is still only INSERT
// statements — no preamble, no CREATE TABLE, no section comment anywhere.
func TestSQLWriter_NeverEmitsDDL(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id"}, config.DefaultStatementSize)
	if err := w.WriteHeader([]string{"id"}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteRow([]interface{}{1}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFooter(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, forbidden := range []string{"CREATE TABLE", "DROP TABLE", "-- Table structure", "-- Data for", "SET NAMES"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("dump must not contain %q, got:\n%s", forbidden, out)
		}
	}
	if !strings.HasPrefix(out, "INSERT INTO") {
		t.Errorf("dump must start at the first INSERT, got:\n%s", out)
	}
}

func TestSQLWriter_EscapeHandling(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"s"}, config.DefaultStatementSize)
	w.WriteHeader(nil)

	// Test: newline, carriage return, tab, backslash, single quote
	w.WriteRow([]interface{}{"line1\nline2\rline3\tline4\\line5'end"})
	w.WriteFooter()

	out := buf.String()
	// Find the INSERT line
	lines := strings.Split(out, "\n")
	var insertLine string
	for _, l := range lines {
		if strings.HasPrefix(l, "INSERT") {
			insertLine = l
			break
		}
	}
	if insertLine == "" {
		t.Fatalf("no INSERT line found in output:\n%s", out)
	}

	// Check escapes
	if !strings.Contains(insertLine, `\n`) {
		t.Error("newline not escaped to \\n")
	}
	if !strings.Contains(insertLine, `\r`) {
		t.Error("carriage return not escaped to \\r")
	}
	if !strings.Contains(insertLine, `\t`) {
		t.Error("tab not escaped to \\t")
	}
	if !strings.Contains(insertLine, `\\`) {
		t.Error("backslash not escaped to \\\\")
	}
	if !strings.Contains(insertLine, `''`) {
		t.Error("single quote not double-written to ''")
	}
	// Must NOT contain backslash-escaped single quote
	if strings.Contains(insertLine, `\'`) {
		t.Error("single quote should use '' not \\'")
	}
}

func TestSQLWriter_OneInsertPerLine(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"s"}, config.DefaultStatementSize)
	w.WriteHeader(nil)
	w.WriteRow([]interface{}{"hello\nworld"})
	w.WriteRow([]interface{}{"foo\rbar"})
	w.WriteFooter()

	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")

	insertCount := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "INSERT") {
			insertCount++
			// Each INSERT must be on a single physical line (ends with ;)
			if !strings.HasSuffix(l, ";") {
				t.Errorf("INSERT does not end with semicolon on same line: %q", l)
			}
		}
	}
	if insertCount == 0 {
		t.Error("no INSERT statements found")
	}
}

func TestSQLWriter_HexBlobEncoding(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id", "data"}, config.DefaultStatementSize)
	w.SetBinaryFormat([]bool{false, true}, true)
	w.WriteHeader(nil)

	// Non-empty binary value
	w.WriteRawRow([]sql.RawBytes{
		sql.RawBytes("1"),
		sql.RawBytes([]byte{0xDE, 0xAD, 0xBE, 0xEF}),
	})
	// Empty binary value → must be 0x (not '' or NULL)
	w.WriteRawRow([]sql.RawBytes{
		sql.RawBytes("2"),
		sql.RawBytes([]byte{}),
	})
	// NULL binary → must be NULL
	w.WriteRawRow([]sql.RawBytes{
		sql.RawBytes("3"),
		nil,
	})
	w.WriteFooter()

	out := buf.String()
	if !strings.Contains(out, "0xdeadbeef") {
		t.Errorf("expected 0xdeadbeef in output, got:\n%s", out)
	}
	// Empty binary: should contain "0x" followed by comma or paren
	if !strings.Contains(out, "0x,") && !strings.Contains(out, "0x)") {
		t.Errorf("expected empty binary as 0x in output, got:\n%s", out)
	}
	// NULL binary
	if !strings.Contains(out, "NULL") {
		t.Errorf("expected NULL for nil binary in output, got:\n%s", out)
	}
}

func TestSQLWriter_HexBlobDisabled(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"data"}, config.DefaultStatementSize)
	w.SetBinaryFormat([]bool{true}, false) // binary column but hex disabled
	w.WriteHeader(nil)
	w.WriteRawRow([]sql.RawBytes{sql.RawBytes("raw\x00bytes")})
	w.WriteFooter()

	out := buf.String()
	if strings.Contains(out, "0x") {
		t.Errorf("hex encoding should be disabled, but found 0x in output:\n%s", out)
	}
	if !strings.Contains(out, "raw") {
		t.Errorf("expected raw bytes passthrough, got:\n%s", out)
	}
}

func TestSQLWriter_StatementSizeBatching(t *testing.T) {
	// Use a very small statement size to force batch splitting
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id", "name"}, 80)
	w.WriteHeader(nil)

	// Write enough rows to exceed the 80-byte statement size
	for i := 0; i < 5; i++ {
		w.WriteRow([]interface{}{i, "hello"})
	}
	w.WriteFooter()

	out := buf.String()
	// Count INSERT statements - should be more than 1 due to batching
	insertCount := strings.Count(out, "INSERT INTO")
	if insertCount < 2 {
		t.Errorf("expected multiple INSERT statements due to batch splitting, got %d in:\n%s", insertCount, out)
	}
}

func TestSQLWriter_StatementSizeSingleRowOverflow(t *testing.T) {
	// A single row that exceeds statementSize should still be written (graceful degradation)
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"data"}, 10)
	w.WriteHeader(nil)

	// This single value exceeds the 10-byte budget
	w.WriteRow([]interface{}{"this is a very long value that exceeds the budget"})
	w.WriteFooter()

	out := buf.String()
	if !strings.Contains(out, "INSERT INTO") {
		t.Errorf("expected single overflow INSERT to be written, got:\n%s", out)
	}
}

func TestSQLWriter_YearNormalization(t *testing.T) {
	// YEAR "0000" should pass through as-is (normalization happens upstream)
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"y"}, config.DefaultStatementSize)
	w.WriteHeader(nil)
	w.WriteRawRow([]sql.RawBytes{sql.RawBytes("0000")})
	w.WriteFooter()

	out := buf.String()
	if !strings.Contains(out, "'0000'") {
		t.Errorf("YEAR 0000 should be written as '0000', got:\n%s", out)
	}
}

func TestSQLWriter_NumericColumnsUnquoted(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id", "amount", "name", "flag", "year"},
		config.DefaultStatementSize)
	w.SetColumnKinds([]common.ColumnKind{
		common.KindNumber, common.KindNumber, common.KindUnknown, common.KindNumber, common.KindYear,
	})
	if err := w.WriteHeader(nil); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteRawRow([]sql.RawBytes{
		sql.RawBytes("18446744073709551615"),
		sql.RawBytes("1.2300"),
		sql.RawBytes("123"),
		sql.RawBytes("1"),
		sql.RawBytes("0000"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFooter(); err != nil {
		t.Fatal(err)
	}

	want := "INSERT INTO `t` (`id`, `amount`, `name`, `flag`, `year`) " +
		"VALUES (18446744073709551615,1.2300,'123',1,'0000');\n"
	if !strings.Contains(buf.String(), want) {
		t.Errorf("numeric columns must be bare literals and YEAR must stay quoted:\n got %q\nwant %q",
			buf.String(), want)
	}
}

func TestSQLWriter_WriteRowNumericValuesUnquoted(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"a", "b", "c", "d"},
		config.DefaultStatementSize)
	w.SetColumnKinds([]common.ColumnKind{
		common.KindNumber, common.KindNumber, common.KindNumber, common.KindUnknown,
	})
	if err := w.WriteHeader(nil); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteRow([]interface{}{[]byte("7"), "8.50", int64(9), "text"}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFooter(); err != nil {
		t.Fatal(err)
	}

	if want := "VALUES (7,8.50,9,'text');"; !strings.Contains(buf.String(), want) {
		t.Errorf("expected %q, got:\n%s", want, buf.String())
	}
}

func TestSQLWriter_NumericValuesStayQuotedWithoutKinds(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"a", "b"},
		config.DefaultStatementSize)
	if err := w.WriteHeader(nil); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteRawRow([]sql.RawBytes{sql.RawBytes("123"), sql.RawBytes("1.2300")}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFooter(); err != nil {
		t.Fatal(err)
	}

	if want := "VALUES ('123','1.2300');"; !strings.Contains(buf.String(), want) {
		t.Errorf("without column kinds the historical quoted layout must survive, got:\n%s", buf.String())
	}
}

func TestSQLWriter_MergedTuplesHaveNoSpaces(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id", "v"},
		config.DefaultStatementSize)
	w.SetColumnKinds([]common.ColumnKind{common.KindNumber, common.KindUnknown})
	if err := w.WriteHeader(nil); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		if err := w.WriteRow([]interface{}{[]byte(strconv.Itoa(i)), "row"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.WriteFooter(); err != nil {
		t.Fatal(err)
	}

	if want := "VALUES (1,'row'),(2,'row');"; !strings.Contains(buf.String(), want) {
		t.Errorf("expected tuples joined by a bare comma, got:\n%s", buf.String())
	}
}

func TestSQLWriter_SingleRowInsert(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id", "v"}, config.DefaultStatementSize)
	w.SetSingleRowInsert(true)
	w.WriteHeader(nil)

	for i := 1; i <= 3; i++ {
		if err := w.WriteRow([]interface{}{int64(i), "row"}); err != nil {
			t.Fatalf("WriteRow: %v", err)
		}
	}
	if err := w.WriteFooter(); err != nil {
		t.Fatalf("WriteFooter: %v", err)
	}

	out := buf.String()
	lines := strings.Split(out, "\n")
	var inserts []string
	for _, line := range lines {
		if strings.HasPrefix(line, "INSERT ") {
			inserts = append(inserts, line)
		}
	}
	if len(inserts) != 3 {
		t.Fatalf("expected one INSERT per row, got %d in:\n%s", len(inserts), out)
	}
	for _, line := range inserts {
		if strings.Count(line, "(") != 2 {
			t.Errorf("statement carries more than one VALUES tuple: %q", line)
		}
		if !strings.HasSuffix(line, ";") {
			t.Errorf("statement is not terminated on its own line: %q", line)
		}
	}
	if !strings.Contains(inserts[0], "VALUES (1,'row');") {
		t.Errorf("unexpected statement text: %q", inserts[0])
	}
}

func TestSQLWriter_SingleRowInsertRawRows(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id"}, config.DefaultStatementSize)
	w.SetSingleRowInsert(true)
	w.WriteHeader(nil)

	for i := 1; i <= 4; i++ {
		w.WriteRawRow([]sql.RawBytes{sql.RawBytes(strconv.Itoa(i))})
	}
	w.WriteFooter()

	if got := strings.Count(buf.String(), "INSERT INTO"); got != 4 {
		t.Fatalf("expected 4 statements from raw rows, got %d in:\n%s", got, buf.String())
	}
}

func TestSQLWriter_SingleRowInsertOverridesStatementSize(t *testing.T) {
	// A statement size that would normally split rows into one-per-statement is
	// irrelevant now, but the layout must stay valid either way.
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id", "v"}, 1)
	w.SetSingleRowInsert(true)
	w.WriteHeader(nil)
	w.WriteRow([]interface{}{int64(1), strings.Repeat("x", 50)})
	w.WriteRow([]interface{}{int64(2), "y"})
	w.WriteFooter()

	out := buf.String()
	if got := strings.Count(out, "INSERT INTO"); got != 2 {
		t.Fatalf("expected 2 statements, got %d in:\n%s", got, out)
	}
	if strings.Contains(out, "),(") {
		t.Errorf("rows were merged into one statement: %q", out)
	}
}

func TestSQLWriterWritesNoTableLocks(t *testing.T) {
	// The dump used to carry a LOCK TABLES ... WRITE / UNLOCK TABLES pair. The
	// lock statement came before CREATE TABLE, so the file could not be replayed
	// into an empty schema: the standard mysql client stopped on the second line
	// with ERROR 1146 (table does not exist). The pair is not written at all now,
	// for either layout, and the SET NAMES preamble that used to sit above it is
	// gone too, so the file is INSERT statements from its first line.
	for _, singleRow := range []bool{true, false} {
		var buf bytes.Buffer
		w := NewSQLWriter(&buf, "t", []string{"id"}, config.DefaultStatementSize)
		w.SetSingleRowInsert(singleRow)
		if err := w.WriteHeader(nil); err != nil {
			t.Fatalf("WriteHeader(singleRow=%v): %v", singleRow, err)
		}
		if err := w.WriteRow([]interface{}{int64(1)}); err != nil {
			t.Fatalf("WriteRow(singleRow=%v): %v", singleRow, err)
		}
		if err := w.WriteFooter(); err != nil {
			t.Fatalf("WriteFooter(singleRow=%v): %v", singleRow, err)
		}

		out := buf.String()
		if strings.Contains(out, "LOCK TABLES") || strings.Contains(out, "UNLOCK TABLES") {
			t.Errorf("dump must not contain lock statements (singleRow=%v):\n%s", singleRow, out)
		}
		if strings.Contains(out, "SET NAMES") {
			t.Errorf("dump must not carry the SET NAMES preamble (singleRow=%v):\n%s", singleRow, out)
		}
		if !strings.HasPrefix(out, "INSERT INTO") {
			t.Errorf("data must survive as the first line (singleRow=%v):\n%s", singleRow, out)
		}
	}
}

func TestSQLWriter_ExtendedInsertNeverMergedWarns(t *testing.T) {
	// --statement-size below one row's own width makes the merge rule flush before
	// a second tuple can join, so the merged layout silently collapses to the
	// single-row layout it was meant to avoid. That has to be said out loud.
	logs := captureWriterWarnings(t)
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id", "v"}, 64)
	w.WriteHeader(nil)

	for i := 1; i <= 3; i++ {
		if err := w.WriteRow([]interface{}{int64(i), strings.Repeat("x", 40)}); err != nil {
			t.Fatalf("WriteRow: %v", err)
		}
	}
	if err := w.WriteFooter(); err != nil {
		t.Fatalf("WriteFooter: %v", err)
	}

	out := buf.String()
	if got := strings.Count(out, "INSERT INTO"); got != 3 {
		t.Fatalf("expected one statement per row once merging is impossible, got %d in:\n%s", got, out)
	}
	if strings.Contains(out, "),(") {
		t.Errorf("rows merged despite --statement-size 64:\n%s", out)
	}
	if n := strings.Count(logs.String(), "--extended-insert never merged"); n != 1 {
		t.Errorf("expected exactly one never-merged warning, got %d in:\n%s", n, logs.String())
	}
}

func TestSQLWriter_ExtendedInsertMergedStaysSilent(t *testing.T) {
	logs := captureWriterWarnings(t)
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id", "v"}, config.DefaultStatementSize)
	w.WriteHeader(nil)

	for i := 1; i <= 3; i++ {
		if err := w.WriteRow([]interface{}{int64(i), "row"}); err != nil {
			t.Fatalf("WriteRow: %v", err)
		}
	}
	if err := w.WriteFooter(); err != nil {
		t.Fatalf("WriteFooter: %v", err)
	}

	if got := strings.Count(buf.String(), "INSERT INTO"); got != 1 {
		t.Fatalf("expected all three rows in one statement, got %d in:\n%s", got, buf.String())
	}
	if logs.String() != "" {
		t.Errorf("a layout that really merged must not warn: %s", logs.String())
	}
}

func TestSQLWriter_ExtendedInsertSingleRowDoesNotWarn(t *testing.T) {
	// One row has nothing to merge with whatever --statement-size says, so the
	// never-merged warning would only be noise here.
	logs := captureWriterWarnings(t)
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id", "v"}, 1)
	w.WriteHeader(nil)
	if err := w.WriteRow([]interface{}{int64(1), "row"}); err != nil {
		t.Fatalf("WriteRow: %v", err)
	}
	if err := w.WriteFooter(); err != nil {
		t.Fatalf("WriteFooter: %v", err)
	}
	if logs.String() != "" {
		t.Errorf("a single-row dump must not be reported as a failed merge: %s", logs.String())
	}
}

func TestSQLWriter_SingleRowInsertWarnsOnceOnly(t *testing.T) {
	// The other side of the layout switch keeps its own warning, deduplicated to
	// one line per file, and never emits the merged-layout warning.
	logs := captureWriterWarnings(t)
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id", "v"}, 64)
	w.SetSingleRowInsert(true)
	w.WriteHeader(nil)

	for i := 1; i <= 3; i++ {
		if err := w.WriteRow([]interface{}{int64(i), strings.Repeat("x", 40)}); err != nil {
			t.Fatalf("WriteRow: %v", err)
		}
	}
	if err := w.WriteFooter(); err != nil {
		t.Fatalf("WriteFooter: %v", err)
	}

	if n := strings.Count(logs.String(), "single-row INSERT exceeds"); n != 1 {
		t.Errorf("expected one oversized-statement warning, got %d in:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), "--extended-insert never merged") {
		t.Errorf("single-row layout must not report a failed merge: %s", logs.String())
	}
}

// TestAppendQuotedSQLEscaping pins the byte-level escaping the append writer
// performs, including the control bytes that only show up in binary-ish text.
func TestAppendQuotedSQLEscaping(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"plain", "'plain'"},
		{"it's", "'it''s'"},
		{`back\slash`, `'back\\slash'`},
		{"a\nb\rc\td", `'a\nb\rc\td'`},
		{"nul\x00z\x1ain", `'nul\0z\Zin'`},
		{"中文😀", "'中文😀'"},
		{"", "''"},
	}
	for _, tt := range cases {
		if got := string(appendQuotedSQL(nil, tt.in)); got != tt.want {
			t.Errorf("appendQuotedSQL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestSQLWriter_DMDialect pins the Dameng/Oracle grammar byte for byte, using
// only the forms the live DM8 disql probe confirmed the server accepts:
// double-quoted identifiers, single-quote doubling with a literal backslash and
// literal control bytes in strings (no backslash escapes), and HEXTORAW('..')
// for binary. The empty-binary value must render as HEXTORAW(”), not NULL.
func TestSQLWriter_DMDialect(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "T1", []string{"ID", "NAME", "DATA"}, config.DefaultStatementSize)
	w.SetSingleRowInsert(true)
	w.SetDialect(config.SQLDialectDM)
	w.SetBinaryFormat([]bool{false, false, true}, true) // DATA is binary, hex forced
	if err := w.WriteHeader([]string{"ID", "NAME", "DATA"}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if err := w.WriteRow([]interface{}{7, "it's a\\b", []byte{0x41, 0x42}}); err != nil {
		t.Fatalf("WriteRow: %v", err)
	}
	if err := w.WriteRow([]interface{}{8, "", []byte{}}); err != nil {
		t.Fatalf("WriteRow empty: %v", err)
	}
	if err := w.WriteRow([]interface{}{9, nil, nil}); err != nil {
		t.Fatalf("WriteRow null: %v", err)
	}
	if err := w.WriteFooter(); err != nil {
		t.Fatalf("WriteFooter: %v", err)
	}
	want := "INSERT INTO \"T1\" (\"ID\", \"NAME\", \"DATA\") VALUES (7,'it''s a\\b',HEXTORAW('4142'));\n" +
		"INSERT INTO \"T1\" (\"ID\", \"NAME\", \"DATA\") VALUES (8,'',HEXTORAW(''));\n" +
		"INSERT INTO \"T1\" (\"ID\", \"NAME\", \"DATA\") VALUES (9,NULL,NULL);\n"
	if got := buf.String(); got != want {
		t.Errorf("DM dump mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestAppendQuotedSQLDM asserts the Oracle escaper keeps every byte except a
// lone single quote literal, which is why a backslash or a control character
// must NOT be turned into a MySQL-style escape.
func TestAppendQuotedSQLDM(t *testing.T) {
	cases := []struct{ in, want string }{
		{"abc", "'abc'"},
		{"it's", "'it''s'"},
		{`a\b`, "'a\\b'"},  // backslash stays literal, no MySQL escape
		{`a"b`, "'a\"b'"},  // embedded double quote stays literal
		{"a\nb", "'a\nb'"}, // newline emitted verbatim
		{"a\tb", "'a\tb'"}, // tab emitted verbatim
		{"", "''"},
	}
	for _, tt := range cases {
		if got := string(appendQuotedSQLDM(nil, tt.in)); got != tt.want {
			t.Errorf("appendQuotedSQLDM(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// writeBoolRow renders one boolean column through the writer in the given
// dialect, with boolFlags marking that column as boolean, and returns the file.
func writeBoolRow(t *testing.T, d config.SQLDialect, val interface{}) string {
	t.Helper()
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"b"}, config.DefaultStatementSize)
	w.SetSingleRowInsert(true)
	w.SetDialect(d)
	w.SetBooleanFlags([]bool{true})
	if err := w.WriteRow([]interface{}{val}); err != nil {
		t.Fatalf("WriteRow: %v", err)
	}
	if err := w.WriteFooter(); err != nil {
		t.Fatalf("WriteFooter: %v", err)
	}
	return buf.String()
}

// TestSQLWriter_CrossDialectBool pins the fix for the defect where a
// PostgreSQL-sourced boolean column exported in a non-PG dialect wrote the raw
// driver token ("t"/"f") into the VALUES list, which the target then read back
// as a column reference. With boolFlags set the writer must emit each dialect's
// own valid literal, whatever value representation the driver handed over.
func TestSQLWriter_CrossDialectBool(t *testing.T) {
	for _, d := range []config.SQLDialect{config.SQLDialectMySQL, config.SQLDialectDM} {
		for _, tc := range []struct {
			val    interface{}
			wantIn string
		}{
			{true, "1)"},
			{false, "0)"},
			{[]byte("t"), "1)"},
			{[]byte("f"), "0)"},
			{"t", "1)"},
			{"false", "0)"},
			{"true", "1)"},
		} {
			got := writeBoolRow(t, d, tc.val)
			if !strings.Contains(got, tc.wantIn) {
				t.Errorf("dialect %q, value %v: want %q in %q", d, tc.val, tc.wantIn, got)
			}
			// A bare t/f token must never survive into a non-PG dump.
			if strings.Contains(got, ",t,") || strings.Contains(got, ",f,") ||
				strings.Contains(got, "(t,") || strings.Contains(got, "(f)") ||
				strings.HasSuffix(strings.TrimSpace(got), ",t)") ||
				strings.HasSuffix(strings.TrimSpace(got), ",f)") {
				t.Errorf("dialect %q, value %v: bare t/f token leaked: %q", d, tc.val, got)
			}
		}
	}

	// PostgreSQL dialect keeps spelling true/false (regression guard).
	if g := writeBoolRow(t, config.SQLDialectPG, []byte("t")); !strings.Contains(g, "(true)") {
		t.Errorf("pg dialect must spell true/false, got %q", g)
	}
	if g := writeBoolRow(t, config.SQLDialectPG, []byte("f")); !strings.Contains(g, "(false)") {
		t.Errorf("pg dialect must spell true/false, got %q", g)
	}
}
