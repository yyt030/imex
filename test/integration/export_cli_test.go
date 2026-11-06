package integration

// These cases drive the real binary against a real database and pin three
// behaviours that changed together:
//
//   1. A -q statement (inline or @file) that names a schema ("SELECT * FROM
//      db.tbl") used to re-attach the connection's own database and fail with
//      Error 1146 (table 'db.db.tbl' doesn't exist). The dump now references the
//      bare table everywhere, and the query's schema only picks the table read.
//   2. SQL export is data-only, and there is no option to put DDL back in:
//      --create and --no-data were removed outright (a plain usage error), and
//      the table definition comes from `imex show tab` instead.
//   3. "-q @file" reads the statement from a file, "@@" escapes a literal leading
//      at-sign, and the statement must be non-empty.
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
// schema bug. A data-only export of a query that spells out db.tbl must succeed,
// and the produced file must reference the bare table only, with the connection's
// database never appearing a second time.
func TestLive_ExportSchemaQualifiedQuery(t *testing.T) {
	env := newCLILive(t)
	src, _ := env.makePair(t, "qual")
	db := env.dbName(t)

	out := filepath.Join(env.dir, "qual.sql")
	text, code := env.runExportCLI(t, "-f", "sql",
		"-q", fmt.Sprintf("SELECT * FROM `%s`.`%s`", db, src), "-o", out)
	if code != 0 {
		t.Fatalf("schema-qualified export must succeed, exit %d\n%s", code, text)
	}
	if strings.Contains(text, "1146") || strings.Contains(text, "doesn't exist") {
		t.Fatalf("re-qualified schema leaked into the run:\n%s", text)
	}

	content := readFile(t, out)
	// The file is data: no DDL can appear, so the schema could never have been
	// re-attached to a CREATE statement either.
	if strings.Contains(content, "CREATE TABLE") {
		t.Errorf("a SQL export must not carry DDL\n%s", firstLines(content, 8))
	}
	// The emitted file never carries the schema prefix: INSERT names the bare
	// table, so the dump loads into whatever database it is pointed at.
	if strings.Contains(content, "`"+db+"`") {
		t.Errorf("dump must not embed the source schema %q\n%s", db, firstLines(content, 8))
	}
	if !strings.Contains(content, "INSERT INTO `"+src+"`") {
		t.Errorf("dump must insert into the bare table `%s`\n%s", src, firstLines(content, 8))
	}

	// The dump has to be usable: re-import it into a fresh target created here.
	tgt := env.liveTable(t, "qual_tgt")
	env.exec(t, fmt.Sprintf("CREATE TABLE `%s` LIKE `%s`", tgt, src))
	env.cliImport(t, tgt, "sql", out)
	assertSameRows(t, env.snapshotTable(t, src), env.snapshotTable(t, tgt))
}

// TestLive_ExportCreateAndNoDataAreUnknown pins the DDL removal at the CLI face.
// The two options that used to opt a dump into or out of DDL are gone, so a
// script that still passes one learns the option is missing instead of quietly
// getting a different file — including the SQL-only scope check that used to
// reject --create for other formats and can no longer see it.
func TestLive_ExportCreateAndNoDataAreUnknown(t *testing.T) {
	env := newCLILive(t)
	src, _ := env.makePair(t, "createlayout")

	for _, tc := range []struct{ format, flag string }{
		{"sql", "--create"},
		{"sql", "--no-data"},
		{"sql", "--no-create"},
		{"del", "--create"},
		{"del", "--no-data"},
		{"parquet", "--create"},
	} {
		name := tc.format + "_" + strings.TrimPrefix(tc.flag, "--")
		out := filepath.Join(env.dir, "gone_"+name+"."+tc.format)
		text, code := env.runExportCLI(t, "-f", tc.format,
			"-q", "SELECT * FROM `"+src+"`", "-o", out, tc.flag)
		if code != 2 {
			t.Errorf("%s with -f %s must exit 2, got %d\n%s", tc.flag, tc.format, code, text)
		}
		if !strings.Contains(text, "unknown flag: "+tc.flag) {
			t.Errorf("%s with -f %s must be rejected as an unknown flag\n%s", tc.flag, tc.format, text)
		}
		// Parsing fails before anything is opened, so no empty output file is
		// left behind for a --no-data-style mistake.
		if _, err := os.Stat(out); err == nil {
			t.Errorf("%s must not create %s", tc.flag, out)
		}
	}

	// What the single remaining product is: INSERT statements from the first
	// line, with no preamble, no DDL and no section comments.
	data := filepath.Join(env.dir, "createlayout.sql")
	if text, code := env.runExportCLI(t, "-f", "sql", "-q", "SELECT * FROM `"+src+"`", "-o", data); code != 0 {
		t.Fatalf("export exit %d\n%s", code, text)
	}
	content := readFile(t, data)
	for _, forbidden := range []string{"CREATE TABLE", "DROP TABLE", "-- Table structure", "-- Data for", "SET NAMES", "40101"} {
		if strings.Contains(content, forbidden) {
			t.Errorf("a SQL export must not contain %q:\n%s", forbidden, firstLines(content, 8))
		}
	}
	if !strings.HasPrefix(content, "INSERT INTO `"+src+"`") {
		t.Errorf("a SQL export must start at the first INSERT:\n%s", firstLines(content, 8))
	}
}

// TestLive_ExportQueryFile covers the "-q @file" input path: the file's
// surrounding whitespace and trailing semicolons are trimmed before the
// statement reaches the parser, the configuration record keeps naming the source
// file, and "@@" escapes a literal leading at-sign.
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
	text, code := env.runExportCLI(t, "-f", "sql", "--log-level", "info", "-q", "@"+qf, "-o", out)
	if code != 0 {
		t.Fatalf("-q @file export must succeed, exit %d\n%s", code, text)
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
			name: "neither_source",
			args: []string{"-f", "del", "-o", filepath.Join(env.dir, "none.del")},
			want: "-q is required",
		},
		{
			name: "empty_query_file",
			args: func() []string {
				e := filepath.Join(env.dir, "empty.sql")
				if err := os.WriteFile(e, []byte("   \n;\n  "), 0o644); err != nil {
					t.Fatalf("write empty query file: %v", err)
				}
				return []string{"-f", "del", "-q", "@" + e, "-o", filepath.Join(env.dir, "empty_out.del")}
			}(),
			want: "contains no SQL statement",
		},
		{
			name: "missing_query_file",
			args: []string{"-f", "del", "-q", "@" + filepath.Join(env.dir, "nope.sql"), "-o", filepath.Join(env.dir, "miss.del")},
			want: "cannot read query file",
		},
		{
			name: "empty_path",
			args: []string{"-f", "del", "-q", "@", "-o", filepath.Join(env.dir, "at.del")},
			want: "missing file path after @",
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

	// "@@" is an inline statement whose first character is literally "@", so it
	// must reach the server as SQL instead of being opened as a file.
	t.Run("escaped_at", func(t *testing.T) {
		text, code := env.runExportCLI(t, "-f", "del", "-q", "@@nonsense", "-o", filepath.Join(env.dir, "esc.del"))
		if code != 1 {
			t.Fatalf("inline @-statement must reach the server and fail as SQL, got %d\n%s", code, text)
		}
		if strings.Contains(text, "cannot read query file") {
			t.Errorf("@@ must not be read as a file\n%s", text)
		}
	})
}

// TestLive_ExportQueryFileParquet confirms "-q @file" is format-independent: it
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
	text, code := env.runExportCLI(t, "-f", "parquet", "-q", "@"+qf, "-o", out)
	if code != 0 {
		t.Fatalf("-q @file with --format parquet must succeed, exit %d\n%s", code, text)
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		t.Fatalf("-q @file parquet produced no usable output (err=%v)", err)
	}
}
