package backend

import (
	"sort"
	"strings"
	"testing"
)

// TestMySQLKnownCharsetVocabulary pins the 16 names and aliases --default-charset
// accepts on MySQL. The table moved from cmd/validate.go in round 134; this list is
// the part that must not drift, because the rejection sentence is contract face ③.
func TestMySQLKnownCharsetVocabulary(t *testing.T) {
	want := []string{
		"1386", "1392", "384", "5488", "819", "936", "ascii", "gb18030",
		"gbk", "iso-8859-1", "latin1", "us-ascii", "utf-8", "utf8", "utf8mb3",
		"utf8mb4",
	}
	got := make([]string, 0, len(mysqlCharsets))
	for name := range mysqlCharsets {
		got = append(got, name)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("mysqlCharsets = %v, want %v", got, want)
	}

	be := Must(EngineMySQL)
	for _, name := range want {
		if !be.KnownCharset(name) {
			t.Errorf("KnownCharset(%q) = false, want true", name)
		}
	}
	// Lookup trims and folds case.
	for _, name := range []string{" UTF8MB4 ", "Latin1", "GB18030"} {
		if !be.KnownCharset(name) {
			t.Errorf("KnownCharset(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"nonsense", "", "gb2312", "utf-16"} {
		if be.KnownCharset(name) {
			t.Errorf("KnownCharset(%q) = true, want false", name)
		}
	}

	// ValidateCharset stays permissive: MySQL takes the server's own spelling under
	// SET NAMES, and cmd's gate is the one that owns the rejected-name sentence.
	for _, name := range []string{"nonsense", "", "gb2312", "utf-16"} {
		if err := be.ValidateCharset(name); err != nil {
			t.Errorf("ValidateCharset(%q) = %v, want nil", name, err)
		}
	}
}

// TestCharsetVocabulariesAreSeparate keeps the two answers apart. The 16 names
// coincide today, but each engine answers from its own table and prints its own
// sentence; "gb2312" is outside both.
func TestCharsetVocabulariesAreSeparate(t *testing.T) {
	for _, be := range []Backend{Must(EngineMySQL), Must(EnginePG)} {
		if be.KnownCharset("gb2312") {
			t.Errorf("%s accepted gb2312", be.Engine())
		}
		if !be.KnownCharset("utf8mb4") || !be.KnownCharset("us-ascii") {
			t.Errorf("%s is missing a shipped alias", be.Engine())
		}
	}
	pgErr := Must(EnginePG).ValidateCharset("gb2312")
	if pgErr == nil || !strings.Contains(pgErr.Error(), `with --engine pg`) {
		t.Fatalf("pg message = %v", pgErr)
	}
	if err := Must(EngineMySQL).ValidateCharset("gb2312"); err != nil {
		t.Fatalf("MySQL ValidateCharset = %v, want nil (the server is the authority)", err)
	}
}
