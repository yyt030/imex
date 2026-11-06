package integration

// End-to-end tests for `imex show`: the six kinds, the definition text they
// print, and the object-name forms. They drive the compiled binary, so flag
// parsing, name splitting, the SHOW statement and the channel model are all
// exercised together. Skipped unless IMEX_TEST_DSN is set.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// runShowCLI runs `imex show ...` and returns stdout, stderr and the exit code
// separately. The split is the whole point of the command's contract, so the
// combined-output helper cannot be used here.
func (c *cliEnv) runShowCLI(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	full := append([]string{"show"}, append(append([]string{}, c.base...), args...)...)
	return runIMEXSplit(t, full)
}

// runShowNoDatabase is runShowCLI with the -D flag removed, so a test can supply
// the schema inside the object name instead.
func (c *cliEnv) runShowNoDatabase(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	restore := c.withDatabaseRemoved()
	defer restore()
	return c.runShowCLI(t, args...)
}

// runShowRaw runs `imex show ...` without the shared connection flags. The parent
// command owns no flags, so arguments that fail before dispatch (no kind, unknown
// kind) must be exercised without them.
func runShowRaw(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	return runIMEXSplit(t, append([]string{"show"}, args...))
}

// showFixtures creates one object of every kind `show` handles and returns their
// names plus the cleanup they registered.
type showFixtures struct {
	table, view, trigger, event, proc, fn string
}

func (f *showFixtures) names() map[string]string {
	return map[string]string{
		"tab": f.table, "vws": f.view, "trg": f.trigger,
		"evt": f.event, "prc": f.proc, "fcn": f.fn,
	}
}

func newShowFixtures(t *testing.T, env *cliEnv) *showFixtures {
	t.Helper()
	base := fmt.Sprintf("imex_it_show_%d", time.Now().UnixNano())
	f := &showFixtures{
		table:   base + "_t",
		view:    base + "_v",
		trigger: base + "_trg",
		event:   base + "_e",
		proc:    base + "_p",
		fn:      base + "_f",
	}

	env.exec(t, fmt.Sprintf("CREATE TABLE `%s` (`id` INT NOT NULL PRIMARY KEY, `name` VARCHAR(20) NULL, KEY `idx_%s` (`name`)) ENGINE=InnoDB COMMENT='show fixture'", f.table, base))
	t.Cleanup(func() { _, _ = env.db.Exec("DROP TABLE IF EXISTS `" + f.table + "`") })

	env.exec(t, fmt.Sprintf("CREATE VIEW `%s` AS SELECT `id`, `name` FROM `%s` WHERE `id` > 0", f.view, f.table))
	t.Cleanup(func() { _, _ = env.db.Exec("DROP VIEW IF EXISTS `" + f.view + "`") })

	env.exec(t, fmt.Sprintf("CREATE TRIGGER `%s` BEFORE INSERT ON `%s` FOR EACH ROW SET @imex_show_seen = 1", f.trigger, f.table))
	t.Cleanup(func() { _, _ = env.db.Exec("DROP TRIGGER IF EXISTS `" + f.trigger + "`") })

	env.exec(t, fmt.Sprintf("CREATE EVENT `%s` ON SCHEDULE EVERY 1 DAY DISABLE DO SELECT 1", f.event))
	t.Cleanup(func() { _, _ = env.db.Exec("DROP EVENT IF EXISTS `" + f.event + "`") })

	env.exec(t, fmt.Sprintf("CREATE PROCEDURE `%s`(IN `a` INT, OUT `b` VARCHAR(10)) BEGIN SELECT `a` INTO `b`; END", f.proc))
	t.Cleanup(func() { _, _ = env.db.Exec("DROP PROCEDURE IF EXISTS `" + f.proc + "`") })

	env.exec(t, fmt.Sprintf("CREATE FUNCTION `%s`(`x` INT) RETURNS INT DETERMINISTIC RETURN `x` + 1", f.fn))
	t.Cleanup(func() { _, _ = env.db.Exec("DROP FUNCTION IF EXISTS `" + f.fn + "`") })

	return f
}

// kindExpectation ties a CLI kind name to the SHOW statement behind it.
type kindExpectation struct {
	short, full, statement string
}

func TestShowCLIKinds(t *testing.T) {
	env := newCLILive(t)
	fix := newShowFixtures(t, env)

	kinds := []kindExpectation{
		{"tab", "table", "SHOW CREATE TABLE"},
		{"vws", "view", "SHOW CREATE VIEW"},
		{"trg", "trigger", "SHOW CREATE TRIGGER"},
		{"evt", "event", "SHOW CREATE EVENT"},
		{"prc", "procedure", "SHOW CREATE PROCEDURE"},
		{"fcn", "function", "SHOW CREATE FUNCTION"},
	}

	for _, k := range kinds {
		t.Run(k.short, func(t *testing.T) {
			name := fix.names()[k.short]
			// info level so the interaction echo, which lives at info, is visible.
			out, errOut, code := env.runShowCLI(t, "--log-level", "info", k.short, name)
			if code != 0 {
				t.Fatalf("exit %d, stderr:\n%s", code, errOut)
			}
			if !strings.HasPrefix(out, "CREATE") {
				t.Errorf("definition must be the server's CREATE text, got:\n%s", out)
			}
			if !strings.Contains(out, "`"+name+"`") {
				t.Errorf("definition does not name the object %q:\n%s", name, out)
			}
			// The tool adds exactly the terminator the server leaves out, so
			// `imex import -f sql` can read the file.
			if !strings.HasSuffix(out, ";\n") {
				t.Errorf("definition must end with \";\\n\", got %q", out)
			}
			if strings.HasSuffix(out, ";\n\n") {
				t.Error("terminator added twice")
			}
			// The result is the only thing on stdout; logs stay on stderr.
			if strings.Contains(out, "imex starting") || strings.Contains(out, "db interaction") {
				t.Errorf("log records leaked to stdout:\n%s", out)
			}
			if !strings.Contains(errOut, `sql="`+k.statement+" `") {
				t.Errorf("the SHOW statement was not echoed to the log:\n%s", errOut)
			}

			// The English word is an alias and must print the very same bytes.
			full, fullErr, code := env.runShowCLI(t, k.full, name)
			if code != 0 {
				t.Fatalf("alias %s exit %d, stderr:\n%s", k.full, code, fullErr)
			}
			if full != out {
				t.Errorf("alias %s differs from %s:\n%s\n%s", k.full, k.short, full, out)
			}
		})
	}
}

func TestShowCLINeverPrintsSessionAttributes(t *testing.T) {
	// The definition column is not at a fixed position: for trigger, procedure,
	// function it is the third column and for event the fourth, with sql_mode and
	// time_zone in between. A positional read printed sql_mode instead.
	env := newCLILive(t)
	fix := newShowFixtures(t, env)

	for _, short := range []string{"trg", "evt", "prc", "fcn"} {
		t.Run(short, func(t *testing.T) {
			name := fix.names()[short]
			out, errOut, code := env.runShowCLI(t, short, name)
			if code != 0 {
				t.Fatalf("exit %d, stderr:\n%s", code, errOut)
			}
			if strings.Contains(out, "STRICT_TRANS_TABLES") || strings.Contains(out, "NO_ZERO_DATE") {
				t.Errorf("sql_mode leaked into the definition:\n%s", out)
			}
			if strings.Contains(out, "SYSTEM") {
				t.Errorf("time_zone leaked into the definition:\n%s", out)
			}
		})
	}
}

func TestShowCLIQualifiedNames(t *testing.T) {
	env := newCLILive(t)
	fix := newShowFixtures(t, env)
	db := env.liveEnv.base.Database

	unqualified, _, code := env.runShowCLI(t, "tab", fix.table)
	if code != 0 {
		t.Fatalf("unqualified exit %d", code)
	}

	// Same object named through a schema qualifier, with --database absent: the
	// name carries the scope, so the flag is not required.
	qualified, _, code := env.runShowNoDatabase(t, "tab", db+"."+fix.table)
	if code != 0 {
		t.Fatalf("qualified exit %d", code)
	}
	if qualified != unqualified {
		t.Errorf("qualified name printed a different definition:\n%s\n%s", qualified, unqualified)
	}

	// Backquoted forms resolve to the same object. The bare quoted name keeps
	// needing --database, because quoting a name does not give it a schema.
	quoted := "`" + db + "`.`" + fix.table + "`"
	out, errOut, code := env.runShowNoDatabase(t, "tab", quoted)
	if code != 0 {
		t.Fatalf("%s: exit %d, stderr:\n%s", quoted, code, errOut)
	}
	if out != unqualified {
		t.Errorf("%s printed a different definition", quoted)
	}

	bare, errOut, code := env.runShowCLI(t, "tab", "`"+fix.table+"`")
	if code != 0 {
		t.Fatalf("quoted name: exit %d, stderr:\n%s", code, errOut)
	}
	if bare != unqualified {
		t.Errorf("quoted name printed a different definition")
	}

	// A quoted name with no schema and no --database is a usage error: the scope
	// cannot be inferred from quoting alone.
	_, errOut, code = env.runShowNoDatabase(t, "tab", "`"+fix.table+"`")
	if code != 2 || !strings.Contains(errOut, "--database is required for show") {
		t.Errorf("exit = %d, want 2, stderr:\n%s", code, errOut)
	}
}

// TestShowCLIForeignSchemaQualifier shows an object outside the connection's own
// database, which is how `list` output feeds a `show` batch.
func TestShowCLIQualifiedBatchFromList(t *testing.T) {
	env := newCLILive(t)
	fix := newShowFixtures(t, env)
	db := env.liveEnv.base.Database

	// list gives the schema and the name in two columns; show takes them joined.
	out, _, code := env.runListCLI(t, "tables", "--pattern", fix.table)
	if code != 0 {
		t.Fatalf("list exit %d", code)
	}
	if !strings.Contains(out, fix.table) {
		t.Fatalf("list result missing the fixture:\n%s", out)
	}

	def, errOut, code := env.runShowNoDatabase(t, "tab", db+"."+fix.table)
	if code != 0 {
		t.Fatalf("show exit %d, stderr:\n%s", code, errOut)
	}
	if !strings.HasPrefix(def, "CREATE TABLE `"+fix.table+"`") {
		t.Errorf("definition:\n%s", def)
	}
}

func TestShowCLIUnknownObjectIsRuntimeError(t *testing.T) {
	env := newCLILive(t)
	gone := fmt.Sprintf("imex_no_such_%d", time.Now().UnixNano())

	// The name parses and the connection works; the server has no such object, so
	// this is a runtime failure (exit 1), not a usage error.
	out, errOut, code := env.runShowCLI(t, "tab", gone)
	if code != 1 {
		t.Fatalf("exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if out != "" {
		t.Errorf("a failed show must print no definition, got:\n%s", out)
	}
	// The server answers SHOW CREATE for a missing table with an error rather than
	// an empty set, so the run fails at the query.
	if !strings.Contains(errOut, "show tab failed") {
		t.Errorf("stderr does not name the command that failed:\n%s", errOut)
	}
	if !strings.Contains(errOut, gone) {
		t.Errorf("stderr does not name the missing object:\n%s", errOut)
	}
	if strings.Contains(errOut, "sql_mode") {
		t.Errorf("the error should not expose the definition columns:\n%s", errOut)
	}
}

func TestShowCLIUsageErrors(t *testing.T) {
	env := newCLILive(t)

	cases := []struct {
		name string
		args []string
		want string
		raw  bool
	}{
		{"no kind", nil, "show requires a kind", true},
		{"unknown kind", []string{"tabel", "x"}, "unknown kind", true},
		{"old plural names", []string{"tables", "x"}, "unknown kind", true},
		{"missing argument", []string{"tab"}, "accepts 1 argument", false},
		{"two arguments", []string{"tab", "a", "b"}, "accepts 1 argument", false},
		{"no database", []string{"tab", "-D", "", "x"}, "--database is required for show", false},
		{"empty qualifier", []string{"tab", "mydb."}, "the part after the schema qualifier is empty", false},
		{"bad log level", []string{"tab", "--log-level", "verbose", "x"}, "invalid --log-level", false},
		{"bad charset", []string{"tab", "--default-charset", "klingon", "x"}, "invalid --default-charset", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr string
			var code int
			if tc.raw {
				stdout, stderr, code = runShowRaw(t, tc.args...)
			} else {
				stdout, stderr, code = env.runShowCLI(t, tc.args...)
			}
			if code != 2 {
				t.Fatalf("exit = %d, want 2\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr does not contain %q:\n%s", tc.want, stderr)
			}
			if strings.Contains(stdout, tc.want) {
				t.Errorf("error text leaked to stdout:\n%s", stdout)
			}
		})
	}
}

// TestShowHelpStaysReadable checks the parent command and its six kinds render
// within the line limit, the same contract the rest of the CLI holds to.
func TestShowHelpStaysReadable(t *testing.T) {
	if os.Getenv("IMEX_TEST_DSN") == "" {
		t.Skip("TestMain builds the CLI binary only for the live suite")
	}
	surfaces := [][]string{{"--help"}}
	for _, short := range []string{"tab", "vws", "trg", "evt", "prc", "fcn"} {
		surfaces = append(surfaces, []string{short, "--help"})
	}
	for _, args := range surfaces {
		cmd := exec.Command(cliBinary, append([]string{"show"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("show %v: %v\n%s", args, err, out)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if len(line) > 118 {
				t.Errorf("show %v: line is %d bytes: %q", args, len(line), line)
			}
		}
	}
}

// TestShowAppearsInTheRootHelp keeps the new command visible where a user looks
// for it, with the three-letter kinds as the registered names.
func TestShowAppearsInTheRootHelp(t *testing.T) {
	if os.Getenv("IMEX_TEST_DSN") == "" {
		t.Skip("TestMain builds the CLI binary only for the live suite")
	}
	out, errOut, code := runIMEXSplit(t, []string{"--help"})
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut)
	}
	text := out + errOut
	if !strings.Contains(text, "show") {
		t.Errorf("root help does not mention show:\n%s", text)
	}

	// Only the leaves swap -h for -?, the same rule `list` follows: the parent has
	// no connection flags, so -h is still cobra's help there.
	parent, _, code := runIMEXSplit(t, []string{"show", "--help"})
	if code != 0 {
		t.Fatalf("show --help exit %d", code)
	}
	for _, short := range []string{"tab", "vws", "trg", "evt", "prc", "fcn"} {
		if !strings.Contains(parent, short) {
			t.Errorf("show help does not list kind %q:\n%s", short, parent)
		}
	}
}
