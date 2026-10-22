package common

import (
	"strings"
	"testing"
)

func TestSQLPreviewRedacts(t *testing.T) {
	// Default preview keeps identifiers/placeholders but only type/length tags.
	got := SQLPreview("INSERT INTO `t` (`a`, `b`) VALUES (?,?)", []interface{}{1, "secret-passw"})
	if got != "INSERT INTO `t` (`a`, `b`) VALUES (?,?) /* args: num, str(12) */" {
		t.Fatalf("got %q", got)
	}
	// must NOT contain the sensitive string
	if strings.Contains(got, "secret") {
		t.Errorf("redaction leaked value: %s", got)
	}

	// []byte/nil typed tags, no content
	got = SQLPreview("SELECT ?,?", []interface{}{[]byte{0x0A, 0xFF}, nil})
	if got != "SELECT ?,? /* args: bytes(2), NULL */" {
		t.Errorf("got %q", got)
	}

	// nil args: statement echoed verbatim, no comment
	if got := SQLPreview("CREATE TABLE x (id INT)", nil); got != "CREATE TABLE x (id INT)" {
		t.Errorf("got %q", got)
	}

	// many args are capped with a "+N more" note
	wide := make([]interface{}, 100)
	for i := range wide {
		wide[i] = i
	}
	got = SQLPreview("INSERT INTO t VALUES ("+strings.Repeat("?,", 99)+"?)", wide)
	if !strings.Contains(got, "+68 more") {
		t.Errorf("expected cap note, got %q", got)
	}
}

func TestSQLPreviewWithValues(t *testing.T) {
	got := SQLPreviewWithValues("INSERT INTO `t` (`a`, `b`) VALUES (?,?)", []interface{}{1, "O'Brien"})
	want := "INSERT INTO `t` (`a`, `b`) VALUES (1,'O''Brien')"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	got = SQLPreviewWithValues("SELECT ?,?", []interface{}{[]byte{0x0A, 0xFF}, nil})
	if got != "SELECT 0x0AFF,NULL" {
		t.Errorf("got %q", got)
	}
	if got := SQLPreviewWithValues("VALUES (?,?)", []interface{}{7}); got != "VALUES (7,?)" {
		t.Errorf("got %q", got)
	}
}

func TestFailingSQLHonorsSwitch(t *testing.T) {
	defer func() { EchoSQLValues = false }()

	EchoSQLValues = false
	if got := FailingSQL("SELECT ?", []interface{}{"hunter2"}); strings.Contains(got, "hunter2") {
		t.Errorf("default must redact, got %q", got)
	}

	EchoSQLValues = true
	if got := FailingSQL("SELECT ?", []interface{}{"hunter2"}); !strings.Contains(got, "'hunter2'") {
		t.Errorf("opt-in must inline value, got %q", got)
	}
}

func TestTruncateSQL(t *testing.T) {
	// Redacted path: a long STATEMENT must truncate (values never appear).
	longSQL := "INSERT INTO t (" + strings.Repeat("col_, ", 500) + "c) VALUES (" + strings.Repeat("?, ", 5) + "?)"
	got := SQLPreview(longSQL, []interface{}{"a", "b", "c", "d", "e"})
	if !strings.HasSuffix(got, "/* ... truncated */") {
		t.Fatalf("expected truncation marker, got len=%d tail=%q", len(got), got[len(got)-30:])
	}
	if len(got) > MaxSQLErrorPreview+len(" /* ... truncated */") {
		t.Errorf("output too long: %d", len(got))
	}
	if strings.Contains(got, "'a'") {
		t.Errorf("redacted preview must not contain values")
	}

	// WithValues path must also truncate and keep quote parity.
	spaced := strings.Repeat("lorem ipsum dolor sit amet ", MaxSQLErrorPreview/24+1)
	got = SQLPreviewWithValues("INSERT INTO t VALUES (?)", []interface{}{spaced})
	if !strings.HasSuffix(got, "/* ... truncated */") {
		t.Fatalf("expected truncation, len=%d", len(got))
	}
	if q := strings.IndexByte(got, '\''); q >= 0 && strings.Count(got[q:], "'")%2 != 0 {
		t.Errorf("truncation left an unbalanced quote: %s", got)
	}
}
