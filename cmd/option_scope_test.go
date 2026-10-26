package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/imex/internal/config"
)

// delModifierFlags are the DEL layout options, present with these names on both
// subcommands. Listed here rather than read from the registry so the test stays an
// independent check that a new layout option cannot escape the registry.
var delModifierFlags = []string{
	"char-del", "col-del", "row-del", "escape", "no-escape",
	"timestamp-fmt", "date-fmt", "no-char-del", "no-col-names",
}

func scopeFlags(scopes config.OptionScopes) map[string]bool {
	out := make(map[string]bool, len(scopes))
	for _, sc := range scopes {
		out[sc.Flag] = true
	}
	return out
}

// TestScopeMarkersReachTheHelpText is the B half of the format-scope work: every
// option the validator rejects for a format has to say so in its own help line,
// from the same registry entry that produces the rejection.
func TestScopeMarkersReachTheHelpText(t *testing.T) {
	cases := []struct {
		name   string
		cmd    *cobra.Command
		scopes config.OptionScopes
	}{
		{"export", exportCmd, config.ExportOptionScopes()},
		{"import", importCmd, config.ImportOptionScopes()},
	}

	for _, tc := range cases {
		for _, sc := range tc.scopes {
			flag := tc.cmd.Flags().Lookup(sc.Flag)
			if flag == nil {
				t.Errorf("%s: the registry scopes --%s but the command has no such flag", tc.name, sc.Flag)
				continue
			}
			if !strings.HasSuffix(flag.Usage, sc.Hint) {
				t.Errorf("%s: --%s help reads %q, which does not end in the registry marker %q", tc.name, sc.Flag, flag.Usage, sc.Hint)
			}
		}

		// And the marker has to survive into the rendered help, not just the
		// flag's own Usage string: this is what the operator reads.
		usage := tc.cmd.Flags().FlagUsages()
		for _, sc := range tc.scopes {
			if !strings.Contains(usage, sc.Hint) {
				t.Errorf("%s: --help does not show marker %q for --%s", tc.name, sc.Hint, sc.Flag)
			}
		}
	}
}

// TestEveryDelModifierFlagIsScoped catches the other direction: a layout option
// added to a subcommand without a registry entry would be silently accepted with
// a format that ignores it, and would be missing from the configuration record.
func TestEveryDelModifierFlagIsScoped(t *testing.T) {
	cases := []struct {
		name   string
		scopes config.OptionScopes
	}{
		{"export", config.ExportOptionScopes()},
		{"import", config.ImportOptionScopes()},
	}
	for _, tc := range cases {
		registered := scopeFlags(tc.scopes)
		for _, flag := range delModifierFlags {
			if !registered[flag] {
				t.Errorf("%s: --%s is a DEL layout option but the registry does not scope it", tc.name, flag)
			}
		}
	}
}

// TestAnnotateOptionScopesIsIdempotent matters because the command tree is
// package-level: init registers the flags once, but a second pass over the same
// command must not append a second marker to the same help line.
func TestAnnotateOptionScopesIsIdempotent(t *testing.T) {
	before := map[string]string{}
	for _, sc := range config.ExportOptionScopes() {
		if flag := exportCmd.Flags().Lookup(sc.Flag); flag != nil {
			before[sc.Flag] = flag.Usage
		}
	}

	annotateOptionScopes(exportCmd, config.ExportOptionScopes())

	for _, sc := range config.ExportOptionScopes() {
		flag := exportCmd.Flags().Lookup(sc.Flag)
		if flag == nil {
			continue
		}
		if flag.Usage != before[sc.Flag] {
			t.Errorf("--%s help changed on a second pass: %q -> %q", sc.Flag, before[sc.Flag], flag.Usage)
		}
		if got := strings.Count(flag.Usage, sc.Hint); got != 1 {
			t.Errorf("--%s carries the marker %q %d times, want once", sc.Flag, sc.Hint, got)
		}
	}
}
