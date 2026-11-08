package backend

import (
	"strings"
	"testing"
)

// TestDamengShowDefinitions pins the Dameng DBMS_METADATA.GET_DDL calls: the
// object-type argument per kind, the case folding (GET_DDL's name/schema args are
// case-sensitive and DM Oracle mode stores unquoted names upper-cased), the
// per-kind terminator property, the two-argument form when no schema is given,
// and that a crafted name stays inert. These are contract face ⑤ and were
// measured against a live DM8 instance in Oracle-compatible mode.
func TestDamengShowDefinitions(t *testing.T) {
	dm := Must(EngineDM)

	cases := []struct {
		object      string
		wantType    string
		wantHasTerm bool
	}{
		{"table", "TABLE", true},
		{"view", "VIEW", false},
		{"trigger", "TRIGGER", true},
		{"procedure", "PROCEDURE", true},
		{"function", "FUNCTION", true},
	}
	for _, c := range cases {
		query, column, hasTerm, ok := dm.ShowDefinition(c.object, "imexit", "t_emp")
		if !ok {
			t.Errorf("dm ShowDefinition(%q): not ok", c.object)
			continue
		}
		if column != "DDL" {
			t.Errorf("dm ShowDefinition(%q) column = %q, want DDL", c.object, column)
		}
		if hasTerm != c.wantHasTerm {
			t.Errorf("dm ShowDefinition(%q) hasTerminator = %v, want %v", c.object, hasTerm, c.wantHasTerm)
		}
		// The name and schema are upper-cased into the literal.
		wantCall := "GET_DDL('" + c.wantType + "','T_EMP','IMEXIT')"
		if !strings.Contains(query, wantCall) {
			t.Errorf("dm ShowDefinition(%q) query = %q, want it to contain %q", c.object, query, wantCall)
		}
		if !strings.Contains(query, "DBMS_METADATA") || !strings.Contains(query, "FROM DUAL") {
			t.Errorf("dm ShowDefinition(%q) query = %q, want the DBMS_METADATA ... FROM DUAL shape", c.object, query)
		}
	}

	// event has no object to print on DM8 Oracle mode.
	if _, _, _, ok := dm.ShowDefinition("event", "", "e"); ok {
		t.Error("dm ShowDefinition(event): want not ok, DM8 Oracle mode has no event scheduler")
	}

	// No schema: the two-argument form resolves against the current schema.
	query, _, _, _ := dm.ShowDefinition("table", "", "t")
	if !strings.Contains(query, "GET_DDL('TABLE','T')") {
		t.Errorf("dm ShowDefinition(table, no schema) query = %q, want the two-argument form", query)
	}
	if strings.Contains(query, "'T',") {
		t.Errorf("dm ShowDefinition(table, no schema) query = %q, want no third argument", query)
	}

	// A crafted name is inert: the single quote is doubled inside the literal.
	query, _, _, _ = dm.ShowDefinition("table", "", "a'); DROP--")
	if strings.Contains(query, "'A'); DROP") {
		t.Errorf("dm ShowDefinition: crafted name escaped to %q, want the quote doubled", query)
	}
	if !strings.Contains(query, "''") {
		t.Errorf("dm ShowDefinition: crafted name query = %q, want the doubled-quote escape", query)
	}
}
