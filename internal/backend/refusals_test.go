package backend

import (
	"strings"
	"testing"
)

// TestRefusalTablesAreInReportingOrder pins the shape of each engine's tables:
// which command has rules, how many, in what order, and under what label.
//
// The order is the message a doubly-invalid run gets, so it is contract face ③ and
// lives with the dialect that owns the rules. cmd/engine_test.go asserts the whole
// sentences against the CLI; this pins the parts that must not be re-sorted, renamed
// or quietly dropped when a rule moves between files.
func TestRefusalTablesAreInReportingOrder(t *testing.T) {
	cases := []struct {
		c    Command
		want []string // option labels, in order
	}{
		{CmdExport, []string{"--format sql", "--threads", "--no-hex-blob", "--session-var"}},
		{CmdImport, []string{"--format sql", "--mode create", "--session-var"}},
		{CmdList, []string{"list events"}},
		{CmdShow, []string{"show table"}},
	}
	for _, tc := range cases {
		rules := Must(EnginePG).Refusals(tc.c)
		if len(rules) != len(tc.want) {
			t.Fatalf("%s on pg has %d rules, want %d", engineScopeName(tc.c), len(rules), len(tc.want))
		}
		for i, r := range rules {
			if got := r.Option(RunOptions{ShowKind: "table"}); got != tc.want[i] {
				t.Errorf("rule %d of %s = %q, want %q", i, engineScopeName(tc.c), got, tc.want[i])
			}
			if !strings.Contains(r.Reason, ",") && len(r.Reason) < 40 {
				t.Errorf("rule %d of %s does not explain and offer an alternative: %q", i, engineScopeName(tc.c), r.Reason)
			}
		}
	}
	// MySQL refuses nothing on any command, which is what makes the cmd-side loop
	// engine-blind rather than pg-blind.
	for _, c := range []Command{CmdExport, CmdImport, CmdList, CmdShow} {
		if rules := Must(EngineMySQL).Refusals(c); len(rules) != 0 {
			t.Errorf("MySQL %s must refuse nothing, got %d rules", engineScopeName(c), len(rules))
		}
	}
	// An unknown command scope has no rules, so a new command cannot inherit one by
	// accident.
	if rules := Must(EnginePG).Refusals(Command(99)); len(rules) != 0 {
		t.Errorf("unknown scope must yield no rules, got %d", len(rules))
	}
}

func engineScopeName(c Command) string {
	switch c {
	case CmdExport:
		return "export"
	case CmdImport:
		return "import"
	case CmdList:
		return "list"
	case CmdShow:
		return "show"
	}
	return "unknown"
}

// TestRefusalPredicatesFireOnValueNotDefault pins which input trips which rule, and
// equally that a run's shipped defaults trip nothing: the presence-only rules read
// the bool cmd folded in, so `--session-var` left alone must stay usable.
func TestRefusalPredicatesFireOnValueNotDefault(t *testing.T) {
	byLabel := map[string]func(RunOptions) bool{}
	for _, c := range []Command{CmdExport, CmdImport, CmdList, CmdShow} {
		for _, r := range Must(EnginePG).Refusals(c) {
			byLabel[r.Option(RunOptions{ShowKind: "table"})] = r.Applies
		}
	}
	cases := []struct {
		option string
		o      RunOptions
		want   bool
	}{
		{"--format sql", RunOptions{Format: "sql"}, true},
		{"--format sql", RunOptions{Format: "del"}, false},
		{"--format sql", RunOptions{Format: "parquet"}, false},
		{"--threads", RunOptions{Threads: 1}, false},
		{"--threads", RunOptions{Threads: 2}, true},
		{"--threads", RunOptions{Threads: 0}, false},
		{"--no-hex-blob", RunOptions{NoHexBlob: true}, true},
		{"--no-hex-blob", RunOptions{NoHexBlob: false}, false},
		{"--session-var", RunOptions{SessionVar: true}, true},
		{"--session-var", RunOptions{}, false},
		{"--mode create", RunOptions{Mode: "create"}, true},
		{"--mode create", RunOptions{Mode: "replace"}, false},
		{"list events", RunOptions{Object: "events"}, true},
		{"list events", RunOptions{Object: "tables"}, false},
		{"show table", RunOptions{ShowKind: "view"}, true},
		{"show table", RunOptions{}, false},
	}
	for _, tc := range cases {
		applies, ok := byLabel[tc.option]
		if !ok {
			t.Fatalf("no pg rule labelled %q", tc.option)
		}
		if got := applies(tc.o); got != tc.want {
			t.Errorf("%q on %+v = %v, want %v", tc.option, tc.o, got, tc.want)
		}
	}
	// A run that names no option at all trips nothing on any label, in either
	// direction: the tables decide, and they are consulted before anything else.
	for label, applies := range byLabel {
		if applies(RunOptions{}) {
			t.Errorf("%q fires on an unset run", label)
		}
	}
}

// TestValidateCharsetPinsThePostgresMessage pins the rejection sentence a pg run
// reads for a charset the server cannot name, byte for byte, and that every charset
// the option accepts on this engine passes. MySQL has no opinion here at all: the
// error text an unknown name produces is the server's, not the tool's.
func TestValidateCharsetPinsThePostgresMessage(t *testing.T) {
	want := `invalid --default-charset "nonsense" with --engine pg: PostgreSQL has no such client encoding (supported: utf8mb4, latin1, gbk, gb18030, ascii)`
	err := Must(EnginePG).ValidateCharset("nonsense")
	if err == nil || err.Error() != want {
		t.Fatalf("pg charset rejection = %v, want %q", err, want)
	}
	for _, charset := range []string{"utf8mb4", "UTF8MB4", " utf8 ", "latin1", "iso-8859-1", "gbk", "gb18030", "ascii", "us-ascii", "819", "936", "5488", "384", "utf8mb3", "utf-8", "1386", "1392"} {
		if err := Must(EnginePG).ValidateCharset(charset); err != nil {
			t.Errorf("pg must accept %q, got %v", charset, err)
		}
	}
	for _, charset := range []string{"utf8mb4", "nonsense", "", "gb2312"} {
		if err := Must(EngineMySQL).ValidateCharset(charset); err != nil {
			t.Errorf("MySQL must not reject %q, got %v", charset, err)
		}
	}
}

// TestEngineNameInRefusalMessageIsTheValue is the reason cmd formats the sentence
// with the engine rather than hard-coding "pg": a third dialect's rules must read as
// that dialect's name, and the label text here is what a run prints.
func TestEngineNameInRefusalMessageIsTheValue(t *testing.T) {
	rules := Must(EnginePG).Refusals(CmdList)
	if len(rules) != 1 {
		t.Fatalf("want one list rule, got %d", len(rules))
	}
	msg := rules[0].Option(RunOptions{Object: "events"}) + " is not supported with --engine " + string(EnginePG)
	if msg != "list events is not supported with --engine pg" {
		t.Errorf("refusal prefix = %q", msg)
	}
}
