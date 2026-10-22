package integration

import (
	"bufio"
	"bytes"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/gomydump/internal/common"
	"github.com/gomydump/internal/config"
	"github.com/gomydump/internal/export"
	_import "github.com/gomydump/internal/import"
)

// These tests are pure writer → reader round trips: no database, no files. They
// lock down the on-disk contract between the two halves of gomydump — the
// exporter's escaping rules and the importer's parser have to agree exactly, and
// both sides are only reachable through the internal packages, so the pair is
// tested from outside as a consumer would use it.

// sqlRoundTrip writes rows through the SQL exporter and reads them back through
// the SQL importer, returning the generated text and the decoded rows.
func sqlRoundTrip(t *testing.T, columns []string, rows [][]interface{}, statementSize int,
	binaryFlags []bool, hexBlob bool) (string, [][]interface{}, []string) {
	t.Helper()

	var buf bytes.Buffer
	w := export.NewSQLWriter(&buf, "t", columns, statementSize, false, true,
		common.ServerMySQL, "", config.DefaultCharset)
	w.SetBinaryFormat(binaryFlags, hexBlob)
	if err := w.WriteHeader(columns); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	for _, row := range rows {
		if err := w.WriteRow(row); err != nil {
			t.Fatalf("WriteRow(%v): %v", row, err)
		}
	}
	if err := w.WriteFooter(); err != nil {
		t.Fatalf("WriteFooter: %v", err)
	}

	text := buf.String()
	r := _import.NewSQLReader(bufio.NewReader(bytes.NewReader(buf.Bytes())))
	cols, err := r.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader: %v (sql=%q)", err, text)
	}

	var out [][]interface{}
	for {
		row, err := r.ReadRow()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("ReadRow: %v", err)
		}
		out = append(out, row)
	}
	return text, out, cols
}

func statementLines(text string) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "INSERT ") {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestSQLRoundTrip_PreservesEscapedText is the core escaping contract: every
// character the exporter has to escape must come back byte-identical.
func TestSQLRoundTrip_PreservesEscapedText(t *testing.T) {
	columns := []string{"id", "v"}
	rows := [][]interface{}{
		{int64(1), "line\nbreak"},
		{int64(2), "crlf\r\npair"},
		{int64(3), "trailing\r"},
		{int64(4), "tab\there"},
		{int64(5), "back\\slash"},
		{int64(6), "double\\\\slash"},
		{int64(7), "quote'inside"},
		{int64(8), `double"quote`},
		{int64(9), "nul\x00byte"},
		{int64(10), "sub\x1astop"},
		{int64(11), "mix:'\"\n\r\t\x00\x1a"},
		{int64(12), ""},
		{int64(13), nil},
		{int64(14), "中文\nemoji"},
		{int64(15), []byte("raw\x00bytes")},
		{int64(16), "\n"},
		{int64(17), "'"},
	}

	text, out, cols := sqlRoundTrip(t, columns, rows, 8<<20, nil, false)

	if want := strings.Join(columns, ","); strings.Join(cols, ",") != want {
		t.Fatalf("columns = %v, want %v", cols, want)
	}
	if len(out) != len(rows) {
		t.Fatalf("read %d rows, wrote %d", len(out), len(rows))
	}

	for i, row := range rows {
		gotID := stringOf(t, out[i][0])
		if want := itoa(row[0]); gotID != want {
			t.Fatalf("row %d: id = %q, want %q", i, gotID, want)
		}
		if !sameValue(out[i][1], row[1]) {
			t.Fatalf("row %d: value = %#v, want %#v (sql=%q)", i, out[i][1], row[1], text)
		}
	}

	// NULL must not collapse into the empty string and vice versa.
	if out[12][1] != nil {
		t.Errorf("NULL came back as %#v (sql=%q)", out[12][1], text)
	}
	if s, ok := out[11][1].(string); !ok || s != "" {
		t.Errorf("empty string came back as %#v (sql=%q)", out[11][1], text)
	}
}

// TestSQLRoundTrip_OnePhysicalLinePerStatement is what makes an exported SQL
// dump splittable by line: a value containing a newline may never emit a raw
// line break into the file, so each INSERT occupies exactly one physical line.
func TestSQLRoundTrip_OnePhysicalLinePerStatement(t *testing.T) {
	columns := []string{"id", "v"}
	rows := [][]interface{}{
		{int64(1), "a\nb\nc"},
		{int64(2), "wrapped\r\nvalue"},
	}

	text, out, _ := sqlRoundTrip(t, columns, rows, 8<<20, nil, false)

	if strings.Contains(text, "\r") {
		t.Errorf("file contains a raw carriage return: %q", text)
	}
	lines := statementLines(text)
	if len(lines) != 1 {
		t.Fatalf("expected the two rows to share one statement, got %d lines", len(lines))
	}
	if !strings.HasSuffix(lines[0], ";") {
		t.Errorf("statement is not terminated on its own line: %q", lines[0])
	}
	// Preamble lines (SET NAMES / LOCK / UNLOCK) must not confuse the reader;
	// reaching this assertion at all proves the rows were found.
	if len(out) != 2 || !sameValue(out[0][1], rows[0][1]) {
		t.Fatalf("row content lost: %#v", out)
	}
}

// TestSQLRoundTrip_StatementSizeBatching checks that the byte budget groups rows
// into fewer statements without changing what the reader recovers.
func TestSQLRoundTrip_StatementSizeBatching(t *testing.T) {
	columns := []string{"id", "v"}
	rows := make([][]interface{}, 0, 200)
	filler := strings.Repeat("x", 60)
	for i := 0; i < 200; i++ {
		rows = append(rows, []interface{}{int64(i), filler})
	}

	oneStatement, out, _ := sqlRoundTrip(t, columns, rows, 8<<20, nil, false)
	if n := len(statementLines(oneStatement)); n != 1 {
		t.Fatalf("large statement-size produced %d statements, want 1", n)
	}
	if len(out) != len(rows) {
		t.Fatalf("read %d rows, want %d", len(out), len(rows))
	}

	split, outSplit, _ := sqlRoundTrip(t, columns, rows, 512, nil, false)
	n := len(statementLines(split))
	if n < 2 {
		t.Fatalf("statement-size 512 should split the batch, got %d statements", n)
	}
	if len(outSplit) != len(rows) {
		t.Fatalf("split read %d rows, want %d", len(outSplit), len(rows))
	}
	for i := range rows {
		if !sameValue(outSplit[i][1], rows[i][1]) {
			t.Fatalf("split row %d lost content: %#v", i, outSplit[i][1])
		}
	}
}

// TestSQLRoundTrip_HexBinaryLiterals covers the charset-safe binary encoding,
// including the two cases that are easy to confuse: an empty blob (0x) and NULL.
func TestSQLRoundTrip_HexBinaryLiterals(t *testing.T) {
	columns := []string{"id", "payload"}
	rows := [][]interface{}{
		{int64(1), nil},
		{int64(2), []byte{}},
		{int64(3), []byte{0x00, 0x1a, '\'', '"', '\\', '\n'}},
		{int64(4), []byte{0xde, 0xad, 0xbe, 0xef}},
	}

	text, out, _ := sqlRoundTrip(t, columns, rows, 8<<20, []bool{false, true}, true)

	if !strings.Contains(text, "0xdeadbeef") {
		t.Errorf("expected lowercase hex literal in output: %q", text)
	}
	if out[0][1] != nil {
		t.Errorf("NULL blob came back as %#v", out[0][1])
	}
	empty, ok := out[1][1].([]byte)
	if !ok || len(empty) != 0 {
		t.Fatalf("empty blob came back as %#v, want []byte{}", out[1][1])
	}
	for i := 2; i < len(rows); i++ {
		got, ok := out[i][1].([]byte)
		if !ok {
			t.Fatalf("row %d came back as %#v, want []byte", i, out[i][1])
		}
		want := rows[i][1].([]byte)
		if !bytes.Equal(got, want) {
			t.Fatalf("row %d: got %x, want %x", i, got, want)
		}
	}
}

// TestSQLRoundTrip_MySQLTextValuesMatchHexValues proves the two binary
// encodings the exporter can emit decode to the same bytes, so a dump written
// with --hex-blob and one written without it land identically in a BLOB column.
func TestSQLRoundTrip_MySQLTextValuesMatchHexValues(t *testing.T) {
	columns := []string{"id", "payload"}
	payload := []byte{0x00, 0x1a, '\'', '"', '\\', '\n', 0x80, 0xff}

	_, hexRows, _ := sqlRoundTrip(t, columns, [][]interface{}{{int64(1), payload}}, 8<<20,
		[]bool{false, true}, true)
	_, textRows, _ := sqlRoundTrip(t, columns, [][]interface{}{{int64(1), payload}}, 8<<20,
		[]bool{false, true}, false)

	if got := asBytes(t, hexRows[0][1]); !bytes.Equal(got, payload) {
		t.Fatalf("hex-encoded round trip = %x, want %x", got, payload)
	}
	if got := asBytes(t, textRows[0][1]); !bytes.Equal(got, payload) {
		t.Fatalf("escaped-text round trip = %x, want %x", got, payload)
	}
}

// delRoundTrip writes rows through the DEL exporter and reads them back with
// the matching modifier set.
func delRoundTrip(t *testing.T, columns []string, rows [][]interface{}, mod *config.DelModifier) (string, [][]interface{}) {
	t.Helper()

	var buf bytes.Buffer
	w := export.NewDelWriter(&buf, mod, false, nil)
	if err := w.WriteHeader(columns); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	for _, row := range rows {
		if err := w.WriteRow(row); err != nil {
			t.Fatalf("WriteRow(%v): %v", row, err)
		}
	}
	if err := w.WriteFooter(); err != nil {
		t.Fatalf("WriteFooter: %v", err)
	}

	r := _import.NewDelReader(bufio.NewReader(bytes.NewReader(buf.Bytes())), mod, false)
	var out [][]interface{}
	for {
		row, err := r.ReadRow()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("ReadRow: %v", err)
		}
		out = append(out, row)
	}
	return buf.String(), out
}

// TestDelRoundTrip_PreservesQuotedText shows the DEL side of the contract:
// quotes, column delimiters and even row delimiters survive, because a newline
// inside a quoted field is not a record boundary.
func TestDelRoundTrip_PreservesQuotedText(t *testing.T) {
	mod := &config.DelModifier{CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `"`}
	columns := []string{"id", "v"}
	rows := [][]interface{}{
		{int64(1), `say "hi"`},
		{int64(2), "a,b,c"},
		{int64(3), "line\nbreak"},
		{int64(4), "crlf\r\npair"},
		{int64(5), "tab\there"},
		{int64(6), `mixed "quoted," and newline`},
		{int64(7), nil},
		{int64(8), ""},
	}

	text, out := delRoundTrip(t, columns, rows, mod)

	if len(out) != len(rows) {
		t.Fatalf("read %d records from %q, wrote %d", len(out), text, len(rows))
	}
	for i, row := range rows {
		if !sameValue(out[i][1], row[1]) {
			t.Fatalf("row %d: value = %#v, want %#v (file=%q)", i, out[i][1], row[1], text)
		}
	}

	// Unlike SQL, DEL keeps a literal newline inside quotes, so the file has
	// more physical lines than records. That is the documented difference.
	if strings.Count(text, "\n") <= len(rows) {
		t.Fatalf("expected embedded newlines to span physical lines, got %q", text)
	}
}

// TestDelRoundTrip_BackslashEscapeMode covers the alternate escaping mode
// (--escape with a character that is not the string delimiter).
func TestDelRoundTrip_BackslashEscapeMode(t *testing.T) {
	mod := &config.DelModifier{CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `\`}
	columns := []string{"id", "v"}
	rows := [][]interface{}{
		{int64(1), `quote"inside`},
		{int64(2), `back\slash`},
		{int64(3), `both"and\`},
	}

	text, out := delRoundTrip(t, columns, rows, mod)
	if len(out) != len(rows) {
		t.Fatalf("read %d records from %q, want %d", len(out), text, len(rows))
	}
	for i, row := range rows {
		if !sameValue(out[i][1], row[1]) {
			t.Fatalf("row %d: value = %#v, want %#v (file=%q)", i, out[i][1], row[1], text)
		}
	}
}

// TestDelRoundTrip_HexBlobIsNotDecoded documents a known asymmetry rather than
// asserting it as desired behaviour: the exporter writes 0x... literals for
// binary columns in DEL format too, but the DEL reader has no schema knowledge
// and returns the literal text. Importing such a file needs --format sql, or a
// target column whose bytes match the hex text. If DEL decoding is implemented,
// this test must be rewritten to require the decoded bytes.
func TestDelRoundTrip_HexBlobIsNotDecoded(t *testing.T) {
	mod := &config.DelModifier{CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `"`}
	payload := []byte{0xde, 0xad}

	var buf bytes.Buffer
	w := export.NewDelWriter(&buf, mod, false, nil)
	w.SetBinaryFormat([]bool{false, true}, true)
	if err := w.WriteRow([]interface{}{int64(1), payload}); err != nil {
		t.Fatalf("WriteRow: %v", err)
	}
	if err := w.WriteFooter(); err != nil {
		t.Fatalf("WriteFooter: %v", err)
	}

	text := buf.String()
	if !strings.Contains(text, "0xdead") {
		t.Fatalf("exporter did not write a hex literal: %q", text)
	}

	r := _import.NewDelReader(bufio.NewReader(bytes.NewReader(buf.Bytes())), mod, false)
	row, err := r.ReadRow()
	if err != nil {
		t.Fatalf("ReadRow: %v", err)
	}
	if got := stringOf(t, row[1]); got != "0xdead" {
		t.Fatalf("DEL reader returned %q; the gap has been closed — update this test to require %x", got, payload)
	}
}

// TestSQLRoundTrip_SingleRowInsert covers the --single-row-insert layout: one
// INSERT per row, still exactly one physical line each, and the reader has to
// recover every row from the many-statement file.
func TestSQLRoundTrip_SingleRowInsert(t *testing.T) {
	columns := []string{"id", "v"}
	rows := [][]interface{}{
		{int64(1), "a\nb"},
		{int64(2), nil},
		{int64(3), "quote'and\"slash\\"},
		{int64(4), ""},
	}

	var buf bytes.Buffer
	w := export.NewSQLWriter(&buf, "t", columns, config.DefaultStatementSize, false, true,
		common.ServerMySQL, "", config.DefaultCharset)
	w.SetSingleRowInsert(true)
	if err := w.WriteHeader(columns); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	for _, row := range rows {
		if err := w.WriteRow(row); err != nil {
			t.Fatalf("WriteRow: %v", err)
		}
	}
	if err := w.WriteFooter(); err != nil {
		t.Fatalf("WriteFooter: %v", err)
	}

	text := buf.String()
	if lines := statementLines(text); len(lines) != len(rows) {
		t.Fatalf("expected %d INSERT statements, got %d in:\n%s", len(rows), len(lines), text)
	}
	for _, line := range statementLines(text) {
		if strings.Count(line, "), (") != 0 {
			t.Fatalf("statement merged rows: %q", line)
		}
	}

	r := _import.NewSQLReader(bufio.NewReader(bytes.NewReader(buf.Bytes())))
	if _, err := r.ReadHeader(); err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	for i, want := range rows {
		got, err := r.ReadRow()
		if err != nil {
			t.Fatalf("ReadRow %d: %v", i, err)
		}
		if !sameValue(got[1], want[1]) {
			t.Fatalf("row %d: value = %#v, want %#v", i, got[1], want[1])
		}
	}
	if _, err := r.ReadRow(); err != io.EOF {
		t.Fatalf("expected EOF after %d rows, got %v", len(rows), err)
	}
}

// --- helpers ---------------------------------------------------------------

// sameValue compares a written value against a read value. The reader hands
// back untyped strings for SQL literals, so byte slices are compared as bytes.
func sameValue(got, want interface{}) bool {
	if want == nil || got == nil {
		return got == nil && want == nil
	}
	g, w := asByteString(got), asByteString(want)
	return g == w
}

func asByteString(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return "\x00nil" // never matches a real value
	case []byte:
		return string(x)
	case string:
		return x
	default:
		return ""
	}
}

func asBytes(t *testing.T, v interface{}) []byte {
	t.Helper()
	switch x := v.(type) {
	case []byte:
		return x
	case string:
		return []byte(x)
	default:
		t.Fatalf("unexpected value type %T (%#v)", v, v)
		return nil
	}
}

func stringOf(t *testing.T, v interface{}) string {
	t.Helper()
	if v == nil {
		return "<nil>"
	}
	switch x := v.(type) {
	case []byte:
		return string(x)
	case string:
		return x
	default:
		return ""
	}
}

func itoa(v interface{}) string {
	switch x := v.(type) {
	case int64:
		return strconv.FormatInt(x, 10)
	default:
		return ""
	}
}
