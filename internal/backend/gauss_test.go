package backend

import (
	"strings"
	"testing"

	"github.com/imex/internal/config"
)

// TestGaussShowDefinition pins the native definition path: a table is printed
// through pg_get_tabledef, other kinds refuse, and the reference is carried as
// an SQL literal (single quotes doubled) rather than a quoted identifier.
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

	// Other kinds have no equivalent definition function on this engine.
	for _, kind := range []string{"view", "trigger", "event", "procedure", "function"} {
		if _, _, _, ok := gauss.ShowDefinition(kind, "", "x"); ok {
			t.Errorf("gauss ShowDefinition(%q): want not ok", kind)
		}
	}

	// gauss is a distinct engine id but inherits the PostgreSQL connection.
	if gauss.Engine() != EngineGauss {
		t.Errorf("Engine = %q, want gauss", gauss.Engine())
	}
	if gauss.DefaultPort() != 5432 {
		t.Errorf("DefaultPort = %d, want 5432", gauss.DefaultPort())
	}
}

// TestGaussParseIsPublic verifies the engine resolves through Parse AND is
// listed by Engines(), i.e. it appears in --engine help and in the invalid-value
// rejection list. It was a hidden spike until round 185 (real openGauss-lite
// 5.0.3 measurement); this test now pins the opposite contract.
func TestGaussParseIsPublic(t *testing.T) {
	e, err := Parse("gauss")
	if err != nil || e != EngineGauss {
		t.Fatalf("Parse(gauss) = %v, %v; want gauss, nil", e, err)
	}
	for _, name := range []string{"gaussdb", "openGauss", "GAUSS", "  gauss  "} {
		if e, err := Parse(name); err != nil || e != EngineGauss {
			t.Errorf("Parse(%q) = %v, %v; want gauss", name, e, err)
		}
	}
	found := false
	for _, listed := range Engines() {
		if listed == "gauss" {
			found = true
		}
	}
	if !found {
		t.Error("gauss must be listed by Engines()/engineOrder now that it is a public engine")
	}
	if got := strings.Join(Engines(), "/"); got != "mysql/pg/dm/gauss" {
		t.Errorf("Engines() order = %q, want mysql/pg/dm/gauss (gauss appended last)", got)
	}
}

// TestGaussCatalogueSQLIsNinetyTwoCompatible pins the two catalog statements
// that had to be overridden. The inherited PostgreSQL forms use columns and
// syntax the Gauss kernel does not have; these were PREPARE/EXECUTE-tested on
// openGauss-lite 5.0.3 and must not regress into the pg forms.
func TestGaussCatalogueSQLIsNinetyTwoCompatible(t *testing.T) {
	gauss := Must(EngineGauss)

	gen := gauss.GeneratedColumnsSQL()
	for _, forbidden := range []string{"attgenerated", "attidentity", "pg_catalog.pg_attribute"} {
		if strings.Contains(gen, forbidden) {
			t.Errorf("GeneratedColumnsSQL must not use %q on a 9.2 kernel: %s", forbidden, gen)
		}
	}
	for _, required := range []string{"information_schema.columns", "is_generated", "is_identity", "$1", "$2"} {
		if !strings.Contains(gen, required) {
			t.Errorf("GeneratedColumnsSQL must read %q: %s", required, gen)
		}
	}

	uniq := gauss.UniqueKeyIndexColumnsSQL()
	for _, forbidden := range []string{"LATERAL", "WITH ORDINALITY"} {
		if strings.Contains(uniq, forbidden) {
			t.Errorf("UniqueKeyIndexColumnsSQL must not use %q on a 9.2 kernel: %s", forbidden, uniq)
		}
	}
	for _, required := range []string{"information_schema.table_constraints", "information_schema.key_column_usage", "ordinal_position", "PRIMARY KEY"} {
		if !strings.Contains(uniq, required) {
			t.Errorf("UniqueKeyIndexColumnsSQL must read %q: %s", required, uniq)
		}
	}

	// The released PostgreSQL engine keeps its exact statements: face ⑤ unchanged.
	pg := Must(EnginePG)
	if !strings.Contains(pg.GeneratedColumnsSQL(), "attgenerated") {
		t.Error("the PostgreSQL GeneratedColumnsSQL must still read attgenerated")
	}
	if !strings.Contains(pg.UniqueKeyIndexColumnsSQL(), "WITH ORDINALITY") {
		t.Error("the PostgreSQL UniqueKeyIndexColumnsSQL must still use WITH ORDINALITY")
	}
}

// TestGaussUpsertIsRefused pins the two layers of the insert_update boundary:
// the CLI refusal (before dialling) and the library-level UpsertSuffix error.
func TestGaussUpsertIsRefused(t *testing.T) {
	gauss := Must(EngineGauss)
	if _, _, err := gauss.UpsertSuffix("t", []string{"a"}, []string{"id"}); err == nil {
		t.Error("gauss UpsertSuffix must refuse to build ON CONFLICT")
	}
	if gauss.UpsertNeedsTarget() {
		t.Error("gauss must not ask for an ON CONFLICT target it cannot use")
	}

	var refused bool
	for _, r := range gauss.Refusals(CmdImport) {
		if r.Applies(RunOptions{Mode: string(config.ImportModeInsertUpdate)}) {
			refused = true
			if !strings.Contains(r.Reason, "ON CONFLICT") {
				t.Errorf("insert_update refusal must name the missing syntax: %q", r.Reason)
			}
		}
	}
	if !refused {
		t.Error("gauss CmdImport must refuse --mode insert_update before dialling")
	}
	for _, mode := range []config.ImportMode{config.ImportModeInsert, config.ImportModeReplace} {
		for _, r := range gauss.Refusals(CmdImport) {
			if r.Applies(RunOptions{Mode: string(mode)}) {
				t.Errorf("gauss must allow --mode %s", mode)
			}
		}
	}
}

// TestGaussShowRefusals pins that CmdShow on gauss refuses every kind except
// table (in either its short or full spelling, since cmd folds the SHORT name),
// and that every other command is stated in this engine's own words rather than
// PostgreSQL's.
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
	// Export refusals are the same single byte-safety rule, but gauss owns its
	// wording; it must not claim to be PostgreSQL.
	if len(gauss.Refusals(CmdExport)) != len(Must(EnginePG).Refusals(CmdExport)) {
		t.Error("gauss CmdExport refusals diverged from pg")
	}
	for _, c := range []Command{CmdExport, CmdImport, CmdList, CmdShow} {
		for _, r := range gauss.Refusals(c) {
			if strings.Contains(r.Reason, "PostgreSQL") {
				t.Errorf("gauss %d refusal must not call itself PostgreSQL: %q", c, r.Reason)
			}
		}
	}
	// The charset rejection names this engine, not pg.
	if err := gauss.ValidateCharset("nonsense"); err == nil {
		t.Error("gauss must reject an unknown charset")
	} else if !strings.Contains(err.Error(), "--engine gauss") || strings.Contains(err.Error(), "--engine pg:") {
		t.Errorf("gauss charset error must name gauss: %q", err.Error())
	}
	if err := gauss.ValidateCharset("utf8mb4"); err != nil {
		t.Errorf("gauss must accept utf8mb4: %v", err)
	}
}
