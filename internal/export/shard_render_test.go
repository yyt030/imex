package export

import (
	"strings"
	"testing"

	"github.com/imex/internal/backend"
)

// The shard query is the one statement the parallel path builds by *rewriting*
// the user's SQL, and it is rewritten by the MySQL parser. These tests pin what
// that text looks like once another engine has to read it: nothing but the two
// MySQL-only spellings may change, and an identifier the target is happy to see
// bare stays bare, because bare is how the same query reads at --threads 1.

func TestTranslateShardQueryLeavesMySQLAlone(t *testing.T) {
	text := "select ID, `NAME` from t where t.`NAME` >= 'O\\'Brien'"
	got, err := translateShardQuery(text, nil, nil)
	if err != nil {
		t.Fatalf("mysql translation must not fail: %v", err)
	}
	if got != text {
		t.Errorf("mysql shard query was rewritten: %q", got)
	}
}

func TestTranslateShardQueryIdentifiers(t *testing.T) {
	cases := []struct {
		name     string
		eng      backend.Engine
		in, want string
	}{
		// A word only MySQL considers a keyword stays bare: PG and DM fold it
		// the way they fold the same name in the user's own query.
		{"pg keyword column", backend.EnginePG,
			"select ID, `NAME` from t", "select ID, NAME from t"},
		{"dm keyword column", backend.EngineDM,
			"select ID, `NAME` from t", "select ID, NAME from t"},
		{"typed case is kept", backend.EnginePG,
			"select `name` from t", "select name from t"},
		// A word the target itself reserves has to arrive quoted, or the server
		// reads it as syntax (OFFSET) or as something other than the column.
		{"pg reserved word", backend.EnginePG,
			"select `OFFSET` from t", `select "OFFSET" from t`},
		{"pg user is a function", backend.EnginePG,
			"select `user` from t", `select "user" from t`},
		{"dm reserved word", backend.EngineDM,
			"where t.`LEVEL` >= 1", `where t."LEVEL" >= 1`},
		// Dameng has no use for OFFSET, PostgreSQL has no use for LEVEL: each set
		// carries its own server's answer, not a shared guess.
		{"dm reads offset bare", backend.EngineDM,
			"select `OFFSET` from t", "select OFFSET from t"},
		// Names that cannot be written bare keep the quoting, in the target's
		// grammar.
		{"space in name", backend.EnginePG,
			"select `weird name` from t", `select "weird name" from t`},
		{"backtick in the name", backend.EngineDM,
			"select `we``ird` from t", "select \"we`ird\" from t"},
		{"double quote in the name", backend.EnginePG,
			"select `we\"ird` from t", `select "we""ird" from t`},
		{"leading digit", backend.EnginePG,
			"select `1abc` from t", `select "1abc" from t`},
		{"unicode name", backend.EngineDM,
			"select `名称` from t", `select "名称" from t`},
		// A backtick inside a string literal is data, not a delimiter, and stays.
		{"backtick in literal", backend.EngineDM,
			"where c = 'a`b'", "where c = 'a`b'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reserved, quoteIdent := shardIdentQuoter(backend.Must(tc.eng))
			if reserved == nil || quoteIdent == nil {
				t.Fatalf("engine %s must have a reserved set and a quoter", tc.eng)
			}
			got, err := translateShardQuery(tc.in, reserved, quoteIdent)
			if err != nil {
				t.Fatalf("translateShardQuery(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("translateShardQuery(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTranslateShardQueryLiterals(t *testing.T) {
	_, quoteIdent := shardIdentQuoter(backend.Must(backend.EngineDM))
	cases := []struct {
		name     string
		in, want string
	}{
		{"escaped quote doubles", `where c = 'O\'Brien'`, `where c = 'O''Brien'`},
		{"doubled quote stays", `where c = 'O''Brien'`, `where c = 'O''Brien'`},
		{"backslash is one byte", `where c = 'a\\b'`, `where c = 'a\b'`},
		{"newline is the byte", `where c = 'a\nb'`, "where c = 'a\nb'"},
		{"tab is the byte", `where c = 'a\tb'`, "where c = 'a\tb'"},
		{"like escape keeps its slash", `where c like '100\%'`, `where c like '100\%'`},
		{"unknown escape drops the slash", `where c = 'a\qb'`, `where c = 'aqb'`},
		{"quote inside backticked ident", "select `we'ird` from t", `select "we'ird" from t`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := translateShardQuery(tc.in, dmShardReservedIdents, quoteIdent)
			if err != nil {
				t.Fatalf("translateShardQuery(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("translateShardQuery(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	for _, in := range []string{`where c = 'a\0b'`, "where c = 'unterminated", `where c = 'trailing\`} {
		if _, err := translateShardQuery(in, dmShardReservedIdents, quoteIdent); err == nil {
			t.Errorf("translateShardQuery(%q) must fail, not send bytes the server cannot read", in)
		}
	}
}

// TestBuildShardedQueryForPinsBothEngines runs the whole render, condition
// included: the boundary values come from the server's own data, so a shard key
// holding O'Brien has to come back as a literal the target parses.
func TestBuildShardedQueryForPinsBothEngines(t *testing.T) {
	p := NewSQLParser()
	info := &ShardInfo{Columns: []string{"NAME"}, IsNumeric: []bool{false}}
	rng := ShardRange{
		Mins:   []interface{}{"O'Brien"},
		Maxs:   []interface{}{"z"},
		IsLast: true,
	}

	mysqlQuery, err := p.BuildShardedQuery("SELECT * FROM tbl", info, rng)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mysqlQuery, "`NAME`") || !strings.Contains(mysqlQuery, `\'`) {
		t.Errorf("mysql render changed: %q", mysqlQuery)
	}

	dmQuery, err := p.BuildShardedQueryFor("SELECT * FROM tbl", info, rng, backend.Must(backend.EngineDM))
	if err != nil {
		t.Fatal(err)
	}
	want := "select * from tbl where tbl.NAME >= 'O''Brien' and tbl.NAME <= 'z'"
	if dmQuery != want {
		t.Errorf("dm shard query = %q, want %q", dmQuery, want)
	}

	pgInfo := &ShardInfo{Columns: []string{"OFFSET"}, IsNumeric: []bool{false}}
	pgQuery, err := p.BuildShardedQueryFor("SELECT * FROM tbl", pgInfo, ShardRange{
		Mins: []interface{}{1}, Maxs: []interface{}{9}, IsLast: true,
	}, backend.Must(backend.EnginePG))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pgQuery, `tbl."OFFSET" >= 1`) {
		t.Errorf("pg shard query lost the quoting a reserved column needs: %q", pgQuery)
	}
	if strings.ContainsRune(pgQuery, '`') {
		t.Errorf("pg shard query still carries MySQL backticks: %q", pgQuery)
	}
}

// TestShardReservedSetsAreScopedToWhatTheParserCanQuote: the sets only ever get
// consulted for identifiers vitess chose to backtick, so a word outside that
// behaviour must not appear in them. Both sets were measured on a live server;
// these pins are the parts of that measurement the code depends on.
func TestShardReservedSetsAreScopedToWhatTheParserCanQuote(t *testing.T) {
	for _, word := range []string{"name", "id", "status", "type", "key", "value"} {
		if dmShardReservedIdents[word] || pgShardReservedIdents[word] {
			t.Errorf("%q must stay bare: the MySQL parser quotes it, neither server needs the quoting", word)
		}
	}
	for _, word := range []string{"offset", "only", "user", "any", "some", "references", "returning", "grant", "tablesample"} {
		if !pgShardReservedIdents[word] {
			t.Errorf("pg must quote %q", word)
		}
	}
	for _, word := range []string{"char", "int", "decimal", "level", "user", "view", "trigger", "procedure", "commit", "without", "than"} {
		if !dmShardReservedIdents[word] {
			t.Errorf("dm must quote %q", word)
		}
	}
	if dmShardReservedIdents["offset"] {
		t.Error("dm reads OFFSET bare; quoting it would pin its case for nothing")
	}
	if pgShardReservedIdents["char"] {
		t.Error("pg reads CHAR as a bare column reference in a select list")
	}
}
