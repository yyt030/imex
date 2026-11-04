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
		"table":     {"SHOW CREATE TABLE", "Create Table"},
		"view":      {"SHOW CREATE VIEW", "Create View"},
		"trigger":   {"SHOW CREATE TRIGGER", "SQL Original Statement"},
		"event":     {"SHOW CREATE EVENT", "Create Event"},
		"procedure": {"SHOW CREATE PROCEDURE", "Create Procedure"},
		"function":  {"SHOW CREATE FUNCTION", "Create Function"},
	}
	for object, w := range want {
		stmt, column, ok := mysql.ShowDefinition(object)
		if !ok {
			t.Errorf("mysql ShowDefinition(%q): not ok", object)
			continue
		}
		if stmt != w[0] || column != w[1] {
			t.Errorf("mysql ShowDefinition(%q) = (%q, %q), want (%q, %q)", object, stmt, column, w[0], w[1])
		}
	}

	if _, _, ok := mysql.ShowDefinition("database"); ok {
		t.Error("mysql ShowDefinition(database): want not ok, there is no SHOW CREATE DATABASE in the registry")
	}
	if stmt, column, ok := Must(EnginePG).ShowDefinition("table"); ok || stmt != "" || column != "" {
		t.Errorf("pg ShowDefinition(table) = (%q, %q, %v), want the empty refusal", stmt, column, ok)
	}
}
