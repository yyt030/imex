package _import

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/parquet-go/parquet-go"

	"github.com/imex/internal/common"
)

// TestParquetReader_ZoneTimestampText writes the two columns the exporter would
// write for a Dameng dump — a zone-bearing timestamp and an ordinary text column,
// both as BYTE_ARRAY — and checks that only the classified column has its UTC
// offset separated by a space. Dameng reads the spaced text and rejects the glued
// one with Error 6015; the other column must come through byte for byte, because
// a value that merely looks like a timestamp in a VARCHAR column is that
// column's content.
func TestParquetReader_ZoneTimestampText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tz.parquet")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	group := parquet.Group{
		"TZ":    parquet.Optional(parquet.String()),
		"PLAIN": parquet.Optional(parquet.String()),
	}
	w := parquet.NewGenericWriter[map[string]interface{}](f,
		&parquet.WriterConfig{Schema: parquet.NewSchema("schema", group)})
	rows := []map[string]interface{}{
		{"TZ": "2024-05-06 07:08:09.123456+08:00", "PLAIN": "2024-05-06 07:08:09+08:00"},
		{"TZ": nil, "PLAIN": nil},
	}
	if _, err := w.Write(rows); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}

	open := func() *ParquetReader {
		t.Helper()
		file, err := os.Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		r, err := NewParquetReader(file)
		if err != nil {
			file.Close()
			t.Fatalf("NewParquetReader: %v", err)
		}
		t.Cleanup(func() { r.Close(); file.Close() })
		return r
	}

	r := open()
	cols, err := r.ReadHeader()
	if err != nil || len(cols) != 2 {
		t.Fatalf("ReadHeader = %v, %v", cols, err)
	}
	// The schema field order is the file's, not the writer's: resolve the two
	// positions by name.
	tzIdx, plainIdx := -1, -1
	for i, c := range cols {
		switch c {
		case "TZ":
			tzIdx = i
		case "PLAIN":
			plainIdx = i
		}
	}
	if tzIdx < 0 || plainIdx < 0 {
		t.Fatalf("header = %v, want a TZ and a PLAIN column", cols)
	}
	kinds := make([]common.ColumnKind, len(cols))
	kinds[tzIdx] = common.KindTimestampTZ
	r.SetColumnKinds(kinds)
	got, err := r.ReadRow()
	if err != nil {
		t.Fatalf("ReadRow: %v", err)
	}
	if got[tzIdx] != "2024-05-06 07:08:09.123456 +08:00" {
		t.Errorf("zone column = %v, want the offset separated by one space", got[tzIdx])
	}
	if got[plainIdx] != "2024-05-06 07:08:09+08:00" {
		t.Errorf("text column = %v, want it untouched", got[plainIdx])
	}
	if got, err := r.ReadRow(); err != nil {
		t.Fatalf("ReadRow 2: %v", err)
	} else if got[tzIdx] != nil || got[plainIdx] != nil {
		t.Errorf("NULL row = %v, want both fields nil", got)
	}

	// Without the classification nothing is rewritten, which is the state a
	// reader is in when the target column types could not be resolved.
	plain := open()
	if _, err := plain.ReadHeader(); err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	got, err = plain.ReadRow()
	if err != nil {
		t.Fatalf("ReadRow: %v", err)
	}
	if got[tzIdx] != "2024-05-06 07:08:09.123456+08:00" {
		t.Errorf("zone column without kinds = %v, want the file text verbatim", got[tzIdx])
	}
}

// TestParquetReader_BindFixupsReadsRawBytesOnlyWhereAsked pins the two halves of
// the parquet side of the seam: the file's own schema says which BYTE_ARRAY
// columns hold content bytes rather than annotated text (imex writes a binary
// column as a plain BYTE_ARRAY and every other column as parquet.String()), and
// those bytes are only pulled out as a []byte where the target engine has
// something to say about them. An engine that answers nil for every column —
// MySQL and PostgreSQL — must read exactly what it read before, and a text
// target's adapter must be handed the string the file's text already was.
func TestParquetReader_BindFixupsReadsRawBytesOnlyWhereAsked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raw.parquet")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	group := parquet.Group{
		"TXT": parquet.Optional(parquet.String()),
		"RAW": parquet.Optional(parquet.Leaf(parquet.ByteArrayType)),
		"NUM": parquet.Optional(parquet.String()),
	}
	w := parquet.NewGenericWriter[map[string]interface{}](f,
		&parquet.WriterConfig{Schema: parquet.NewSchema("schema", group)})
	rows := []map[string]interface{}{
		{"TXT": "hello", "RAW": []byte{0xaa, 0x00, 0x01}, "NUM": "99999999999999999999"},
	}
	if _, err := w.Write(rows); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}

	open := func() *ParquetReader {
		t.Helper()
		file, err := os.Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		r, err := NewParquetReader(file)
		if err != nil {
			file.Close()
			t.Fatalf("NewParquetReader: %v", err)
		}
		t.Cleanup(func() { r.Close(); file.Close() })
		return r
	}
	order := func(r *ParquetReader) map[string]int {
		t.Helper()
		cols, err := r.ReadHeader()
		if err != nil {
			t.Fatalf("ReadHeader: %v", err)
		}
		at := map[string]int{}
		for i, c := range cols {
			at[c] = i
		}
		if len(at) != 3 {
			t.Fatalf("header = %v, want three columns", cols)
		}
		return at
	}

	// No adapters: the state every engine but Dameng is in.
	plain := open()
	at := order(plain)
	got, err := plain.ReadRow()
	if err != nil {
		t.Fatalf("ReadRow: %v", err)
	}
	if got[at["TXT"]] != "hello" || got[at["NUM"]] != "99999999999999999999" {
		t.Errorf("text columns = %#v, want the file's text", got)
	}
	if got[at["RAW"]] != string([]byte{0xaa, 0x00, 0x01}) {
		t.Errorf("raw column without an adapter = %#v, want the string it has always been", got[at["RAW"]])
	}

	// With an engine that answers: the raw column is bytes, the annotated ones are
	// still strings, and each adapter runs at its own position.
	dm := open()
	at = order(dm)
	var rawSeen, textSeen string
	binds := make([]func(interface{}) interface{}, 3)
	binds[at["RAW"]] = func(v interface{}) interface{} {
		b, ok := v.([]byte)
		if !ok {
			rawSeen = fmt.Sprintf("%T", v)
			return v
		}
		rawSeen = fmt.Sprintf("%x", b)
		return "bytes:" + string(b)
	}
	binds[at["TXT"]] = func(v interface{}) interface{} {
		s, ok := v.(string)
		if !ok {
			textSeen = fmt.Sprintf("%T", v)
			return v
		}
		textSeen = s
		return "txt:" + s
	}
	dm.SetBindFixups(binds)
	got, err = dm.ReadRow()
	if err != nil {
		t.Fatalf("ReadRow with binds: %v", err)
	}
	if rawSeen != "aa0001" {
		t.Errorf("raw adapter saw %q, want the payload hex", rawSeen)
	}
	if got[at["RAW"]] != "bytes:"+string([]byte{0xaa, 0x00, 0x01}) {
		t.Errorf("raw column = %#v, want the adapter's answer on bytes", got[at["RAW"]])
	}
	if textSeen != "hello" {
		t.Errorf("text adapter saw %q, want the string it has always been", textSeen)
	}
	if got[at["TXT"]] != "txt:hello" {
		t.Errorf("text column = %#v, want the adapter's answer on text", got[at["TXT"]])
	}
	if got[at["NUM"]] != "99999999999999999999" {
		t.Errorf("a column with no adapter = %#v, want it untouched", got[at["NUM"]])
	}

	// The mark a binary target already had still wins: the payload is bytes with
	// or without an adapter, so the hex path is unchanged.
	bin := open()
	at = order(bin)
	flags := make([]bool, 3)
	flags[at["RAW"]] = true
	bin.SetBinaryColumns(flags)
	row, err := bin.ReadRow()
	if err != nil {
		t.Fatalf("ReadRow with binary mark: %v", err)
	}
	if b, ok := row[at["RAW"]].([]byte); !ok || len(b) != 3 || b[0] != 0xaa {
		t.Errorf("marked binary column = %#v, want bytes", row[at["RAW"]])
	}
}
