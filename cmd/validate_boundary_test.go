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
		// A disabled option is not part of the layout, so it cannot collide.
		{"char-del equal to col-del with --no-char-del", []string{"--char-del", ",", "--col-del", ",", "--no-char-del"}, false, ""},
		{"escape equal to col-del with --no-escape", []string{"--escape", "|", "--col-del", "|", "--no-escape"}, false, ""},
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
