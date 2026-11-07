package backend

import "testing"

// TestGaussShowDefinition pins the spike: a table is printed through the native
// Gauss pg_get_tabledef, other kinds refuse, and the reference is carried as an
// SQL literal (single quotes doubled) rather than a quoted identifier.
func TestGaussShowDefinition(t *testing.T) {
	gauss := Must(EngineGauss)

	query, column, hasTerm, ok := gauss.ShowDefinition("table", "", "t1")
	if !ok {
		t.Fatal("gauss ShowDefinition(table): want ok")
	}
	if want := "SELECT pg_get_tabledef('t1')"; query != want {
		t.Errorf("query = %q, want %q", query, want)
	}
	if column != "pg_get_tabledef" {
		t.Errorf("column = %q, want pg_get_tabledef", column)
	}
	if !hasTerm {
		t.Error("gauss pg_get_tabledef output already carries ';' — hasTerminator must be true so show does not print ';;'")
	}

	query, _, _, ok = gauss.ShowDefinition("table", "public", "t1")
	if !ok {
		t.Fatal("gauss ShowDefinition(schema.table): want ok")
	}
	if want := "SELECT pg_get_tabledef('public.t1')"; query != want {
		t.Errorf("query = %q, want %q", query, want)
	}

	// A crafted name must not be able to break out of the SQL string literal.
	query, _, _, _ = gauss.ShowDefinition("table", "", "a'); DROP TABLE t--")
	if want := "SELECT pg_get_tabledef('a''); DROP TABLE t--')"; query != want {
		t.Errorf("escape failed: query = %q, want %q", query, want)
	}

	// Other kinds are not printable on this spike yet.
	for _, kind := range []string{"view", "trigger", "event", "procedure", "function"} {
		if _, _, _, ok := gauss.ShowDefinition(kind, "", "x"); ok {
			t.Errorf("gauss ShowDefinition(%q): want not ok", kind)
		}
	}

	// gauss is a distinct engine id but inherits the PostgreSQL defaults.
	if gauss.Engine() != EngineGauss {
		t.Errorf("Engine = %q, want gauss", gauss.Engine())
	}
	if gauss.DefaultPort() != 5432 {
		t.Errorf("DefaultPort = %d, want 5432", gauss.DefaultPort())
	}
}

// TestGaussParseIsHidden verifies the spike engine resolves through Parse but is
// not advertised in the --engine list that feeds the help text and the oracle.
func TestGaussParseIsHidden(t *testing.T) {
	e, err := Parse("gauss")
	if err != nil || e != EngineGauss {
		t.Fatalf("Parse(gauss) = %v, %v; want gauss, nil", e, err)
	}
	for _, name := range []string{"gaussdb", "openGauss"} {
		if e, err := Parse(name); err != nil || e != EngineGauss {
			t.Errorf("Parse(%q) = %v, %v; want gauss", name, e, err)
		}
	}
	for _, listed := range Engines() {
		if listed == "gauss" {
			t.Error("gauss must stay out of Engines()/engineOrder while it is a spike")
		}
	}
}

// TestGaussShowRefusals pins that CmdShow on gauss refuses every kind except
// table (in either its short or full spelling, since cmd folds the SHORT name),
// and that every other command still follows the PostgreSQL rules.
func TestGaussShowRefusals(t *testing.T) {
	gauss := Must(EngineGauss)

	for _, r := range gauss.Refusals(CmdShow) {
		for _, ok := range []string{"table", "tab"} {
			if r.Applies(RunOptions{ShowKind: ok}) {
				t.Errorf("gauss must NOT refuse show %s", ok)
			}
		}
		for _, kind := range []string{"vws", "view", "evt", "event", "fcn", "function"} {
			if !r.Applies(RunOptions{ShowKind: kind}) {
				t.Errorf("gauss must refuse show %s", kind)
			}
		}
	}
	// Export refusals are inherited from pg unchanged.
	if len(gauss.Refusals(CmdExport)) != len(Must(EnginePG).Refusals(CmdExport)) {
		t.Error("gauss CmdExport refusals diverged from pg")
	}
}
