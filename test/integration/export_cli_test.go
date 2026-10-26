package integration

// These cases drive the real binary against a real database and pin three
// behaviours that changed together:
//
//   1. A --query/--query-file statement that names a schema ("SELECT * FROM
//      db.tbl") used to re-attach the connection's own database and fail with
//      Error 1146 (table 'db.db.tbl' doesn't exist). The dump now references the
//      bare table everywhere, while SHOW CREATE is looked up in the schema the
//      query named.
//   2. SQL export is data-only by default; --create brings the CREATE TABLE back,
//      --no-create no longer exists (a plain usage error), and --create on a
//      non-sql format is a scope reject (exit 2).
//   3. --query-file reads the statement from a file, is mutually exclusive with
//      -q, and requires a non-empty statement from exactly one source.
//
// They share the cliEnv of option_matrix_test.go (same package): one binary built
// by TestMain, connection flags from IMEX_TEST_DSN, gated like every other
// live case. The fixtures come from makePair, whose table is the standard
// multi-column shape the snapshot helpers read, so a round trip is comparable.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dbName reports the schema the DSN connection selects, so a test can build a
// genuinely schema-qualified query against a table it just created there.
func (c *cliEnv) dbName(t *testing.T) string {
	t.Helper()
	var name string
	if err := c.db.QueryRow("SELECT DATABASE()").Scan(&name); err != nil {
		t.Fatalf("SELECT DATABASE(): %v", err)
	}
	if name == "" {
		t.Fatal("connection has no current database")
	}
	return name
}

// TestLive_ExportSchemaQualifiedQuery is the regression for the re-qualified
// schema bug. Both the data-only default and --create must succeed on a query
// that spells out db.tbl, and the produced file must reference the bare table
// only, with the connection's database never appearing a second time.
func TestLive_ExportSchemaQualifiedQuery(t *testing.T) {
	env := newCLILive(t)
	src, _ := env.makePair(t, "qual")
	db := env.dbName(t)

	for _, tc := range []struct {
		name       string
		flags      []string
		wantCreate bool
	}{
		{"data_only_default", nil, false},
		{"with_create", []string{"--create"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(env.dir, "qual_"+tc.name+".sql")
			args := append([]string{"-f", "sql", "-q", fmt.Sprintf("SELECT * FROM `%s`.`%s`", db, src), "-o", out}, tc.flags...)
			text, code := env.runExportCLI(t, args...)
			if code != 0 {
				t.Fatalf("schema-qualified export must succeed, exit %d\n%s", code, text)
			}
			if strings.Contains(text, "1146") || strings.Contains(text, "doesn't exist") {
				t.Fatalf("re-qualified schema leaked into the run:\n%s", text)
			}

			content := readFile(t, out)
			if got := strings.Contains(content, "CREATE TABLE"); got != tc.wantCreate {
				t.Errorf("CREATE TABLE present=%v, want %v\n%s", got, tc.wantCreate, firstLines(content, 8))
			}
			// The emitted file never carries the schema prefix: INSERT/LOCK/CREATE
			// all name the bare table, so the dump loads into whatever database it
			// is pointed at and SHOW CREATE could not double-qualify it.
			if strings.Contains(content, "`"+db+"`") {
				t.Errorf("dump must not embed the source schema %q\n%s", db, firstLines(content, 8))
			}
			if !strings.Contains(content, "INSERT INTO `"+src+"`") {
				t.Errorf("dump must insert into the bare table `%s`\n%s", src, firstLines(content, 8))
			}

			// The dump has to be usable: re-import it into a fresh target created
			// for this subtest, so the two variants never share a populated table.
			tgt := env.liveTable(t, "qual_tgt")
			env.exec(t, fmt.Sprintf("CREATE TABLE `%s` LIKE `%s`", tgt, src))
			env.cliImport(t, tgt, "sql", out)
			assertSameRows(t, env.snapshotTable(t, src), env.snapshotTable(t, tgt))
		})
	}
}

// TestLive_ExportCreateDefault pins the flipped default: a plain SQL export is
// data-only, --create opts the structure back in, --no-create is gone (unknown
// flag, exit 2), and --create is refused for formats that do not write CREATE
// TABLE.
func TestLive_ExportCreateDefault(t *testing.T) {
	env := newCLILive(t)
	src, _ := env.makePair(t, "createlayout")

	hasCreate := func(name string, flags ...string) bool {
		t.Helper()
		out := filepath.Join(env.dir, "cd_"+name+".sql")
		args := append([]string{"-f", "sql", "-q", "SELECT * FROM `" + src + "`", "-o", out}, flags...)
		if text, code := env.runExportCLI(t, args...); code != 0 {
			t.Fatalf("export exit %d\n%s", code, text)
		}
		return strings.Contains(readFile(t, out), "CREATE TABLE")
	}

	if hasCreate("default") {
		t.Error("default SQL export must be data-only")
	}
	if !hasCreate("create", "--create") {
		t.Error("--create must emit CREATE TABLE")
	}

	// --no-create was the negation of a default that no longer exists. Dropping it
	// has to be a usage error rather than a silently ignored flag, so a script that
	// still passes it learns the option is gone instead of getting a --create file.
	goneOut := filepath.Join(env.dir, "cd_gone.sql")
	if text, code := env.runExportCLI(t, "-f", "sql", "-q", "SELECT * FROM `"+src+"`", "-o", goneOut, "--no-create"); code != 2 ||
		!strings.Contains(text, `unknown flag: --no-create`) {
		t.Errorf("--no-create must be rejected as an unknown flag with exit 2\n%s", text)
	}

	// --create describes a SQL-only layout choice, so the DEL writer rejects it
	// rather than silently ignoring it.
	out := filepath.Join(env.dir, "cd_del_scope.del")
	text, code := env.runExportCLI(t, "-f", "del", "--create", "-q", "SELECT * FROM `"+src+"`", "-o", out)
	if code != 2 || !strings.Contains(text, "--create can only be used with --format sql") {
		t.Errorf("--create with --format del must exit 2 with the scope error, got code %d\n%s", code, text)
	}
}

// TestLive_ExportQueryFile covers the --query-file input path and its exclusivity
// with -q. The file's surrounding whitespace and trailing semicolons are trimmed
// before the statement reaches the parser, and the configuration record names the
// source file.
func TestLive_ExportQueryFile(t *testing.T) {
	env := newCLILive(t)
	src, dst := env.makePair(t, "qfile")

	// A query file written the way a SQL client file would be: indentation, a
	// trailing semicolon, a blank line.
	qf := filepath.Join(env.dir, "query.sql")
	if err := os.WriteFile(qf, []byte("\n  SELECT * FROM `"+src+"` ORDER BY id;\n\n"), 0o644); err != nil {
		t.Fatalf("write query file: %v", err)
	}

	// --log-level info is appended after the base flags so the config record is
	// visible; the base already forces --log-level error, which would drop it.
	out := filepath.Join(env.dir, "qfile.sql")
	text, code := env.runExportCLI(t, "-f", "sql", "--log-level", "info", "--query-file", qf, "-o", out)
	if code != 0 {
		t.Fatalf("--query-file export must succeed, exit %d\n%s", code, text)
	}
	if !strings.Contains(text, "query_file="+qf) {
		t.Errorf("config record must name the query source file\n%s", text)
	}
	env.cliImport(t, dst, "sql", out)
	assertSameRows(t, env.snapshotTable(t, src), env.snapshotTable(t, dst))

	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "mutually_exclusive",
			args: []string{"-f", "del", "-q", "SELECT 1", "--query-file", qf, "-o", filepath.Join(env.dir, "both.del")},
			want: "--query and --query-file are mutually exclusive",
		},
		{
			name: "neither_source",
			args: []string{"-f", "del", "-o", filepath.Join(env.dir, "none.del")},
			want: "--query or --query-file is required",
		},
		{
			name: "empty_query_file",
			args: func() []string {
				e := filepath.Join(env.dir, "empty.sql")
				if err := os.WriteFile(e, []byte("   \n;\n  "), 0o644); err != nil {
					t.Fatalf("write empty query file: %v", err)
				}
				return []string{"-f", "del", "--query-file", e, "-o", filepath.Join(env.dir, "empty_out.del")}
			}(),
			want: "contains no SQL statement",
		},
		{
			name: "missing_query_file",
			args: []string{"-f", "del", "--query-file", filepath.Join(env.dir, "nope.sql"), "-o", filepath.Join(env.dir, "miss.del")},
			want: "cannot read --query-file",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, code := env.runExportCLI(t, tc.args...)
			if code != 2 {
				t.Errorf("usage error must exit 2, got %d\n%s", code, text)
			}
			if !strings.Contains(text, tc.want) {
				t.Errorf("output must mention %q\n%s", tc.want, text)
			}
		})
	}
}

// TestLive_ExportQueryFileParquet confirms --query-file is format-independent: it
// is an input source, not a layout option, so parquet accepts it too and produces
// a real file.
func TestLive_ExportQueryFileParquet(t *testing.T) {
	env := newCLILive(t)
	src, _ := env.makePair(t, "qfilepq")

	qf := filepath.Join(env.dir, "pq_query.sql")
	if err := os.WriteFile(qf, []byte("SELECT * FROM `"+src+"`;\n"), 0o644); err != nil {
		t.Fatalf("write query file: %v", err)
	}
	out := filepath.Join(env.dir, "pq.parquet")
	text, code := env.runExportCLI(t, "-f", "parquet", "--query-file", qf, "-o", out)
	if code != 0 {
		t.Fatalf("--query-file with --format parquet must succeed, exit %d\n%s", code, text)
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		t.Fatalf("--query-file parquet produced no usable output (err=%v)", err)
	}
}
