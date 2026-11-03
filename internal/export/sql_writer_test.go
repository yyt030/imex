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

func TestSQLWriter_SetNamesHeader(t *testing.T) {
	// The charset line is the whole preamble, for every connection charset.
	for _, charset := range []string{"utf8mb4", "latin1", "utf8"} {
		var buf bytes.Buffer
		w := NewSQLWriter(&buf, "t", []string{"id"}, config.DefaultStatementSize, charset)
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
		if !strings.HasPrefix(out, "/*!40101 SET NAMES "+charset+" */;\n") {
			t.Errorf("missing SET NAMES header for %s, got:\n%s", charset, out)
		}
		if !strings.Contains(out, "INSERT INTO") {
			t.Errorf("expected INSERT after the header (charset=%s):\n%s", charset, out)
		}
	}
}

// TestSQLWriter_HeaderIsPreambleOnly pins the DDL removal: the header is the
// charset line alone. It used to also carry `-- Table structure for <t>`, the
// CREATE TABLE statement, a blank line and `-- Data for <t>`. Table definitions
// now come only from `imex show tab`, which is the only place this tool reads
// SHOW CREATE TABLE, so a SQL export must not contain DDL or section comments.
func TestSQLWriter_HeaderIsPreambleOnly(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id"}, config.DefaultStatementSize, "utf8mb4")
	if err := w.WriteHeader([]string{"id"}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFooter(); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "/*!40101 SET NAMES utf8mb4 */;\n" {
		t.Errorf("header = %q, want the charset line only", got)
	}
}

// TestSQLWriter_NeverEmitsDDL keeps the whole-file invariant, not just the
// header: after a row has been written the product is still SET NAMES plus
// INSERT statements, with no CREATE TABLE anywhere.
func TestSQLWriter_NeverEmitsDDL(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id"}, config.DefaultStatementSize, "utf8mb4")
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
	for _, forbidden := range []string{"CREATE TABLE", "DROP TABLE", "-- Table structure", "-- Data for"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("dump must not contain %q, got:\n%s", forbidden, out)
		}
	}
}

func TestSQLWriter_EscapeHandling(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"s"}, config.DefaultStatementSize, "utf8mb4")
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
	w := NewSQLWriter(&buf, "t", []string{"s"}, config.DefaultStatementSize, "utf8mb4")
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
	w := NewSQLWriter(&buf, "t", []string{"id", "data"}, config.DefaultStatementSize, "utf8mb4")
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
	w := NewSQLWriter(&buf, "t", []string{"data"}, config.DefaultStatementSize, "utf8mb4")
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
	w := NewSQLWriter(&buf, "t", []string{"id", "name"}, 80, "utf8mb4")
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
	w := NewSQLWriter(&buf, "t", []string{"data"}, 10, "utf8mb4")
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
	w := NewSQLWriter(&buf, "t", []string{"y"}, config.DefaultStatementSize, "utf8mb4")
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
		config.DefaultStatementSize, "utf8mb4")
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
		config.DefaultStatementSize, "utf8mb4")
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
		config.DefaultStatementSize, "utf8mb4")
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
		config.DefaultStatementSize, "utf8mb4")
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
	w := NewSQLWriter(&buf, "t", []string{"id", "v"}, config.DefaultStatementSize, "utf8mb4")
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
	w := NewSQLWriter(&buf, "t", []string{"id"}, config.DefaultStatementSize, "utf8mb4")
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
	w := NewSQLWriter(&buf, "t", []string{"id", "v"}, 1, "utf8mb4")
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
	// for either layout.
	for _, singleRow := range []bool{true, false} {
		var buf bytes.Buffer
		w := NewSQLWriter(&buf, "t", []string{"id"}, config.DefaultStatementSize, "utf8mb4")
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
		if !strings.Contains(out, "SET NAMES") || !strings.Contains(out, "INSERT INTO") {
			t.Errorf("preamble and data must survive (singleRow=%v):\n%s", singleRow, out)
		}
	}
}

func TestSQLWriter_ExtendedInsertNeverMergedWarns(t *testing.T) {
	// --statement-size below one row's own width makes the merge rule flush before
	// a second tuple can join, so the merged layout silently collapses to the
	// single-row layout it was meant to avoid. That has to be said out loud.
	logs := captureWriterWarnings(t)
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id", "v"}, 64, "utf8mb4")
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
	w := NewSQLWriter(&buf, "t", []string{"id", "v"}, config.DefaultStatementSize, "utf8mb4")
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
	w := NewSQLWriter(&buf, "t", []string{"id", "v"}, 1, "utf8mb4")
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
	w := NewSQLWriter(&buf, "t", []string{"id", "v"}, 64, "utf8mb4")
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
