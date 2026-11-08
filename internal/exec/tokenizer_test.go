package exec

import (
	"reflect"
	"testing"

	"github.com/imex/internal/backend"
)

func TestSplitStatementsMySQL(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		want   []string
	}{
		{"two", "SELECT 1; SELECT 2;", []string{"SELECT 1", "SELECT 2"}},
		{"no trailing semicolon", "SELECT 1", []string{"SELECT 1"}},
		{"empty regions skipped", "SELECT 1;;  ; SELECT 2;", []string{"SELECT 1", "SELECT 2"}},
		{"empty script", "", nil},
		{"semicolon in single quotes", `INSERT INTO t VALUES ('a;b');`, []string{`INSERT INTO t VALUES ('a;b')`}},
		{"doubled quote escape", `INSERT INTO t VALUES ('it''s;ok');`, []string{`INSERT INTO t VALUES ('it''s;ok')`}},
		{"backslash escape", `INSERT INTO t VALUES ('a\';b');`, []string{`INSERT INTO t VALUES ('a\';b')`}},
		{"semicolon in double quotes", `SELECT "a;b";`, []string{`SELECT "a;b"`}},
		{"semicolon in backticks", "SELECT `a;b` FROM t;", []string{"SELECT `a;b` FROM t"}},
		{"line comment is not a statement", "SELECT 1; -- a;b\n", []string{"SELECT 1"}},
		{"leading block comment preserved", "/* a;b */ SELECT 1;", []string{"/* a;b */ SELECT 1"}},
		{"hash comment", "# note\nSELECT 1;", []string{"# note\nSELECT 1"}},
		{"only comments", "-- nothing\n/* none */", nil},
		{"multi-line statement", "CREATE TABLE t (\n  id INT,\n  x TEXT\n);", []string{"CREATE TABLE t (\n  id INT,\n  x TEXT\n)"}},
		{
			"delimiter switch for routine body",
			"DELIMITER //\nCREATE PROCEDURE p() BEGIN SELECT 1; SELECT 2; END //\nDELIMITER ;\nSELECT 3;",
			[]string{"CREATE PROCEDURE p() BEGIN SELECT 1; SELECT 2; END", "SELECT 3"},
		},
		{"delimiter reset to semicolon", "DELIMITER $$\nSELECT 'a;b'$$\nDELIMITER ;\nSELECT 2;", []string{"SELECT 'a;b'", "SELECT 2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SplitStatements(tc.script, backend.EngineMySQL)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("SplitStatements() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestSplitStatementsPostgres(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		want   []string
	}{
		{"plain split", "SELECT 1; SELECT 2;", []string{"SELECT 1", "SELECT 2"}},
		{"dollar quote hides semicolons",
			"CREATE FUNCTION f() RETURNS int AS $$ BEGIN RETURN 1; END; $$ LANGUAGE plpgsql;",
			[]string{"CREATE FUNCTION f() RETURNS int AS $$ BEGIN RETURN 1; END; $$ LANGUAGE plpgsql"},
		},
		{"tagged dollar quote",
			"CREATE FUNCTION g() RETURNS void AS $body$ BEGIN NULL; END; $body$ LANGUAGE plpgsql;",
			[]string{"CREATE FUNCTION g() RETURNS void AS $body$ BEGIN NULL; END; $body$ LANGUAGE plpgsql"},
		},
		{"backslash is literal, doubled quote closes", `INSERT INTO t VALUES ('it''s'); SELECT 2;`, []string{`INSERT INTO t VALUES ('it''s')`, "SELECT 2"}},
		{"hash is not a comment", "SELECT 1; #2\nSELECT 3;", []string{"SELECT 1", "#2\nSELECT 3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SplitStatements(tc.script, backend.EnginePG)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("SplitStatements() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestSplitStatementsDameng(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		want   []string
	}{
		{"standard split", "SELECT 1; SELECT 2;", []string{"SELECT 1", "SELECT 2"}},
		{"doubled quote closes, backslash does not", `SELECT 'a\'; SELECT 'b';`, []string{`SELECT 'a\'`, `SELECT 'b'`}},
		{"block comment", "SELECT 1 /* ; */ ; SELECT 2;", []string{"SELECT 1 /* ; */", "SELECT 2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SplitStatements(tc.script, backend.EngineDM)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("SplitStatements() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestOnlyComments(t *testing.T) {
	fl := tokenizerFlags{backslashEscape: true, hashComment: true, backtick: true}
	for _, tc := range []struct {
		s    string
		want bool
	}{
		{"-- just a note", true},
		{"/* block */", true},
		{"  \n ", true},
		{"-- a\n/* b */", true},
		{"SELECT 1", false},
		{"/* c */ SELECT 1", false},
		{`'literal'`, false},
	} {
		if got := onlyComments(tc.s, fl); got != tc.want {
			t.Errorf("onlyComments(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

func TestReturnsRows(t *testing.T) {
	fl := tokenizerFlags{backslashEscape: true, hashComment: true, backtick: true}
	for _, tc := range []struct {
		stmt string
		e    backend.Engine
		want bool
	}{
		{"SELECT 1", backend.EngineMySQL, true},
		{"  -- c\nselect 1", backend.EngineMySQL, true},
		{"/* h */ SHOW TABLES", backend.EngineMySQL, true},
		{"CREATE TABLE t (a INT)", backend.EngineMySQL, false},
		{"INSERT INTO t VALUES (1)", backend.EngineMySQL, false},
		{"INSERT INTO t VALUES (1) RETURNING id", backend.EnginePG, true},
		{"INSERT INTO t VALUES (1) RETURNING id", backend.EngineDM, true},
		{"INSERT INTO t VALUES (1) RETURNING id", backend.EngineMySQL, false},
		{"DELETE FROM t WHERE x='returning'", backend.EnginePG, false},
	} {
		if got := returnsRows(tc.stmt, fl, tc.e); got != tc.want {
			t.Errorf("returnsRows(%q, %s) = %v, want %v", tc.stmt, tc.e, got, tc.want)
		}
	}
}
