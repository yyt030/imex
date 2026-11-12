package backend

import "testing"

// TestShowDefinitions pins the six statements and their result columns. They moved
// here from internal/show in round 133 with the strings copied verbatim, so this
// test is the "nothing was reworded in the move" half of the evidence: the
// statement is contract face ⑤ (what the server is asked) and the column name
// decides which bytes `imex show` prints.
func TestShowDefinitions(t *testing.T) {
	mysql := Must(EngineMySQL)
	want := map[string][2]string{
		"schema":    {"SHOW CREATE DATABASE", "Create Database"},
		"table":     {"SHOW CREATE TABLE", "Create Table"},
		"view":      {"SHOW CREATE VIEW", "Create View"},
		"trigger":   {"SHOW CREATE TRIGGER", "SQL Original Statement"},
		"event":     {"SHOW CREATE EVENT", "Create Event"},
		"procedure": {"SHOW CREATE PROCEDURE", "Create Procedure"},
		"function":  {"SHOW CREATE FUNCTION", "Create Function"},
	}
	// The verb prefix is contract face ⑤ and must stay byte-exact; the query now
	// carries the reference too, so it is verb + " " + the quoted name.
	name := "t"
	ref := mysql.QuoteIdent(name)
	for object, w := range want {
		query, column, hasTerm, ok := mysql.ShowDefinition(object, "", name)
		if !ok {
			t.Errorf("mysql ShowDefinition(%q): not ok", object)
			continue
		}
		if wantQuery := w[0] + " " + ref; query != wantQuery || column != w[1] {
			t.Errorf("mysql ShowDefinition(%q) = (%q, %q), want (%q, %q)", object, query, column, wantQuery, w[1])
		}
		// SHOW CREATE returns no terminator, so the tool must still append one.
		if hasTerm {
			t.Errorf("mysql ShowDefinition(%q): hasTerminator = true, want false", object)
		}
	}

	// A schema IS a database in MySQL, and a qualifier would name a second
	// database the server rejects: SHOW CREATE DATABASE takes one name.
	query, _, _, ok := mysql.ShowDefinition("schema", "other", name)
	if !ok || query != "SHOW CREATE DATABASE "+ref {
		t.Errorf("mysql schema with a qualifier = %q, want %q", query, "SHOW CREATE DATABASE "+ref)
	}

	// "database" is not a kind: `show sch` is the spelling, so the registry must
	// not grow a second entry that would print the same statement.
	if _, _, _, ok := mysql.ShowDefinition("database", "", name); ok {
		t.Error("mysql ShowDefinition(database): want not ok, the kind is spelled schema")
	}
}
