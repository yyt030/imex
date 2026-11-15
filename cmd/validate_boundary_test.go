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

// TestCLIDelimiterParseBoundary drives the full parse-and-validate layer with the
// boundary matrix that used to be checked only by hand against the built binary.
// It pins three behaviours that a value-level unit test cannot see from here:
//
//   - values are compared for collisions *after* parsing, so two different
//     spellings of the same byte (0x09 and \t) are still refused;
//   - a value that cannot be parsed is reported as that option's own error,
//     before any collision check, and in the fixed char/col/row/escape order;
//   - the two flags folded into delimiter values in 187/188 are gone, so their
//     old spellings are unknown-flag usage errors, not silent aliases.
//
// Exit-code 2 (common.IsUsageError) is the whole contract at this layer; a nil
// error means the options parsed and validation would let the run reach the DB.
func TestCLIDelimiterParseBoundary(t *testing.T) {
	base := []string{"-h", "127.0.0.1", "-u", "root", "-p", "pass", "-D", "mydb"}
	exportArgs := append(append([]string{}, base...), "-o", "/tmp/out.del", "-q", "SELECT 1", "-f", "del")
	importArgs := append(append([]string{}, base...), "-i", "/tmp/in.del", "-t", "users", "-f", "del")

	type outcome struct {
		reject   bool
		contains string // substring the usage error must name
	}
	cases := []struct {
		name  string
		flags []string
		outcome
	}{
		// --- hex parsing: valid forms pass, malformed forms are rejected ---
		{"hex tab", []string{"--col-del", "0x09"}, outcome{}},
		{"hex CRLF row", []string{"--row-del", "0x0D0A"}, outcome{}},
		{"hex multi 0x7C0x7C", []string{"--col-del", "0x7C0x7C"}, outcome{}},
		{"hex uppercase 0X", []string{"--col-del", "0X7C"}, outcome{}},
		{"hex odd length", []string{"--col-del", "0x9"}, outcome{true, "--col-del"}},
		{"hex bad digits", []string{"--col-del", "0xZZ"}, outcome{true, "--col-del"}},
		{"hex bare prefix", []string{"--col-del", "0x"}, outcome{true, "--col-del"}},
		// --- escape-sequence parsing ---
		{"escape tab", []string{"--col-del", `\t`}, outcome{}},
		{"escape newline", []string{"--row-del", `\n`}, outcome{}},
		{"escape backslash", []string{"--col-del", `\\`}, outcome{}},
		{"escape nul", []string{"--col-del", `\0`}, outcome{}},
		{"unknown escape kept", []string{"--col-del", `\x`}, outcome{}},
		// --- collisions are judged on the parsed bytes ---
		{"hex tab equals literal tab", []string{"--col-del", "0x09", "--row-del", `\t`}, outcome{true, "--col-del"}},
		{"hex comma equals col-del", []string{"--char-del", "0x2C", "--col-del", ","}, outcome{true, "--char-del"}},
		{"hex newline equals row-del", []string{"--char-del", "0x0A", "--row-del", `\n`}, outcome{true, "--char-del"}},
		{"hex escape equals col-del", []string{"--escape", "0x7C", "--col-del", "|"}, outcome{true, "--escape"}},
		{"0x22 escape is the default double-write", []string{"--char-del", "0x22", "--escape", `"`}, outcome{}},
		// --- bad values win over collisions, in char/col/row/escape order ---
		{"bad col before bad escape", []string{"--col-del", "0x9", "--escape", "0xZZ"}, outcome{true, "--col-del"}},
		{"bad char before bad col", []string{"--char-del", "0x9", "--col-del", "0xZZ"}, outcome{true, "--char-del"}},
		// --- the two folded-away flags are unknown, not aliases ---
		{"removed --no-char-del", []string{"--no-char-del"}, outcome{true, "unknown flag"}},
		{"removed --no-escape", []string{"--no-escape"}, outcome{true, "unknown flag"}},
	}

	for _, c := range cases {
		for _, sub := range []string{"export", "import"} {
			args := exportArgs
			if sub == "import" {
				args = importArgs
			}
			err := validateOnly(t, sub, append(append([]string{}, args...), c.flags...))
			if !c.reject {
				if err != nil {
					t.Errorf("%s %s: unexpected error %v", sub, c.name, err)
				}
				continue
			}
			if err == nil {
				t.Errorf("%s %s: expected a usage error, got nil", sub, c.name)
				continue
			}
			// An unknown flag is produced by pflag's Parse, below the validation
			// layer, so it arrives here unwrapped; the command layer turns it
			// into exit 2 (pinned by TestCLIMatrix). What matters at this layer
			// is that the old spelling is refused at all, with that message.
			if c.contains != "unknown flag" && !common.IsUsageError(err) {
				t.Errorf("%s %s: %q must be a usage error (exit 2)", sub, c.name, err)
			}
			if !strings.Contains(err.Error(), c.contains) {
				t.Errorf("%s %s: error %q must name %q", sub, c.name, err, c.contains)
			}
		}
	}
}
