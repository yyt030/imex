package _import

import (
	"bufio"
	"bytes"
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
