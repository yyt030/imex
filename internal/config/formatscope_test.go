package config

import (
	"strings"
	"testing"
)

// changedOf returns a Flags().Changed stand-in for the flags the test pretends
// the user passed.
func changedOf(given ...string) func(string) bool {
	set := make(map[string]bool, len(given))
	for _, f := range given {
		set[f] = true
	}
	return func(name string) bool { return set[name] }
}

func TestOptionScopesAppliesTo(t *testing.T) {
	cases := []struct {
		scope  string
		flag   string
		format string
		want   bool
	}{
		{"export", "statement-size", FormatSQL, true},
		{"export", "statement-size", FormatDel, false},
		{"export", "statement-size", FormatParquet, false},
		{"export", "create", FormatSQL, true},
		{"export", "create", FormatDel, false},
		{"export", "extended-insert", FormatSQL, true},
		{"export", "extended-insert", FormatParquet, false},
		{"export", "char-del", FormatDel, true},
		{"export", "char-del", FormatSQL, false},
		{"export", "date-fmt", FormatSQL, false},
		{"export", "hex-blob", FormatDel, true},
		{"export", "hex-blob", FormatSQL, true},
		{"export", "hex-blob", FormatParquet, false},
		{"export", "no-hex-blob", FormatParquet, false},
		// An unregistered flag is format-independent, so it always applies.
		{"export", "threads", FormatParquet, true},
		{"export", "default-charset", FormatDel, true},
		{"import", "col-del", FormatDel, true},
		{"import", "col-del", FormatParquet, false},
		// Import's --statement-size bounds the batches the importer builds for
		// any input format, so it is deliberately not scoped.
		{"import", "statement-size", FormatParquet, true},
		{"import", "mode", FormatDel, true},
	}

	for _, tc := range cases {
		var scopes OptionScopes
		if tc.scope == "export" {
			scopes = ExportOptionScopes()
		} else {
			scopes = ImportOptionScopes()
		}
		if got := scopes.AppliesTo(tc.flag, tc.format); got != tc.want {
			t.Errorf("%s %s AppliesTo(%q) = %v, want %v", tc.scope, "--"+tc.flag, tc.format, got, tc.want)
		}
	}
}

// TestOptionScopeViolation pins the wording the CLI has always produced: the
// registry may render the messages, but a rejected run has to read exactly as it
// did when each group had its own hand-written checker.
func TestOptionScopeViolation(t *testing.T) {
	cases := []struct {
		name    string
		scopes  OptionScopes
		changed func(string) bool
		format  string
		want    string
	}{
		{
			name:    "sql option with del",
			scopes:  ExportOptionScopes(),
			changed: changedOf("statement-size"),
			format:  FormatDel,
			want:    "--statement-size can only be used with --format sql",
		},
		{
			name:    "del option with sql",
			scopes:  ExportOptionScopes(),
			changed: changedOf("col-del"),
			format:  FormatSQL,
			want:    "--col-del can only be used with --format del",
		},
		{
			name:    "import del option with parquet",
			scopes:  ImportOptionScopes(),
			changed: changedOf("no-col-names"),
			format:  FormatParquet,
			want:    "--no-col-names can only be used with --format del",
		},
		{
			name:    "binary encoding with parquet",
			scopes:  ExportOptionScopes(),
			changed: changedOf("hex-blob"),
			format:  FormatParquet,
			want:    "--hex-blob cannot be used with --format parquet: binary columns are already stored as binary",
		},
		{
			name:    "first registered flag wins",
			scopes:  ExportOptionScopes(),
			changed: changedOf("col-del", "create"),
			format:  FormatParquet,
			want:    "--create can only be used with --format sql",
		},
		{
			name:    "format that reads its own options",
			scopes:  ExportOptionScopes(),
			changed: changedOf("extended-insert", "hex-blob"),
			format:  FormatSQL,
		},
		{
			name:    "defaults are not violations",
			scopes:  ExportOptionScopes(),
			changed: func(string) bool { return false },
			format:  FormatParquet,
		},
	}

	for _, tc := range cases {
		msg, bad := tc.scopes.Violation(tc.changed, tc.format)
		if tc.want == "" {
			if bad {
				t.Errorf("%s: unexpected violation %q", tc.name, msg)
			}
			continue
		}
		if !bad {
			t.Errorf("%s: want violation %q, got none", tc.name, tc.want)
			continue
		}
		if msg != tc.want {
			t.Errorf("%s: violation = %q, want %q", tc.name, msg, tc.want)
		}
	}
}

// TestOptionScopeRegistryIsComplete guards the registry itself, so an entry
// cannot be added that the validator or the help text would silently drop: a
// missing Hint is a flag whose scope `--help` never mentions, and a missing
// Reject is a usage error that would be reported with an empty message.
func TestOptionScopeRegistryIsComplete(t *testing.T) {
	allFormats := map[string]bool{FormatDel: true, FormatSQL: true, FormatParquet: true}

	for name, scopes := range map[string]OptionScopes{
		"export": ExportOptionScopes(),
		"import": ImportOptionScopes(),
	} {
		seen := map[string]bool{}
		for _, sc := range scopes {
			if seen[sc.Flag] {
				t.Errorf("%s: --%s registered twice", name, sc.Flag)
			}
			seen[sc.Flag] = true

			if strings.HasPrefix(sc.Flag, "-") {
				t.Errorf("%s: --%s must be registered without the leading dashes", name, sc.Flag)
			}
			if len(sc.Formats) == 0 || len(sc.Formats) == len(allFormats) {
				t.Errorf("%s: --%s lists %d formats; a scoped option has to leave at least one format out, and listing all of them makes it format-independent", name, sc.Flag, len(sc.Formats))
			}
			for _, f := range sc.Formats {
				if !allFormats[f] {
					t.Errorf("%s: --%s names unknown format %q", name, sc.Flag, f)
				}
			}
			if !strings.Contains(sc.Hint, "--format") {
				t.Errorf("%s: --%s help marker %q does not name a format", name, sc.Flag, sc.Hint)
			}
			inScope := func(f string) bool {
				for _, got := range sc.Formats {
					if got == f {
						return true
					}
				}
				return false
			}
			if sc.Reject == nil {
				t.Errorf("%s: --%s has no Reject", name, sc.Flag)
				continue
			}
			for _, f := range []string{FormatDel, FormatSQL, FormatParquet} {
				if inScope(f) {
					continue
				}
				if msg := sc.Reject(f); !strings.Contains(msg, "--"+sc.Flag) {
					t.Errorf("%s: --%s rejection for --format %s reads %q, which does not name the flag", name, sc.Flag, f, msg)
				}
			}
		}
	}
}

// TestDelModifierFlagsAgreeAcrossCommands states the one invariant the two
// registries have to keep: the DEL layout options exist on both subcommands, and
// the same nine on both, so a run cannot scope them on one side only.
func TestDelModifierFlagsAgreeAcrossCommands(t *testing.T) {
	delFlags := func(scopes OptionScopes) []string {
		var out []string
		for _, sc := range scopes {
			if len(sc.Formats) == 1 && sc.Formats[0] == FormatDel {
				out = append(out, sc.Flag)
			}
		}
		return out
	}

	exp, imp := delFlags(ExportOptionScopes()), delFlags(ImportOptionScopes())
	if strings.Join(exp, ",") != strings.Join(imp, ",") {
		t.Errorf("DEL-only flags differ: export %v, import %v", exp, imp)
	}
	// --date-fmt, --no-escape and --no-char-del were the three the configuration
	// record used to leave out; they have to be in the registry for it to print
	// them at all.
	for _, want := range []string{"date-fmt", "no-escape", "no-char-del"} {
		found := false
		for _, f := range imp {
			if f == want {
				found = true
			}
		}
		if !found {
			t.Errorf("import registry is missing DEL flag --%s", want)
		}
	}
}
