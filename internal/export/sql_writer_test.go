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
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id"}, config.DefaultStatementSize,
		true, true, "CREATE TABLE `t` (`id` int);", "utf8mb4")
	if err := w.WriteHeader([]string{"id"}); err != nil {
		t.Fatal(err)
	}
	w.WriteFooter()
	out := buf.String()
	if !strings.HasPrefix(out, "/*!40101 SET NAMES utf8mb4 */;\n") {
		t.Errorf("missing SET NAMES header, got:\n%s", out)
	}
}

func TestSQLWriter_NoHeaderWithoutCreateOrData(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id"}, config.DefaultStatementSize,
		false, false, "", "utf8mb4")
	if err := w.WriteHeader([]string{"id"}); err != nil {
		t.Fatal(err)
	}
	w.WriteFooter()
	if buf.Len() != 0 {
		t.Errorf("expected empty output with create and data both off, got %q", buf.String())
	}
}

func TestSQLWriter_HeaderWithDataOnly(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id"}, config.DefaultStatementSize,
		false, true, "", "latin1")
	if err := w.WriteHeader([]string{"id"}); err != nil {
		t.Fatal(err)
	}
	w.WriteRow([]interface{}{1})
	w.WriteFooter()
	out := buf.String()
	if !strings.HasPrefix(out, "/*!40101 SET NAMES latin1 */;\n") {
		t.Errorf("expected SET NAMES header when writeData=true, got:\n%s", out)
	}
}

func TestSQLWriter_HeaderWithCreateOnly(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id"}, config.DefaultStatementSize,
		true, false, "CREATE TABLE `t` (`id` int);", "utf8")
	if err := w.WriteHeader([]string{"id"}); err != nil {
		t.Fatal(err)
	}
	w.WriteFooter()
	out := buf.String()
	if !strings.HasPrefix(out, "/*!40101 SET NAMES utf8 */;\n") {
		t.Errorf("expected SET NAMES header when writeCreate=true, got:\n%s", out)
	}
	if !strings.Contains(out, "CREATE TABLE") {
		t.Errorf("expected CREATE TABLE in output, got:\n%s", out)
	}
}

func TestSQLWriter_EscapeHandling(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"s"}, config.DefaultStatementSize,
		false, true, "", "utf8mb4")
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
	w := NewSQLWriter(&buf, "t", []string{"s"}, config.DefaultStatementSize,
		false, true, "", "utf8mb4")
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
	w := NewSQLWriter(&buf, "t", []string{"id", "data"}, config.DefaultStatementSize,
		false, true, "", "utf8mb4")
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
	w := NewSQLWriter(&buf, "t", []string{"data"}, config.DefaultStatementSize,
		false, true, "", "utf8mb4")
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
	w := NewSQLWriter(&buf, "t", []string{"id", "name"}, 80,
		false, true, "", "utf8mb4")
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
	w := NewSQLWriter(&buf, "t", []string{"data"}, 10,
		false, true, "", "utf8mb4")
	w.WriteHeader(nil)

	// This single value exceeds the 10-byte budget
	w.WriteRow([]interface{}{"this is a very long value that exceeds the budget"})
	w.WriteFooter()

	out := buf.String()
	if !strings.Contains(out, "INSERT INTO") {
		t.Errorf("expected single overflow INSERT to be written, got:\n%s", out)
	}
}

func TestSQLWriter_WithoutCreateSkipsCreateTable(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id"}, config.DefaultStatementSize,
		false, true, "CREATE TABLE `t` (`id` int);", "utf8mb4")
	w.WriteHeader([]string{"id"})
	w.WriteRow([]interface{}{1})
	w.WriteFooter()

	out := buf.String()
	if strings.Contains(out, "CREATE TABLE") {
		t.Errorf("without --create the writer must skip CREATE TABLE, got:\n%s", out)
	}
	if !strings.Contains(out, "INSERT INTO") {
		t.Errorf("expected INSERT in output, got:\n%s", out)
	}
}

func TestSQLWriter_NoDataSkipsInserts(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id"}, config.DefaultStatementSize,
		true, false, "CREATE TABLE `t` (`id` int);", "utf8mb4")
	w.WriteHeader([]string{"id"})
	w.WriteRow([]interface{}{1}) // should be ignored
	w.WriteFooter()

	out := buf.String()
	if !strings.Contains(out, "CREATE TABLE") {
		t.Errorf("expected CREATE TABLE in output, got:\n%s", out)
	}
	if strings.Contains(out, "INSERT INTO") {
		t.Errorf("no-data should skip INSERTs, got:\n%s", out)
	}
}

func TestSQLWriter_YearNormalization(t *testing.T) {
	// YEAR "0000" should pass through as-is (normalization happens upstream)
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"y"}, config.DefaultStatementSize,
		false, true, "", "utf8mb4")
	w.WriteHeader(nil)
	w.WriteRawRow([]sql.RawBytes{sql.RawBytes("0000")})
	w.WriteFooter()

	out := buf.String()
	if !strings.Contains(out, "'0000'") {
		t.Errorf("YEAR 0000 should be written as '0000', got:\n%s", out)
	}
}

func TestSQLWriter_SingleRowInsert(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id", "v"}, config.DefaultStatementSize,
		false, true, "", "utf8mb4")
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
	if !strings.Contains(inserts[0], "VALUES (1, 'row');") {
		t.Errorf("unexpected statement text: %q", inserts[0])
	}
}

func TestSQLWriter_SingleRowInsertRawRows(t *testing.T) {
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id"}, config.DefaultStatementSize,
		false, true, "", "utf8mb4")
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
	w := NewSQLWriter(&buf, "t", []string{"id", "v"}, 1,
		false, true, "", "utf8mb4")
	w.SetSingleRowInsert(true)
	w.WriteHeader(nil)
	w.WriteRow([]interface{}{int64(1), strings.Repeat("x", 50)})
	w.WriteRow([]interface{}{int64(2), "y"})
	w.WriteFooter()

	out := buf.String()
	if got := strings.Count(out, "INSERT INTO"); got != 2 {
		t.Fatalf("expected 2 statements, got %d in:\n%s", got, out)
	}
	if strings.Contains(out, "), (") {
		t.Errorf("rows were merged into one statement: %q", out)
	}
}

func TestSQLWriterWritesNoTableLocks(t *testing.T) {
	// The dump used to carry a LOCK TABLES ... WRITE / UNLOCK TABLES pair. The
	// lock statement came before CREATE TABLE, so the file could not be replayed
	// into an empty schema: the standard mysql client stopped on the second line
	// with ERROR 1146 (table does not exist). The pair is not written at all now,
	// for either layout, with or without --create.
	for _, create := range []bool{false, true} {
		var buf bytes.Buffer
		w := NewSQLWriter(&buf, "t", []string{"id"}, config.DefaultStatementSize,
			create, true, "CREATE TABLE `t` (`id` int);", "utf8mb4")
		if err := w.WriteHeader(nil); err != nil {
			t.Fatalf("WriteHeader(create=%v): %v", create, err)
		}
		if err := w.WriteRow([]interface{}{int64(1)}); err != nil {
			t.Fatalf("WriteRow(create=%v): %v", create, err)
		}
		if err := w.WriteFooter(); err != nil {
			t.Fatalf("WriteFooter(create=%v): %v", create, err)
		}

		out := buf.String()
		if strings.Contains(out, "LOCK TABLES") || strings.Contains(out, "UNLOCK TABLES") {
			t.Errorf("dump must not contain lock statements (create=%v):\n%s", create, out)
		}
		if !strings.Contains(out, "SET NAMES") || !strings.Contains(out, "INSERT INTO") {
			t.Errorf("preamble and data must survive (create=%v):\n%s", create, out)
		}
	}
}

func TestSQLWriter_ExtendedInsertNeverMergedWarns(t *testing.T) {
	// --statement-size below one row's own width makes the merge rule flush before
	// a second tuple can join, so the merged layout silently collapses to the
	// single-row layout it was meant to avoid. That has to be said out loud.
	logs := captureWriterWarnings(t)
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id", "v"}, 64,
		false, true, "", "utf8mb4")
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
	if strings.Contains(out, "), (") {
		t.Errorf("rows merged despite --statement-size 64:\n%s", out)
	}
	if n := strings.Count(logs.String(), "--extended-insert never merged"); n != 1 {
		t.Errorf("expected exactly one never-merged warning, got %d in:\n%s", n, logs.String())
	}
}

func TestSQLWriter_ExtendedInsertMergedStaysSilent(t *testing.T) {
	logs := captureWriterWarnings(t)
	var buf bytes.Buffer
	w := NewSQLWriter(&buf, "t", []string{"id", "v"}, config.DefaultStatementSize,
		false, true, "", "utf8mb4")
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
	w := NewSQLWriter(&buf, "t", []string{"id", "v"}, 1,
		false, true, "", "utf8mb4")
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
	w := NewSQLWriter(&buf, "t", []string{"id", "v"}, 64,
		false, true, "", "utf8mb4")
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
