package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/imex/internal/backend"
	"github.com/imex/internal/config"
	"github.com/imex/internal/show"
)

// TestShowSQLDialectFlagFace pins the new option's CLI face (contract face ①).
//
// Two things are being pinned here that are easy to lose later. The first is
// uniformity: --sql-dialect belongs on every show leaf, the way --header does,
// so that `show vws --sql-dialect mysql` reaches the "table only" refusal rather
// than an unknown-flag error that depends on which leaf the operator typed. The
// second is that the option is *shared* with export rather than duplicated: one
// name, one enum, one order of values, and a usage line that lists them the same
// way, because the question — "say this in another grammar" — is one question.
func TestShowSQLDialectFlagFace(t *testing.T) {
	enum := "(" + enumNames(config.SQLDialects()) + ")"
	exportFlag := exportCmd.Flags().Lookup("sql-dialect")
	if exportFlag == nil {
		t.Fatal("export has no --sql-dialect to compare against")
	}
	if !strings.Contains(exportFlag.Usage, enum) {
		t.Fatalf("export --sql-dialect usage %q does not list %s", exportFlag.Usage, enum)
	}

	for _, parent := range []*cobra.Command{newShowCmd(), newListCmd()} {
		for _, sub := range parent.Commands() {
			if parent.Name() == "list" {
				// `list` has nothing to convert: it prints names, not definitions.
				if sub.Flags().Lookup("sql-dialect") != nil {
					t.Errorf("list %s carries --sql-dialect, which no list output can honour", sub.Name())
				}
				continue
			}
			f := sub.Flags().Lookup("sql-dialect")
			if f == nil {
				t.Errorf("show %s has no --sql-dialect", sub.Name())
				continue
			}
			if f.Value.Type() != "string" {
				t.Errorf("show %s --sql-dialect type = %q, want string", sub.Name(), f.Value.Type())
			}
			if f.DefValue != string(config.SQLDialectAuto) {
				t.Errorf("show %s --sql-dialect default = %q, want %q", sub.Name(), f.DefValue, config.SQLDialectAuto)
			}
			if f.Shorthand != "" {
				t.Errorf("show %s --sql-dialect has shorthand %q, want none", sub.Name(), f.Shorthand)
			}
			if !strings.Contains(f.Usage, enum) {
				t.Errorf("show %s --sql-dialect usage %q does not list %s", sub.Name(), f.Usage, enum)
			}
			// The oracle file carries every help line within 118 bytes, so the
			// new usage string is checked where it is actually printed: in the
			// leaf's whole flag block, default suffix included.
			for _, line := range strings.Split(sub.Flags().FlagUsages(), "\n") {
				if len(line) > 118 {
					t.Errorf("show %s help line is %d bytes, past the 118 the oracle keeps: %q",
						sub.Name(), len(line), line)
				}
			}
		}
	}
	for _, c := range []*cobra.Command{importCmd, newExecCmd()} {
		if c.Flags().Lookup("sql-dialect") != nil {
			t.Errorf("%s carries --sql-dialect, which its own grammar already fixes", c.Name())
		}
	}
}

// TestShowDialectScopeRefusals pins face ③: which asks are answered with a
// message and which are served. The order of the two refusals is part of it —
// a kind that has no rebuild at all is refused whatever grammar was named, and
// the pair check only runs for the kind that does.
func TestShowDialectScopeRefusals(t *testing.T) {
	tableKind, _ := show.Lookup("tab")
	viewKind, _ := show.Lookup("vws")
	triggerKind, _ := show.Lookup("trg")
	prcKind, _ := show.Lookup("prc")
	fcnKind, _ := show.Lookup("fcn")
	schKind, _ := show.Lookup("sch")
	evtKind, _ := show.Lookup("evt")
	for _, tc := range []struct {
		name    string
		engine  backend.Engine
		kind    show.Kind
		dialect string
		want    string
	}{
		{"auto is verbatim", backend.EngineDM, tableKind, "auto", ""},
		{"the engine's own grammar is verbatim", backend.EngineDM, tableKind, "dm", ""},
		{"mysql to mysql is verbatim", backend.EngineMySQL, tableKind, "mysql", ""},
		{"gauss to pg is verbatim", backend.EngineGauss, tableKind, "pg", ""},
		{"dm to mysql converts", backend.EngineDM, tableKind, "mysql", ""},
		{"pg to mysql is not a pair", backend.EnginePG, tableKind, "mysql",
			"--engine pg to --sql-dialect mysql is not a conversion this build makes"},
		{"mysql to pg is not a pair", backend.EngineMySQL, tableKind, "pg",
			"--engine mysql to --sql-dialect pg is not a conversion this build makes"},
		{"dm to pg is not a pair", backend.EngineDM, tableKind, "pg",
			"--engine dm to --sql-dialect pg is not a conversion this build makes"},
		{"dm to mysql converts a view too", backend.EngineDM, viewKind, "mysql", ""},
		{"a view of an unsupported pair hits the pair check", backend.EnginePG, viewKind, "mysql",
			"--engine pg to --sql-dialect mysql is not a conversion this build makes"},
		{"auto never reaches the kind check", backend.EngineDM, viewKind, "auto", ""},
		{"a trigger names its PL/SQL body", backend.EngineDM, triggerKind, "mysql",
			"cannot convert show trg: a trigger's PL/SQL body"},
		{"a procedure names its declaration", backend.EngineDM, prcKind, "mysql",
			"cannot convert show prc: a stored routine's PL/SQL"},
		{"a function names its declaration", backend.EngineDM, fcnKind, "mysql",
			"cannot convert show fcn: a stored routine's PL/SQL"},
		{"a schema names the user/database mismatch", backend.EngineDM, schKind, "mysql",
			"cannot convert show sch: a schema is a user in Dameng but a database in MySQL"},
		{"an event names the missing source", backend.EngineDM, evtKind, "mysql",
			"cannot convert show evt: Dameng has no event scheduler"},
	} {
		err := validateShowDialect(tc.engine, tc.kind, tc.dialect)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: got %v, want the run to proceed", tc.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: got no error, want %q", tc.name, tc.want)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %q, want it to say %q", tc.name, err, tc.want)
		}
	}
}

// TestShowDialectValueReachesConfig pins the wiring between the leaf's flag and
// the field show.Run consults: the default is auto, the flag writes the name it
// was given, and a value the enum does not carry is rejected by the validator
// before the scope check ever sees it.
func TestShowDialectValueReachesConfig(t *testing.T) {
	// A leaf of its own: the shipped tree already has its flags registered, and
	// registering them twice is what pflag's "flag redefined" panic is for.
	leaf := &cobra.Command{Use: "tab"}
	cfg := config.NewShowConfig()
	registerShowFlags(leaf, cfg)
	if cfg.SQLDialect != string(config.SQLDialectAuto) {
		t.Fatalf("NewShowConfig SQLDialect = %q, want auto", cfg.SQLDialect)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, string(config.SQLDialectAuto)},
		{[]string{"--sql-dialect", "mysql"}, "mysql"},
		{[]string{"--sql-dialect=pg"}, "pg"},
	} {
		cfg.SQLDialect = string(config.SQLDialectAuto)
		leaf.Flags().Lookup("sql-dialect").Value.Set("auto")
		if tc.args != nil {
			if err := leaf.Flags().Parse(tc.args); err != nil {
				t.Fatalf("parse %v: %v", tc.args, err)
			}
		}
		if cfg.SQLDialect != tc.want {
			t.Errorf("%v leaves SQLDialect = %q, want %q", tc.args, cfg.SQLDialect, tc.want)
		}
	}
	if err := validateSQLDialect("oracle"); err == nil {
		t.Error("--sql-dialect oracle passed validation")
	} else if !strings.Contains(err.Error(), enumNames(config.SQLDialects())) {
		t.Errorf("bad-name message %q does not list the accepted values", err)
	}
}
