package integration

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/imex/internal/config"
	"github.com/imex/internal/export"
	_import "github.com/imex/internal/import"
)

// These tests pin the --sql-dialect pg path end to end without a database: the
// writer has to emit PostgreSQL grammar (double-quoted identifiers, '\x..'::bytea
// for binary, E'..' for strings carrying control bytes, true/false for booleans),
// and the SQL reader has to parse exactly that back to the original values. The
// reader is dialect-blind, so the pair is the contract: a dump imex writes for
// PG must read back through imex unchanged.

// pgWrite renders rows through the SQL writer in the pg dialect and returns the
// generated text.
func pgWrite(t *testing.T, columns []string, rows [][]interface{},
	binaryFlags, boolFlags []bool, hexBlob bool) string {
	t.Helper()
	var buf bytes.Buffer
	w := export.NewSQLWriter(&buf, "t", columns, config.DefaultStatementSize)
	w.SetBinaryFormat(binaryFlags, hexBlob)
	w.SetBooleanFlags(boolFlags)
	w.SetDialect(config.SQLDialectPG)
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
	return buf.String()
}

// pgRead parses text back through the SQL reader and returns the column list and
// the decoded rows.
func pgRead(t *testing.T, text string) ([]string, [][]interface{}) {
	t.Helper()
	r := _import.NewSQLReader(bufio.NewReader(bytes.NewReader([]byte(text))))
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
			t.Fatalf("ReadRow: %v (sql=%q)", err, text)
		}
		out = append(out, row)
	}
	return cols, out
}

func TestSQLDialect_PGWriterEmitsPGGrammar(t *testing.T) {
	// Column names include one with an embedded double quote, which must be
	// doubled inside the identifier.
	text := pgWrite(t,
		[]string{"id", "txt", "bin", "nul", "q", "bs", "weird\"name"},
		[][]interface{}{
			{"1", "a\x00b\nc", []byte{0xDE, 0xAD, 0xBE, 0xEF}, nil, "it's", "back\\slash", "x"},
			{"2", "plain", []byte{}, nil, "ok", "n/a", "y"},
		},
		[]bool{false, false, true, false, false, false, false}, nil, true)

	checks := []struct {
		name string
		want string
	}{
		{"double-quoted identifier", `INSERT INTO "t" ("id", "txt"`},
		{"embedded quote doubled", `"weird""name"`},
		{"bytea hex literal", `'\xdeadbeef'::bytea`},
		{"empty bytea", `'\x'::bytea`},
		{"E literal for NUL", `E'a\x00b\nc'`},
		{"backslash doubled in E", `E'back\\slash'`},
		{"plain quote doubled in E", `E'it''s'`},
	}
	for _, c := range checks {
		if !strings.Contains(text, c.want) {
			t.Errorf("missing %s: want %q in output:\n%s", c.name, c.want, text)
		}
	}

	// MySQL-only constructs must be gone.
	for _, bad := range []string{"`", "0x", "SET NAMES", "/*!40101"} {
		if strings.Contains(text, bad) {
			t.Errorf("pg output must not contain %q, got:\n%s", bad, text)
		}
	}
}

func TestSQLDialect_PGRoundTripValues(t *testing.T) {
	columns := []string{"id", "txt", "bin", "nul", "q", "bs"}
	rows := [][]interface{}{
		{"1", "a\x00b\nc", []byte{0xDE, 0xAD, 0xBE, 0xEF}, nil, "it's", "back\\slash"},
		{"2", "plain", []byte{}, nil, "no-quote", "tab\there"},
		{"3", "multi\nline\rtext", []byte{0x00, 0x01, 0x1a}, nil, "a\\b'c", "0x1234"},
	}
	text := pgWrite(t, columns, rows,
		[]bool{false, false, true, false, false, false}, nil, true)

	cols, out := pgRead(t, text)
	if strings.Join(cols, ",") != strings.Join(columns, ",") {
		t.Fatalf("columns = %v, want %v", cols, columns)
	}
	if len(out) != len(rows) {
		t.Fatalf("row count = %d, want %d", len(out), len(rows))
	}

	for ri, want := range rows {
		got := out[ri]
		if len(got) != len(want) {
			t.Fatalf("row %d width = %d, want %d", ri, len(got), len(want))
		}
		for ci, w := range want {
			switch exp := w.(type) {
			case nil:
				if got[ci] != nil {
					t.Errorf("row %d col %d = %#v, want nil", ri, ci, got[ci])
				}
			case []byte:
				gb, ok := got[ci].([]byte)
				if !ok {
					t.Errorf("row %d col %d = %#v (%T), want []byte %s",
						ri, ci, got[ci], got[ci], hex.EncodeToString(exp))
					continue
				}
				if !bytes.Equal(gb, exp) {
					t.Errorf("row %d col %d bytes = %s, want %s",
						ri, ci, hex.EncodeToString(gb), hex.EncodeToString(exp))
				}
			default:
				gs, ok := got[ci].(string)
				if !ok {
					t.Errorf("row %d col %d = %#v (%T), want string %q",
						ri, ci, got[ci], got[ci], exp)
					continue
				}
				if gs != exp {
					t.Errorf("row %d col %d = %q, want %q", ri, ci, gs, exp)
				}
			}
		}
	}
}

func TestSQLDialect_PGBoolLiteral(t *testing.T) {
	text := pgWrite(t,
		[]string{"on", "off"},
		[][]interface{}{{true, false}},
		nil, nil, false)
	if !strings.Contains(text, "(true,false)") {
		t.Errorf("pg boolean must be true/false, got:\n%s", text)
	}

	// A driver in text mode hands back a boolean column as the one-character
	// tokens "t"/"f" (pgx) rather than a Go bool. With BooleanFlags set, those
	// must still land on true/false so psql can replay the dump.
	text = pgWrite(t,
		[]string{"on", "off"},
		[][]interface{}{{[]byte("t"), []byte("f")}},
		nil, []bool{true, true}, false)
	if !strings.Contains(text, "(true,false)") {
		t.Errorf("pg bool column with t/f tokens must emit true/false, got:\n%s", text)
	}
	// And the emitted literals must read back as the tokens imex can bind.
	_, out := pgRead(t, text)
	if len(out) != 1 || len(out[0]) != 2 {
		t.Fatalf("round-trip width wrong: %v", out)
	}
	for ci, want := range []string{"true", "false"} {
		got, ok := out[0][ci].(string)
		if !ok || got != want {
			t.Errorf("read col %d = %#v, want %q", ci, out[0][ci], want)
		}
	}
}
