package integration

// End-to-end tests for `imex list`. They drive the compiled binary so flag
// parsing, validation, the query builder, the renderers and the stdout/stderr
// split are all exercised together. Skipped unless IMEX_TEST_DSN is set.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runIMEXSplit executes the binary and separates stdout from stderr.
func runIMEXSplit(t *testing.T, full []string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(cliBinary, full...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %v: %v", full, err)
		}
		code = ee.ExitCode()
	}
	return out.String(), errBuf.String(), code
}

// runListCLI runs `imex list ...` and returns stdout and stderr separately.
// The combined-output helper cannot prove that data went to stdout and logs to
// stderr, which is the central contract of this command.
func (c *cliEnv) runListCLI(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	full := append([]string{"list"}, append(append([]string{}, c.base...), args...)...)
	return runIMEXSplit(t, full)
}

// runListRaw runs `imex list ...` without the shared connection flags. The
// parent `list` command owns no flags, so arguments that fail before dispatch
// (no object, unknown object) must be exercised without them.
func runListRaw(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	return runIMEXSplit(t, append([]string{"list"}, args...))
}

// runListNoDatabase is runListCLI with the -D flag removed.
func (c *cliEnv) runListNoDatabase(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var base []string
	for i := 0; i < len(c.base); i++ {
		if c.base[i] == "-D" {
			i++
			continue
		}
		base = append(base, c.base[i])
	}
	c.base, base = base, c.base
	defer func() { c.base = base }()
	return c.runListCLI(t, args...)
}

func outputLines(out string) []string {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// tableFields splits an aligned table row. It is only used with fixtures whose
// values contain no spaces; the real separator is two or more spaces.
func tableFields(line string) []string {
	return strings.Fields(line)
}

func TestListCLIObjects(t *testing.T) {
	env := newCLILive(t)
	base := fmt.Sprintf("imex_it_list_%d", time.Now().UnixNano())
	db := env.liveEnv.base.Database

	tbl := base + "_t"
	env.exec(t, fmt.Sprintf("CREATE TABLE `%s` (`id` INT NOT NULL PRIMARY KEY, `name` VARCHAR(20) NULL, KEY `idx_name` (`name`)) ENGINE=InnoDB", tbl))
	t.Cleanup(func() { _, _ = env.db.Exec("DROP TABLE IF EXISTS `" + tbl + "`") })

	view := base + "_v"
	env.exec(t, fmt.Sprintf("CREATE VIEW `%s` AS SELECT `id` FROM `%s`", view, tbl))
	t.Cleanup(func() { _, _ = env.db.Exec("DROP VIEW IF EXISTS `" + view + "`") })

	trigger := base + "_trg"
	env.exec(t, fmt.Sprintf("CREATE TRIGGER `%s` BEFORE INSERT ON `%s` FOR EACH ROW SET @imex_list_seen = 1", trigger, tbl))
	t.Cleanup(func() { _, _ = env.db.Exec("DROP TRIGGER IF EXISTS `" + trigger + "`") })

	proc := base + "_p"
	env.exec(t, fmt.Sprintf("CREATE PROCEDURE `%s`() SELECT 1", proc))
	t.Cleanup(func() { _, _ = env.db.Exec("DROP PROCEDURE IF EXISTS `" + proc + "`") })

	fn := base + "_f"
	env.exec(t, fmt.Sprintf("CREATE FUNCTION `%s`() RETURNS INT DETERMINISTIC RETURN 1", fn))
	t.Cleanup(func() { _, _ = env.db.Exec("DROP FUNCTION IF EXISTS `" + fn + "`") })

	event := base + "_e"
	env.exec(t, fmt.Sprintf("CREATE EVENT `%s` ON SCHEDULE EVERY 1 DAY DISABLE DO SELECT 1", event))
	t.Cleanup(func() { _, _ = env.db.Exec("DROP EVENT IF EXISTS `" + event + "`") })

	t.Run("databases", func(t *testing.T) {
		out, errOut, code := env.runListNoDatabase(t, "databases")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, errOut)
		}
		if !strings.Contains(out, db) {
			t.Errorf("database %q missing from output:\n%s", db, out)
		}
		if strings.Contains(out, "imex starting") {
			t.Errorf("log records leaked to stdout:\n%s", out)
		}
	})

	t.Run("tables", func(t *testing.T) {
		out, errOut, code := env.runListCLI(t, "tables")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, errOut)
		}
		if !strings.Contains(out, tbl) {
			t.Errorf("table %q missing:\n%s", tbl, out)
		}
		if strings.Contains(out, view) {
			t.Errorf("view %q must not appear in base tables:\n%s", view, out)
		}

		all, _, code := env.runListCLI(t, "tables", "--type", "all")
		if code != 0 {
			t.Fatalf("--type all exit %d", code)
		}
		if !strings.Contains(all, view) {
			t.Errorf("--type all must include view %q:\n%s", view, all)
		}
	})

	t.Run("views", func(t *testing.T) {
		out, _, code := env.runListCLI(t, "views")
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		if !strings.Contains(out, view) {
			t.Errorf("view %q missing:\n%s", view, out)
		}
	})

	t.Run("columns", func(t *testing.T) {
		out, _, code := env.runListCLI(t, "columns", "-t", tbl)
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		lines := outputLines(out)
		if len(lines) == 0 || !strings.HasPrefix(lines[0], "database") || !strings.Contains(lines[0], "column") {
			t.Fatalf("header = %q", firstLine(lines))
		}
		if len(lines) != 3 {
			t.Fatalf("rows = %d, want 2:\n%s", len(lines)-1, out)
		}
		id := tableFields(lines[1])
		if len(id) < 8 || id[2] != "id" || id[3] != "1" || id[7] != "PRI" {
			t.Errorf("id row = %v", id)
		}
		name := tableFields(lines[2])
		if len(name) < 8 || name[2] != "name" || name[3] != "2" || name[7] != "MUL" {
			t.Errorf("name row = %v", name)
		}

		noHeader, _, code := env.runListCLI(t, "columns", "-t", tbl, "--no-col-names")
		if code != 0 {
			t.Fatalf("--no-col-names exit %d", code)
		}
		if got := len(outputLines(noHeader)); got != 2 {
			t.Errorf("--no-col-names printed %d lines, want 2 (header suppressed):\n%s", got, noHeader)
		}
	})

	t.Run("indexes", func(t *testing.T) {
		out, _, code := env.runListCLI(t, "indexes", "-t", tbl)
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		if !strings.Contains(out, "PRIMARY") || !strings.Contains(out, "idx_name") {
			t.Errorf("indexes missing PRIMARY or idx_name:\n%s", out)
		}
	})

	t.Run("triggers", func(t *testing.T) {
		out, _, code := env.runListCLI(t, "triggers", "-t", tbl)
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		if !strings.Contains(out, trigger) {
			t.Errorf("trigger %q missing:\n%s", trigger, out)
		}
	})

	t.Run("routines", func(t *testing.T) {
		out, _, code := env.runListCLI(t, "routines")
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		if !strings.Contains(out, proc) || !strings.Contains(out, "PROCEDURE") {
			t.Errorf("procedure %q missing:\n%s", proc, out)
		}
		if !strings.Contains(out, fn) || !strings.Contains(out, "FUNCTION") {
			t.Errorf("function %q missing:\n%s", fn, out)
		}

		onlyProc, _, code := env.runListCLI(t, "routines", "--type", "procedure")
		if code != 0 {
			t.Fatalf("--type procedure exit %d", code)
		}
		if strings.Contains(onlyProc, fn) {
			t.Errorf("--type procedure listed function %q:\n%s", fn, onlyProc)
		}
	})

	t.Run("abbreviations", func(t *testing.T) {
		full, _, code := env.runListCLI(t, "databases")
		if code != 0 {
			t.Fatalf("databases exit %d", code)
		}
		short, _, code := env.runListCLI(t, "dbs")
		if code != 0 {
			t.Fatalf("dbs exit %d", code)
		}
		if short != full {
			t.Errorf("dbs output differs from databases:\nshort:\n%s\nfull:\n%s", short, full)
		}

		short, _, code = env.runListCLI(t, "col", "-t", tbl)
		if code != 0 {
			t.Fatalf("col exit %d", code)
		}
		if !strings.Contains(short, tbl) || !strings.Contains(short, "id") {
			t.Errorf("col output missing fixture columns:\n%s", short)
		}

		tab, _, code := env.runListCLI(t, "tab")
		if code != 0 {
			t.Fatalf("tab exit %d", code)
		}
		if !strings.Contains(tab, tbl) {
			t.Errorf("tab output missing fixture table:\n%s", tab)
		}

		trig, _, code := env.runListCLI(t, "trg", "-t", tbl)
		if code != 0 {
			t.Fatalf("trg exit %d", code)
		}
		if !strings.Contains(trig, trigger) {
			t.Errorf("trg output missing trigger:\n%s", trig)
		}
	})

	t.Run("default format", func(t *testing.T) {
		implicit, _, code := env.runListCLI(t, "databases")
		if code != 0 {
			t.Fatalf("default format exit %d", code)
		}
		explicit, _, code := env.runListCLI(t, "databases", "-f", "table")
		if code != 0 {
			t.Fatalf("table exit %d", code)
		}
		if implicit != explicit {
			t.Errorf("default output is not table:\ndefault:\n%s\ntable:\n%s", implicit, explicit)
		}
	})

	t.Run("events", func(t *testing.T) {
		out, _, code := env.runListCLI(t, "events")
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		if !strings.Contains(out, event) {
			t.Errorf("event %q missing:\n%s", event, out)
		}
	})
}

func TestListCLIEmptyAndPatterns(t *testing.T) {
	env := newCLILive(t)
	pattern := fmt.Sprintf("imex_no_such_%d%%", time.Now().UnixNano())

	out, errOut, code := env.runListCLI(t, "tables", "--pattern", pattern)
	if code != 0 {
		t.Fatalf("empty result must exit 0, got %d, stderr:\n%s", code, errOut)
	}
	want := "database  table  type  engine  rows  data_length  index_length  comment"
	if strings.TrimRight(out, "\n") != want {
		t.Errorf("empty result should be the header only, got:\n%s", out)
	}
}

func TestListCLIUsageErrors(t *testing.T) {
	env := newCLILive(t)

	cases := []struct {
		name string
		args []string
		want string
		raw  bool
	}{
		{"missing table", []string{"columns"}, "--table is required for list columns", false},
		{"missing database", []string{"indexes", "-D", ""}, "--database is required for list indexes", false},
		{"unknown object", []string{"tabel"}, "unknown object", true},
		{"old table alias", []string{"tbl"}, "unknown object", true},
		{"no object", nil, "list requires an object", true},
		{"bad format", []string{"tables", "-f", "tsv"}, "invalid --format", false},
		{"bad type", []string{"tables", "--type", "nonsense"}, "invalid --type", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr string
			var code int
			if tc.raw {
				stdout, stderr, code = runListRaw(t, tc.args...)
			} else {
				stdout, stderr, code = env.runListCLI(t, tc.args...)
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

func TestListCLIMessagesRedirectsLogs(t *testing.T) {
	env := newCLILive(t)
	path := filepath.Join(env.dir, "list-messages.log")

	out, errOut, code := env.runListNoDatabase(t, "databases", "--messages", path)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut)
	}
	if errOut != "" {
		t.Errorf("--messages must leave stderr empty, got:\n%s", errOut)
	}
	if !strings.Contains(out, "information_schema") {
		t.Errorf("stdout missing data:\n%s", out)
	}

	logText, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(logText), "imex starting") {
		t.Errorf("log file missing the version banner:\n%s", logText)
	}
}

func firstLine(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return lines[0]
}
