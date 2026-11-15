package cmd

import (
	"strings"
	"testing"

	"github.com/imex/internal/common"
)

// Two DEL layout options that resolve to the same bytes make the file impossible
// to read back, and until now both sides accepted the pair and reported success:
// `--col-del , --row-del ,` wrote a 4-row dump as one long line (it imported later
// with "target table has no column(s)"), and `--char-del ,` wrote ",id,,,v,". The
// failure surfaced in whoever tried to use the file, so the collision is refused
// where it is created. --escape equal to --char-del is deliberately legal: it is
// the shipped default, and doubling the quote is how MySQL escapes it.
func TestCLIDelimiterCollisions(t *testing.T) {
	base := []string{"-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb"}
	exportArgs := append(append([]string{}, base...), "-o", "/tmp/out.del", "-q", "SELECT 1", "-f", "del")
	importArgs := append(append([]string{}, base...), "-i", "/tmp/in.del", "-t", "users", "-f", "del")

	cases := []struct {
		name     string
		flags    []string
		reject   bool
		contains string
	}{
		{"defaults collide nowhere", nil, false, ""},
		{"col-del equal to row-del", []string{"--col-del", ",", "--row-del", ","}, true, "--col-del"},
		{"char-del equal to col-del", []string{"--char-del", ",", "--col-del", ","}, true, "--char-del"},
		{"char-del equal to row-del", []string{"--char-del", `@`, "--row-del", `@`}, true, "--char-del"},
		{"escape equal to col-del", []string{"--escape", "|", "--col-del", "|"}, true, "--escape"},
		{"escape equal to row-del", []string{"--escape", "|", "--row-del", "|"}, true, "--escape"},
		{"escape equal to char-del is the shipped default", []string{"--escape", `"`, "--char-del", `"`}, false, ""},
		// An empty --char-del (bare fields) is not part of the layout either, so
		// it cannot collide with the delimiters that stay in use.
		{"empty char-del with a column delimiter", []string{"--char-del", "", "--col-del", ","}, false, ""},
		{"empty char-del with a row delimiter", []string{"--char-del", "", "--row-del", `\n`}, false, ""},
		// A disabled option is not part of the layout: an empty --escape cannot
		// collide with a column or row delimiter that is still in use.
		{"empty escape with a column delimiter", []string{"--escape", "", "--col-del", ","}, false, ""},
		{"empty escape with a row delimiter", []string{"--escape", "", "--row-del", `\n`}, false, ""},
		{"empty escape and empty char-del", []string{"--escape", "", "--char-del", ""}, false, ""},
		{"multi-char delimiters that differ", []string{"--col-del", ",,", "--row-del", `\n`}, false, ""},
		{"escaped delimiters compare after unescaping", []string{"--col-del", `\t`, "--row-del", `\t`}, true, "--col-del"},
	}

	for _, c := range cases {
		for _, sub := range []string{"export", "import"} {
			args := exportArgs
			if sub == "import" {
				args = importArgs
			}
			err := validateOnly(t, sub, append(append([]string{}, args...), c.flags...))
			if c.reject {
				if err == nil {
					t.Errorf("%s %s: expected usage error, got nil", sub, c.name)
					continue
				}
				if !common.IsUsageError(err) {
					t.Errorf("%s %s: %q must be a usage error (exit code 2)", sub, c.name, err)
				}
				if !strings.Contains(err.Error(), c.contains) || !strings.Contains(err.Error(), "ambiguous") {
					t.Errorf("%s %s: error %q must name %s and say why", sub, c.name, err, c.contains)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s %s: unexpected error %v", sub, c.name, err)
			}
		}
	}
}

// TestCLIEmptyCharDelAndEscapeAreTheOnlyLegalEmptyDelimiters pins the asymmetry
// that makes these folds safe: --char-del may be empty (bare fields, what
// --no-char-del used to select) and --escape may be empty (no escaping, what
// --no-escape used to select), while --col-del and --row-del keep the "cannot be
// empty" rule, because a file with no column or row delimiter cannot be read
// back at all.
func TestCLIEmptyCharDelAndEscapeAreTheOnlyLegalEmptyDelimiters(t *testing.T) {
	base := []string{"-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb"}
	exportArgs := append(append([]string{}, base...), "-o", "/tmp/out.del", "-q", "SELECT 1", "-f", "del")
	importArgs := append(append([]string{}, base...), "-i", "/tmp/in.del", "-t", "users", "-f", "del")

	for _, sub := range []string{"export", "import"} {
		args := exportArgs
		if sub == "import" {
			args = importArgs
		}
		for _, name := range []string{"--char-del", "--escape"} {
			if err := validateOnly(t, sub, append(append([]string{}, args...), name, "")); err != nil {
				t.Errorf("%s %s \"\": must be legal, got %v", sub, name, err)
			}
		}
		// Both at once: every field is written verbatim, which is a layout the
		// reader can still split on --col-del, so it must be accepted.
		if err := validateOnly(t, sub, append(append([]string{}, args...), "--char-del", "", "--escape", "")); err != nil {
			t.Errorf("%s --char-del \"\" --escape \"\": must be legal, got %v", sub, err)
		}
		for _, name := range []string{"--col-del", "--row-del"} {
			err := validateOnly(t, sub, append(append([]string{}, args...), name, ""))
			if err == nil {
				t.Errorf("%s %s \"\": must stay a usage error", sub, name)
				continue
			}
			if !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "delimiter cannot be empty") {
				t.Errorf("%s %s \"\": error %q must name the flag and the empty-delimiter rule", sub, name, err)
			}
			if !common.IsUsageError(err) {
				t.Errorf("%s %s \"\": must exit 2", sub, name)
			}
		}
	}
}
