package show

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/imex/internal/backend"
)

func TestKindRegistryIsConsistent(t *testing.T) {
	kinds := Kinds()
	if len(kinds) != 6 {
		t.Fatalf("kinds = %d, want 6", len(kinds))
	}

	be := backend.Must(backend.EngineMySQL)
	wantShort := []string{"tab", "vws", "trg", "evt", "prc", "fcn"}
	wantFull := []string{"table", "view", "trigger", "event", "procedure", "function"}
	shorts := map[string]bool{}
	fulls := map[string]bool{}
	for i, k := range kinds {
		if k.Short != wantShort[i] {
			t.Errorf("kind %d short = %q, want %q", i, k.Short, wantShort[i])
		}
		if k.Full != wantFull[i] {
			t.Errorf("kind %d full = %q, want %q", i, k.Full, wantFull[i])
		}
		// Three-letter names keep every kind the same width in help output, which
		// is what the naming ruling asked for.
		if len(k.Short) != 3 {
			t.Errorf("kind %q: short name must be 3 letters, got %d", k.Short, len(k.Short))
		}
		// The statement and the result column are the dialect's now (round 133);
		// what this package still owns is the CLI vocabulary, and every kind it
		// registers has to be one the engine can actually print.
		query, column, _, ok := be.ShowDefinition(k.Full, "", "users")
		if !ok {
			t.Errorf("kind %q: engine %s has no definition statement", k.Short, be.Engine())
		}
		if !strings.HasPrefix(query, "SHOW CREATE ") {
			t.Errorf("kind %q: statement = %q", k.Short, query)
		}
		if column == "" {
			t.Errorf("kind %q: no definition column", k.Short)
		}
		if shorts[k.Short] || fulls[k.Full] {
			t.Errorf("kind %q: duplicate name", k.Short)
		}
		shorts[k.Short] = true
		fulls[k.Full] = true
	}

	if got := ShortNames(); strings.Join(got, ",") != strings.Join(wantShort, ",") {
		t.Errorf("ShortNames = %v", got)
	}
}

func TestKindsReturnsACopy(t *testing.T) {
	first := Kinds()
	first[0].Short = "mutated"
	if got := Kinds()[0].Short; got != "tab" {
		t.Errorf("registry changed through the returned slice: %q", got)
	}
}

func TestLookup(t *testing.T) {
	for _, want := range Kinds() {
		for _, name := range []string{want.Short, want.Full} {
			got, ok := Lookup(name)
			if !ok || got.Short != want.Short {
				t.Errorf("Lookup(%q) = %v, %v; want %q", name, got.Short, ok, want.Short)
			}
		}
	}

	// The registry is lowercase-only: a capitalised kind is a usage error at the
	// CLI, and Lookup must not silently accept it.
	for _, name := range []string{"TAB", "Table", "", "tbl", "tabel", "tables"} {
		if _, ok := Lookup(name); ok {
			t.Errorf("Lookup(%q) accepted an unknown name", name)
		}
	}
}

func TestSplitName(t *testing.T) {
	cases := []struct {
		raw            string
		schema, object string
	}{
		{"users", "", "users"},
		{"mydb.users", "mydb", "users"},
		{"`mydb`.`users`", "mydb", "users"},
		{"mydb.`users`", "mydb", "users"},
		{"`users`", "", "users"},
		// A backquoted first part decides on its own, so dots stay inside it.
		{"`a.b`.c", "a.b", "c"},
		{"`a.b`", "", "a.b"},
		// Doubled backquotes are the escape for a backquote in a name.
		{"`we``ird`.`x`", "we`ird", "x"},
		{"  mydb.users  ", "mydb", "users"},
		// Without backquotes the first dot splits; the rest stays in the name,
		// which the server then rejects as an unknown object.
		{"mydb.users.extra", "mydb", "users.extra"},
		{".users", "", "users"},
		{"mydb.", "mydb", ""},
		{"", "", ""},
		// Names the grammar cannot read stay whole, so the server reports the
		// unknown object instead of a guessed split.
		{"`abc", "", "`abc"},
		{"`users`x", "", "`users`x"},
	}
	for _, tc := range cases {
		schema, object := SplitName(tc.raw)
		if schema != tc.schema || object != tc.object {
			t.Errorf("SplitName(%q) = %q, %q; want %q, %q", tc.raw, schema, object, tc.schema, tc.object)
		}
	}
}

func TestSplitNameEmptyObjectIsDetectable(t *testing.T) {
	// runShow turns an empty object into a usage error; the two forms that produce
	// it must be distinguishable from a plain name.
	for _, raw := range []string{"", "   ", "mydb.", "mydb.  "} {
		if _, object := SplitName(raw); object != "" {
			t.Errorf("SplitName(%q) object = %q, want empty", raw, object)
		}
	}
}

func TestScanQuoted(t *testing.T) {
	cases := []struct {
		in, name, rest string
		ok             bool
	}{
		{"`a`", "a", "", true},
		{"`a`.b", "a", ".b", true},
		{"`a``b`.c", "a`b", ".c", true},
		{"`a.b`", "a.b", "", true},
		{"`a", "", "", false},
		{"`a``", "", "", false},
	}
	for _, tc := range cases {
		name, rest, ok := scanQuoted(tc.in)
		if ok != tc.ok || name != tc.name || rest != tc.rest {
			t.Errorf("scanQuoted(%q) = %q, %q, %v; want %q, %q, %v", tc.in, name, rest, ok, tc.name, tc.rest, tc.ok)
		}
	}
}

// The identifier quoting that used to live here is Backend.QuoteIdent now and is
// pinned in internal/backend/backend_test.go for both dialects; what stays here is
// the composition that adds the schema qualifier.

func TestQualify(t *testing.T) {
	be := backend.Must(backend.EngineMySQL)
	if got := qualify(be, "", "users"); got != "`users`" {
		t.Errorf("unqualified = %q", got)
	}
	if got := qualify(be, "mydb", "users"); got != "`mydb`.`users`" {
		t.Errorf("qualified = %q", got)
	}
	// A name carrying a backquote must not be able to close its own quoted span
	// and append statements to the SHOW.
	if got := qualify(be, "a", "x`; DROP TABLE t"); !strings.HasSuffix(got, "``; DROP TABLE t`") {
		t.Errorf("escaping failed: %q", got)
	}
}

// TestSplitAndQualifyRoundTrip checks the two halves agree: the CLI parses what a
// user types and the package renders the statement target back from the parts, so
// a quoted name never loses its escaping on the way to the SHOW.
func TestSplitAndQualifyRoundTrip(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"users", "`users`"},
		{"mydb.users", "`mydb`.`users`"},
		{"`mydb`.`users`", "`mydb`.`users`"},
		{"`a.b`.c", "`a.b`.`c`"},
		{"`we``ird`.`x`", "`we``ird`.`x`"},
		{"`a.b`", "`a.b`"},
	}
	be := backend.Must(backend.EngineMySQL)
	for _, tc := range cases {
		schema, object := SplitName(tc.raw)
		if object == "" {
			t.Fatalf("SplitName(%q) lost the object name", tc.raw)
		}
		if got := qualify(be, schema, object); got != tc.want {
			t.Errorf("SplitName(%q) then qualify = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestPickDefinition(t *testing.T) {
	str := func(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }

	// Measured column shapes on MariaDB 10.6.23: the definition is the second
	// column for table and view, the third for trigger, procedure and function,
	// and the fourth for event. Selecting a fixed index would print sql_mode.
	shapes := []struct {
		name    string
		columns []string
		values  []sql.NullString
		want    string
	}{
		{
			name:    "table",
			columns: []string{"Table", "Create Table"},
			values:  []sql.NullString{str("sh_t"), str("CREATE TABLE `sh_t` (`id` int(11))")},
			want:    "CREATE TABLE `sh_t` (`id` int(11))",
		},
		{
			name:    "view",
			columns: []string{"View", "Create View"},
			values:  []sql.NullString{str("sh_v"), str("CREATE ALGORITHM=UNDEFINED DEFINER=... VIEW `sh_v` AS select 1")},
			want:    "CREATE ALGORITHM=UNDEFINED DEFINER=... VIEW `sh_v` AS select 1",
		},
		{
			name:    "trigger",
			columns: []string{"Trigger", "sql_mode", "SQL Original Statement"},
			values:  []sql.NullString{str("sh_trg"), str("STRICT_TRANS_TABLES"), str("CREATE DEFINER=`gd`@`%` TRIGGER `sh_trg` ...")},
			want:    "CREATE DEFINER=`gd`@`%` TRIGGER `sh_trg` ...",
		},
		{
			name:    "event",
			columns: []string{"Event", "sql_mode", "time_zone", "Create Event"},
			values:  []sql.NullString{str("sh_e"), str("STRICT_TRANS_TABLES"), str("SYSTEM"), str("CREATE DEFINER=... EVENT `sh_e` ON SCHEDULE ...")},
			want:    "CREATE DEFINER=... EVENT `sh_e` ON SCHEDULE ...",
		},
		{
			name:    "procedure",
			columns: []string{"Procedure", "sql_mode", "Create Procedure"},
			values:  []sql.NullString{str("sh_p"), str("STRICT_TRANS_TABLES"), str("CREATE DEFINER=... PROCEDURE `sh_p`() BEGIN ... END")},
			want:    "CREATE DEFINER=... PROCEDURE `sh_p`() BEGIN ... END",
		},
		{
			name:    "function",
			columns: []string{"Function", "sql_mode", "Create Function"},
			values:  []sql.NullString{str("sh_f"), str("STRICT_TRANS_TABLES"), str("CREATE DEFINER=... FUNCTION `sh_f`() RETURNS int(11)")},
			want:    "CREATE DEFINER=... FUNCTION `sh_f`() RETURNS int(11)",
		},
	}
	for _, tc := range shapes {
		t.Run(tc.name, func(t *testing.T) {
			_, column, _, ok := backend.Must(backend.EngineMySQL).ShowDefinition(tc.name, "", "")
			if !ok {
				t.Fatalf("engine has no definition statement for %q", tc.name)
			}
			got, ok := pickDefinition(tc.columns, tc.values, column)
			if !ok {
				t.Fatalf("no definition found in %v", tc.columns)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "STRICT_TRANS_TABLES") {
				t.Errorf("sql_mode leaked into the definition: %q", got)
			}
		})
	}
}

func TestPickDefinitionByNameIsCaseInsensitive(t *testing.T) {
	// A server that changes the column-name case must not disable the tool.
	got, ok := pickDefinition(
		[]string{"Table", "create table"},
		[]sql.NullString{{String: "t", Valid: true}, {String: "CREATE TABLE `t` (id int)", Valid: true}},
		"Create Table",
	)
	if !ok || !strings.HasPrefix(got, "CREATE") {
		t.Fatalf("got %q, %v", got, ok)
	}
}

func TestPickDefinitionFallsBackToValueShape(t *testing.T) {
	// Unknown column name: the first value starting with CREATE is still the
	// definition, so a rename does not silently print a session attribute.
	got, ok := pickDefinition(
		[]string{"Table", "something_new"},
		[]sql.NullString{{String: "t", Valid: true}, {String: "CREATE TABLE `t` (id int)", Valid: true}},
		"Create Table",
	)
	if !ok || got != "CREATE TABLE `t` (id int)" {
		t.Fatalf("got %q, %v", got, ok)
	}

	// Lowercase definition text must be caught too.
	got, ok = pickDefinition(
		[]string{"Table", "x"},
		[]sql.NullString{{String: "t", Valid: true}, {String: "create view v as select 1", Valid: true}},
		"Create View",
	)
	if !ok || !strings.HasPrefix(got, "create view") {
		t.Fatalf("got %q, %v", got, ok)
	}
}

func TestPickDefinitionRejectsNonDefinition(t *testing.T) {
	// Only a name and sql_mode: nothing here starts with CREATE, so the caller
	// gets an error rather than a sql_mode dump.
	if got, ok := pickDefinition(
		[]string{"Table", "sql_mode"},
		[]sql.NullString{{String: "t", Valid: true}, {String: "STRICT_TRANS_TABLES", Valid: true}},
		"Create Table",
	); ok {
		t.Fatalf("accepted non-definition %q", got)
	}

	// A NULL definition column is not a value.
	if _, ok := pickDefinition(
		[]string{"Table", "Create Table"},
		[]sql.NullString{{String: "t", Valid: true}, {}},
		"Create Table",
	); ok {
		t.Fatal("accepted a NULL definition")
	}
}
