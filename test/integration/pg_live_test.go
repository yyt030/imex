package integration

// Live PostgreSQL end-to-end cases, driven through the real binary the same way
// option_matrix_test.go drives MySQL. Skipped unless IMEX_TEST_PG_DSN is set:
//
//	IMEX_TEST_PG_DSN='postgres://pgprobe:pass@127.0.0.1:5432/pgprobe?sslmode=disable' \
//	  go test ./test/integration -run PGLive -v
//
// The DSN database must already exist, and the role's search_path must put it in a
// schema it can create tables in: with --engine pg, -D is the connection's
// database and the schema comes from the server (current_schema()), which is the
// two-level-namespace rule internal/backend/postgres.go documents.
//
// What these cases are for is the part a unit test cannot reach: the values a
// real PostgreSQL server hands back over the wire, and whether the file imex
// writes from them reads back as the same rows. The MySQL suite's rows are the
// template, with the MySQL types replaced by the PostgreSQL ones they map to.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/imex/internal/backend"
	"github.com/imex/internal/config"
)

type pgEnv struct {
	db   *sql.DB
	dir  string
	base []string // connection flags shared by every invocation
}

func newPGLive(t *testing.T) *pgEnv {
	t.Helper()

	dsn := os.Getenv("IMEX_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set IMEX_TEST_PG_DSN to run live PostgreSQL integration tests")
	}
	if cliBinary == "" {
		t.Fatal("TestMain did not build the CLI binary; it only does so when a live DSN is set")
	}

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse IMEX_TEST_PG_DSN: %v", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		t.Fatalf("IMEX_TEST_PG_DSN must be a postgres:// URL, got scheme %q", u.Scheme)
	}
	pass, ok := u.User.Password()
	if !ok || pass == "" {
		t.Fatal("IMEX_TEST_PG_DSN must carry a password: these cases pass it on the command line")
	}
	host, portText, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split DSN host %q: %v", u.Host, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("DSN port %q: %v", portText, err)
	}

	// The connection goes through the same backend the command builds, so these
	// cases cover the URL imex writes for itself, not a hand-written one.
	cfg := &config.Config{
		Engine:             string(backend.EnginePG),
		Host:               host,
		Port:               port,
		User:               u.User.Username(),
		Password:           pass,
		Database:           strings.TrimPrefix(u.Path, "/"),
		LogLevel:           config.LogLevelError,
		DefaultCharset:     config.DefaultCharset,
		ConnectTimeout:     config.DefaultConnectTimeout,
		SocketReadTimeout:  config.DefaultSocketTimeout,
		SocketWriteTimeout: config.DefaultSocketTimeout,
		ProgressInterval:   config.DefaultProgressInterval,
	}
	if cfg.Database == "" {
		t.Fatal("IMEX_TEST_PG_DSN must name a database")
	}
	db, err := backend.Must(backend.EnginePG).Open(cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping %s: %v", u.Host, err)
	}

	base := []string{
		"--engine", "pg",
		"-h", host, "-P", portText,
		"-u", cfg.User, "-p", cfg.Password, "-D", cfg.Database,
		"--log-level", "error",
	}
	return &pgEnv{db: db, dir: t.TempDir(), base: base}
}

// pgTable returns a run-unique table name in the connection's current schema and
// registers its removal.
func (e *pgEnv) pgTable(t *testing.T, prefix string) string {
	t.Helper()
	name := fmt.Sprintf("imex_pg_%s_%d", prefix, time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = e.db.Exec("DROP TABLE IF EXISTS " + name + " CASCADE")
	})
	return name
}

func (e *pgEnv) exec(t *testing.T, query string, args ...interface{}) {
	t.Helper()
	if _, err := e.db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// runCLI executes the binary and returns its combined output plus the exit code.
func (e *pgEnv) runCLI(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(cliBinary, args...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return strings.TrimRight(string(out), "\r\n"), code
}

// export runs a successful export of one table and returns the file's path.
func (e *pgEnv) export(t *testing.T, table, format, name string, extra ...string) string {
	t.Helper()
	path := filepath.Join(e.dir, safeFileName(name))
	args := append([]string{"export"}, e.base...)
	args = append(args, "-q", "SELECT * FROM "+table, "-f", format, "-o", path)
	args = append(args, extra...)
	out, code := e.runCLI(t, args...)
	if code != 0 {
		t.Fatalf("export %s as %s: exit %d\n%s", table, format, code, out)
	}
	return path
}

// importFile runs a successful import. --mode insert is the only mode this engine
// serves, and it is the shipped default, so it never needs spelling out.
func (e *pgEnv) importFile(t *testing.T, table, format, path string, extra ...string) {
	t.Helper()
	args := append([]string{"import"}, e.base...)
	args = append(args, "-i", path, "-t", table, "-f", format)
	args = append(args, extra...)
	out, code := e.runCLI(t, args...)
	if code != 0 {
		t.Fatalf("import %s into %s: exit %d\n%s", path, table, code, out)
	}
}

// pgSourceDDL is the PostgreSQL counterpart of the MySQL fixture: the same tricky
// values, in the types a PostgreSQL table actually offers. numeric(10,2) is
// deliberately narrower than MySQL's decimal(20,4) so an out-of-range value is
// impossible rather than silently clipped.
const pgSourceDDL = `CREATE TABLE %s (
	id integer NOT NULL,
	name text,
	num numeric(10,2),
	note text,
	ts timestamptz,
	ts2 timestamp,
	d date,
	flt real,
	dbl double precision,
	flag boolean,
	blob bytea
)`

type pgRow struct {
	id   int
	name any
	num  any
	note any
	ts   any
	ts2  any
	d    any
	flt  any
	dbl  any
	flag any
	blob any
}

// pgRows mirrors liveEnv.fillSource's ten rows. The bytea values are the point of
// two of them: 0x00 is what a PostgreSQL server refuses inside any text value
// (SQLSTATE 22021), which is why this engine always writes binary out as 0x hex.
func (e *pgEnv) fillSource(t *testing.T, table string) {
	t.Helper()

	e.exec(t, fmt.Sprintf(pgSourceDDL, table))
	rows := []pgRow{
		{1, "plain", "1.50", "simple", "2026-01-02 03:04:05+00", "2026-01-02 03:04:05", "2026-01-02", 1.5, 1.5, true, []byte{0x00, 0x01, 'a'}},
		{2, `O'Brien`, "-0.01", "apostrophe", "2025-12-31 23:59:59+00", "2025-12-31 23:59:59", "2025-12-31", -0.5, -0.5, false, []byte("bin")},
		{3, `say "hi"`, "123456.78", "double quote", "2024-02-29 00:00:00+00", "2024-02-29 00:00:00", "2024-02-29", 3.14, 3.14, true, []byte("q")},
		{4, "line1\nline2", "0.00", "embedded\nnewline", nil, nil, nil, nil, nil, nil, nil},
		{5, "tab\tsep", "999999.99", "\tcolumn-like", "1970-01-01 00:00:01+00", "1970-01-01 00:00:01", "1970-01-01", 0, 0, true, []byte("x")},
		{6, "café 中文 😀", "-999999.99", "multi-byte 中文", "2038-01-19 03:14:07+00", "2038-01-19 03:14:07", "2038-01-19", -1.5, -1.5, false, []byte{0x80, 0x81, 0xff}},
		{7, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil},
		{8, strings.Repeat("wide", 20), "42.42", strings.Repeat("n", 500), "2026-06-15 12:00:00+00", "2026-06-15 12:00:00", "2026-06-15", 2, 1e20, true, []byte(strings.Repeat("p", 20))},
		{9, "trailing space ", "3.14", "ends with space ", "2026-06-15 12:00:01+00", "2026-06-15 12:00:01", "2026-06-15", 0.5, 0.5, false, []byte(" ")},
		{10, `back\slash`, "2.72", `path C:\temp`, "2026-06-15 12:00:02+00", "2026-06-15 12:00:02", "2026-06-15", 1, -1e20, true, []byte(`\`)},
		// The three values MySQL has no column for and PostgreSQL does: an empty
		// string against NULL, and both float infinities.
		{11, "", "0.00", "empty string", "infinity", "infinity", "infinity", math.Inf(1), math.Inf(1), true, nil},
		{12, "-", "-0.00", "minus only", "-infinity", "-infinity", "-infinity", math.Inf(-1), math.Inf(-1), false, []byte{0x00}},
	}
	for _, r := range rows {
		e.exec(t, "INSERT INTO "+table+" VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)",
			r.id, r.name, r.num, r.note, r.ts, r.ts2, r.d, r.flt, r.dbl, r.flag, r.blob)
	}
	// Row 4's newline and row 5's tab come from Go string escapes, which are exact;
	// the numeric and date spellings above are what the server stores, so a
	// comparison of two dumps is a comparison of the tool's text, not of the input.
	e.exec(t, "ANALYZE "+table)
}

// TestPGLive_DelRoundTrip is the increment's headline claim: a DEL dump of a real
// PostgreSQL table, imported back into a clone of it, must reproduce every value.
// The check is that a second dump of the imported table is byte-identical to the
// first, so the assertion is on the file the tool writes, not on a reader the
// tool also owns.
func TestPGLive_DelRoundTrip(t *testing.T) {
	env := newPGLive(t)
	src := env.pgTable(t, "del_src")
	dst := env.pgTable(t, "del_dst")
	env.fillSource(t, src)
	env.exec(t, fmt.Sprintf("CREATE TABLE %s (LIKE %s)", dst, src))

	first := env.export(t, src, "del", "del_first.del")
	env.importFile(t, dst, "del", first)
	second := env.export(t, dst, "del", "del_second.del")

	if a, b := readFile(t, first), readFile(t, second); a != b {
		t.Errorf("re-export of the imported table differs (%d vs %d bytes)", len(a), len(b))
		if diff := firstLineDiff(a, b); diff != "" {
			t.Errorf("first differing pair:\n%s", diff)
		}
	}
	if got, want := env.rowCount(t, dst), env.rowCount(t, src); got != want {
		t.Errorf("imported rows = %d, want %d", got, want)
	}
}

// TestPGLive_ParquetRoundTrip runs the same claim through the columnar format,
// where binary columns are BYTE_ARRAY and the logical UTF-8 annotation is the only
// place the character set is recorded.
func TestPGLive_ParquetRoundTrip(t *testing.T) {
	env := newPGLive(t)
	src := env.pgTable(t, "pq_src")
	dst := env.pgTable(t, "pq_dst")
	env.fillSource(t, src)
	env.exec(t, fmt.Sprintf("CREATE TABLE %s (LIKE %s)", dst, src))

	first := env.export(t, src, "parquet", "first.parquet")
	env.importFile(t, dst, "parquet", first)

	if got, want := env.rowCount(t, dst), env.rowCount(t, src); got != want {
		t.Fatalf("imported rows = %d, want %d", got, want)
	}
	// Parquet text cannot be diffed byte for byte (the header carries a created-by
	// string and row-group offsets), so the comparison is a DEL dump of each table.
	a := env.export(t, src, "del", "pq_src.del")
	b := env.export(t, dst, "del", "pq_dst.del")
	if readFile(t, a) != readFile(t, b) {
		t.Errorf("values differ after the parquet hop:\n%s", firstLineDiff(readFile(t, a), readFile(t, b)))
	}
}

// TestPGLive_BinaryIsHexAndSurvives pins the two rules the pg path cannot relax:
// bytea goes out as 0x hex, and it comes back as the same bytes. A raw NUL in the
// text protocol is a server error (SQLSTATE 22021), so there is no engine setting
// that may turn this off.
func TestPGLive_BinaryIsHexAndSurvives(t *testing.T) {
	env := newPGLive(t)
	src := env.pgTable(t, "hex_src")
	dst := env.pgTable(t, "hex_dst")
	env.exec(t, fmt.Sprintf(pgSourceDDL, src))
	env.exec(t, "CREATE TABLE "+dst+" (LIKE "+src+")")

	for _, tc := range []struct {
		id   int
		blob []byte
		hex  string
	}{
		{1, []byte{0x00, 0x01, 0xff}, "0x0001ff"},
		{2, []byte{0x00}, "0x00"},
		{3, []byte("abc"), "0x616263"},
	} {
		env.exec(t, "INSERT INTO "+src+" (id, blob) VALUES ($1, $2)", tc.id, tc.blob)
	}

	file := env.export(t, src, "del", "hex.del")
	text := readFile(t, file)
	for _, tc := range []struct {
		hex string
		id  int
	}{{"0x0001ff", 1}, {"0x00", 2}, {"0x616263", 3}} {
		if !strings.Contains(text, tc.hex) {
			t.Errorf("bytea row %d did not come out as %s\n%s", tc.id, tc.hex, text)
		}
	}

	env.importFile(t, dst, "del", file)
	var got []byte
	if err := env.db.QueryRow("SELECT blob FROM " + dst + " WHERE id = 1").Scan(&got); err != nil {
		t.Fatalf("read back row 1: %v", err)
	}
	if len(got) != 3 || got[0] != 0x00 || got[1] != 0x01 || got[2] != 0xff {
		t.Errorf("row 1 bytea = %x, want 0001ff", got)
	}
	if err := env.db.QueryRow("SELECT blob FROM " + dst + " WHERE id = 2").Scan(&got); err != nil {
		t.Fatalf("read back row 2: %v", err)
	}
	if len(got) != 1 || got[0] != 0x00 {
		t.Errorf("row 2 bytea = %x, want a single 00 byte", got)
	}
}

// TestPGLive_GeneratedColumnIsExcluded proves the INSERT is built from the real
// column list: a generated column cannot be assigned, and the server must be the
// one that computes it on the way back in.
func TestPGLive_GeneratedColumnIsExcluded(t *testing.T) {
	env := newPGLive(t)
	src := env.pgTable(t, "gen_src")
	dst := env.pgTable(t, "gen_dst")
	env.exec(t, fmt.Sprintf(`CREATE TABLE %s (
		id integer,
		name text,
		upper_name text GENERATED ALWAYS AS (upper(name)) STORED,
		num numeric(10,2)
	)`, src))
	env.exec(t, fmt.Sprintf(`CREATE TABLE %s (
		id integer,
		name text,
		upper_name text GENERATED ALWAYS AS (upper(name)) STORED,
		num numeric(10,2)
	)`, dst))
	env.exec(t, "INSERT INTO "+src+" (id, name, num) VALUES (1, 'mixed Case', 1.25), (2, NULL, NULL)")

	file := env.export(t, src, "del", "gen.del")
	env.importFile(t, dst, "del", file)

	var upper sql.NullString
	if err := env.db.QueryRow("SELECT upper_name FROM " + dst + " WHERE id = 1").Scan(&upper); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if upper.String != "MIXED CASE" {
		t.Errorf("generated column = %q, want the server's own MIXED CASE", upper.String)
	}
}

// TestPGLive_ListObjects runs every object `list` supports on this engine against
// the live server. The metadata rows are read through sql.NullString, so this also
// proves the numeric and catalogue columns PostgreSQL hands back survive the scan.
func TestPGLive_ListObjects(t *testing.T) {
	env := newPGLive(t)
	src := env.pgTable(t, "list_src")
	env.exec(t, fmt.Sprintf(pgSourceDDL, src))
	env.exec(t, "INSERT INTO "+src+" (id, name) VALUES (1, 'row')")
	env.exec(t, "CREATE INDEX "+src+"_name ON "+src+" (name)")

	cases := []struct {
		object string
		args   []string
		want   []string
	}{
		{"databases", nil, []string{env.databaseName(t)}},
		{"tables", nil, []string{src}},
		{"columns", []string{"--table", src}, []string{"blob", "bytea"}},
		{"indexes", []string{"--table", src}, []string{src + "_name", "btree"}},
	}
	for _, c := range cases {
		t.Run(c.object, func(t *testing.T) {
			args := append([]string{"list", c.object}, env.base...)
			args = append(args, c.args...)
			out, code := env.runCLI(t, args...)
			if code != 0 {
				t.Fatalf("list %s: exit %d\n%s", c.object, code, out)
			}
			for _, want := range c.want {
				if !strings.Contains(out, want) {
					t.Errorf("list %s output is missing %q:\n%s", c.object, want, out)
				}
			}
		})
	}

	// views, triggers and routines have nothing to report on a fresh table, and an
	// empty result is still a successful run with a header.
	for _, object := range []string{"views", "triggers", "routines"} {
		t.Run(object, func(t *testing.T) {
			args := append([]string{"list", object}, env.base...)
			out, code := env.runCLI(t, args...)
			if code != 0 {
				t.Fatalf("list %s: exit %d\n%s", object, code, out)
			}
		})
	}
}

// listResultRows strips the version banner — it carries a timestamp and a PID, so
// it differs on every run — and returns the list result itself. It refuses a
// result with no catalog rows, because "nothing printed" would compare equal for
// two different statements.
func listResultRows(t *testing.T, out string) string {
	t.Helper()

	var kept []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "imex starting") {
			continue
		}
		kept = append(kept, line)
	}
	body := strings.Join(kept, "\n")
	if !strings.Contains(body, "bytea") {
		t.Fatalf("list columns printed no PostgreSQL catalog rows:\n%s", out)
	}
	return body
}

// TestPGLive_EngineSpellingIsNormalised is the real-server half of the 131
// ruling. It runs the same list command with `--engine PG`, `--engine Pg`,
// `--engine " pg "` and `--engine "pg\t"` and requires the same rows the canonical
// spelling gets. Only the catalog choice ever differed: before the fix the raw
// string decided the statement, so PostgreSQL was asked for information_schema
// rows through MySQL's syntax and the run ended in a server error with exit 1
// (measured against the previous release's binary, see the change record). The
// comparison is the whole result rather than a substring, so a different
// placeholder style or column order cannot pass.
func TestPGLive_EngineSpellingIsNormalised(t *testing.T) {
	env := newPGLive(t)
	src := env.pgTable(t, "spell_src")
	env.exec(t, fmt.Sprintf(pgSourceDDL, src))
	env.exec(t, "INSERT INTO "+src+" (id, name) VALUES (1, 'row')")

	// env.base opens with {"--engine", "pg"} (see newPGLive), so the connection
	// flags after those two elements are what a spelling test reuses verbatim.
	head := []string{"list", "columns", "--table", src}
	want, code := env.runCLI(t, append(append([]string{}, head...), env.base...)...)
	if code != 0 {
		t.Fatalf("list columns --engine pg: exit %d\n%s", code, want)
	}
	rows := listResultRows(t, want)

	for _, spelling := range []string{"PG", "Pg", " pg ", "pg\t"} {
		args := append(append(append([]string{}, head...), "--engine", spelling), env.base[2:]...)
		out, got := env.runCLI(t, args...)
		if got != 0 {
			t.Errorf("list columns --engine %q: exit %d\n%s", spelling, got, out)
			continue
		}
		if diff := listResultRows(t, out); diff != rows {
			t.Errorf("list columns --engine %q rows differ from --engine pg:\n%s\nvs\n%s", spelling, diff, rows)
		}
	}
}

// TestPGLive_EventAndShowRefusalsReachExit2 repeats the two refusals that need a
// resolved engine and a real flag set, so the exit code is read from the process
// rather than from the error value a validator returns.
// TestPGLive_AdminShutdownIsATransientCode proves the SQLSTATE table on the wire:
// a backend terminated by an administrator reaches the retry decision as
// *pgconn.PgError 57P01, which dberr_postgres.go now calls transient. The measured
// type matters — if pgx switched to a bare io.EOF the table would never see it.
func TestPGLive_AdminShutdownIsATransientCode(t *testing.T) {
	e := newPGLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, err := e.db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin a connection: %v", err)
	}
	defer conn.Close()

	var pid int
	if err := conn.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("read backend pid: %v", err)
	}
	if _, err := e.db.ExecContext(ctx, "SELECT pg_terminate_backend($1)", pid); err != nil {
		t.Fatalf("terminate backend %d: %v", pid, err)
	}

	var one int
	err = conn.QueryRowContext(ctx, "SELECT 1").Scan(&one)
	if err == nil {
		t.Fatal("query after pg_terminate_backend succeeded")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("termination surfaced as %T (%v), want *pgconn.PgError", err, err)
	}
	if pgErr.Code != "57P01" {
		t.Fatalf("SQLSTATE = %q, want 57P01 (%v)", pgErr.Code, err)
	}
	transient, decided := backend.Must(backend.EnginePG).TransientError(err)
	if !transient || !decided {
		t.Fatalf("TransientError(%v) = (%v, %v), want (true, true)", err, transient, decided)
	}
}

func TestPGLive_EventAndShowRefusalsReachExit2(t *testing.T) {
	env := newPGLive(t)

	out, code := env.runCLI(t, append([]string{"list", "events"}, env.base...)...)
	if code != 2 || !strings.Contains(out, "list events is not supported with --engine pg") {
		t.Errorf("list events: exit %d, output %q", code, out)
	}

	// Since v0.29.0 PostgreSQL rebuilds table/view/trigger/procedure/function
	// definitions from the catalog. Only `event` is still refused: there is no
	// scheduler object to print. `show tab` must succeed, not exit 2.
	out, code = env.runCLI(t, append([]string{"show", "evt", "no_such_event"}, env.base...)...)
	if code != 2 || !strings.Contains(out, "show evt is not supported with --engine pg") {
		t.Errorf("show evt: exit %d, output %q", code, out)
	}

	src := env.pgTable(t, "show_src")
	env.exec(t, fmt.Sprintf(pgSourceDDL, src))
	out, code = env.runCLI(t, append([]string{"show", "tab", src}, env.base...)...)
	if code != 0 || !strings.Contains(out, "CREATE TABLE") {
		t.Errorf("show tab on pg must be supported: exit %d, output %q", code, out)
	}
}

// TestPGLive_ShowTableDDLIsReplayable pins the catalog reconstruction: a stored
// generated column must not also carry a DEFAULT (PostgreSQL rejects "both
// default and generation expression specified"), and an identity column must
// keep its identity instead of collapsing into a plain integer column.
func TestPGLive_ShowTableDDLIsReplayable(t *testing.T) {
	env := newPGLive(t)
	src := env.pgTable(t, "ddl_src")
	replay := env.pgTable(t, "ddl_replay")
	env.exec(t, fmt.Sprintf(`CREATE TABLE %s (
		id integer PRIMARY KEY,
		a integer,
		b integer GENERATED ALWAYS AS (a * 2) STORED,
		d integer GENERATED ALWAYS AS IDENTITY,
		e text DEFAULT 'x'::text
	)`, src))

	path := filepath.Join(env.dir, "ddl.sql")
	out, code := env.runCLI(t, append([]string{"show", "tab", src, "-o", path}, env.base...)...)
	if code != 0 {
		t.Fatalf("show tab: exit %d\n%s", code, out)
	}
	ddl, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read DDL: %v", err)
	}
	got := string(ddl)
	if strings.Contains(got, "DEFAULT (a * 2)") {
		t.Errorf("generated column must not also carry a DEFAULT:\n%s", got)
	}
	if !strings.Contains(got, "GENERATED ALWAYS AS IDENTITY") {
		t.Errorf("identity column lost its IDENTITY clause:\n%s", got)
	}

	// Replay into a second table. Renaming every occurrence of the source name
	// also renames the constraints, so no index name collides with the original.
	env.exec(t, strings.ReplaceAll(got, src, replay))

	var isIdentity string
	if err := env.db.QueryRow(
		"SELECT is_identity FROM information_schema.columns WHERE table_name = $1 AND column_name = 'd'",
		replay).Scan(&isIdentity); err != nil {
		t.Fatalf("read replayed identity column: %v", err)
	}
	if isIdentity != "YES" {
		t.Errorf("replayed d.is_identity = %q, want YES", isIdentity)
	}
	var isGenerated string
	if err := env.db.QueryRow(
		"SELECT is_generated FROM information_schema.columns WHERE table_name = $1 AND column_name = 'b'",
		replay).Scan(&isGenerated); err != nil {
		t.Fatalf("read replayed generated column: %v", err)
	}
	if isGenerated != "ALWAYS" {
		t.Errorf("replayed b.is_generated = %q, want ALWAYS", isGenerated)
	}
}

func (e *pgEnv) rowCount(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func (e *pgEnv) databaseName(t *testing.T) string {
	t.Helper()
	var name string
	if err := e.db.QueryRow("SELECT current_database()").Scan(&name); err != nil {
		t.Fatalf("current_database: %v", err)
	}
	if name == "" {
		t.Fatal("connection has no current database")
	}
	return name
}

// firstLineDiff reports the first line where two dumps disagree, with the matching
// line from the other side, so a failure reads as a value rather than a byte count.
func firstLineDiff(a, b string) string {
	la := strings.Split(a, "\n")
	lb := strings.Split(b, "\n")
	for i := 0; i < len(la) && i < len(lb); i++ {
		if la[i] != lb[i] {
			return fmt.Sprintf("line %d:\n  -%s\n  +%s", i+1, la[i], lb[i])
		}
	}
	if len(la) != len(lb) {
		return fmt.Sprintf("line counts differ: %d vs %d", len(la), len(lb))
	}
	return ""
}

// ---------------------------------------------------------------------------
// The ported class: --mode replace, --mode insert_update and --threads.
//
// Every case here exists because the option used to be a refusal whose reason was
// "has not been measured". A live bed is the only place the reason can be settled,
// so the assertions are on the three things that matter: the bytes the server
// stores, the counters the run reports, and the exit code of the cases the tool
// deliberately refuses to guess about.
// ---------------------------------------------------------------------------

// pgWideDDL is the type set the parquet hop used to corrupt: every column goes into
// the file as a BYTE_ARRAY, so only the target column's own type says whether those
// bytes are text or raw. uuid and integer[] are the two that fail loudly-but-
// silently — a []byte binds to a uuid as its own bytes and to an integer[] as one
// element per byte, and both insert successfully with the wrong value.
const pgWideDDL = `CREATE TABLE %s (
	k integer PRIMARY KEY,
	bl boolean,
	js json,
	jb jsonb,
	ar integer[],
	m text[],
	tsz timestamptz,
	u uuid,
	xf float4,
	xn numeric(12,4),
	dt date,
	tt time,
	ip inet,
	vb bit varying(16),
	b8 bit(8),
	it interval,
	ba bytea
)`

// fillWideSource writes three rows: one with every column set, one where the
// nullable columns are NULL, and one that exercises the empty-array, zero-uuid and
// empty-JSON spellings a dump can carry. The jsonb values are deliberately written
// in a shape the server rewrites — measured, `{"z":1,"a":[2,3]}` is stored and
// printed as `{"a": [2, 3], "z": 1}`, keys sorted and spaces inserted — so the
// round trip compares against what the catalogue holds rather than against the text
// the run was fed, and a reader that re-serialises instead of copying would show up
// as a difference on the second hop.
func (e *pgEnv) fillWideSource(t *testing.T, table string) {
	t.Helper()

	e.exec(t, fmt.Sprintf(pgWideDDL, table))
	e.exec(t, "INSERT INTO "+table+
		" VALUES (1, true, '{\"a\":1}'::json, '{\"z\":1,\"a\":[2,3]}'::jsonb, ARRAY[1,2,3], ARRAY['x','y']::text[],"+
		" '2026-01-02 03:04:05+00'::timestamptz, '11112222-3333-4444-5555-666677778888'::uuid,"+
		" 1.5, 123456.7890, '2024-02-29', '12:34:56', '10.0.0.1'::inet, B'101', B'10100101'::bit(8),"+
		" '1 day 2:03:04'::interval, '\\x00ff80'::bytea)")
	e.exec(t, "INSERT INTO "+table+
		" VALUES (2, false, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL)")
	e.exec(t, "INSERT INTO "+table+
		" VALUES (3, NULL, '[]'::json, '{\"b\": null}'::jsonb, '{}', '{}', '1970-01-01 00:00:00+00'::timestamptz,"+
		" '00000000-0000-0000-0000-000000000000'::uuid, 0, 0, '2038-01-19', '00:00:00',"+
		" '2001:db8::1'::inet, B'111111', B'11111111'::bit(8), '0 seconds'::interval, '\\x'::bytea)")
	e.exec(t, "ANALYZE "+table)
}

// TestPGLive_ParquetWideTypesRoundTrip is the correctness fix, live: a parquet dump
// of the native type set must read back as the same values. The comparison is a DEL
// dump of each side, because a parquet file itself is not byte-stable.
func TestPGLive_ParquetWideTypesRoundTrip(t *testing.T) {
	env := newPGLive(t)
	src := env.pgTable(t, "wide_src")
	dst := env.pgTable(t, "wide_dst")
	env.fillWideSource(t, src)
	env.exec(t, fmt.Sprintf("CREATE TABLE %s (LIKE %s)", dst, src))

	file := env.export(t, src, "parquet", "wide.parquet")
	env.importFile(t, dst, "parquet", file)

	if got, want := env.rowCount(t, dst), env.rowCount(t, src); got != want {
		t.Fatalf("imported rows = %d, want %d", got, want)
	}
	a := env.export(t, src, "del", "wide_src.del")
	b := env.export(t, dst, "del", "wide_dst.del")
	if readFile(t, a) != readFile(t, b) {
		t.Errorf("values differ after the parquet hop:\n%s", firstLineDiff(readFile(t, a), readFile(t, b)))
	}
	// The two columns that were silently wrong before the fix, named in the
	// failure so a regression says which one broke.
	for _, probe := range []struct{ label, query string }{
		{"integer[]", "SELECT ar::text FROM %s WHERE k = 1"},
		{"uuid", "SELECT u::text FROM %s WHERE k = 1"},
		{"text[]", "SELECT m::text FROM %s WHERE k = 1"},
		{"jsonb", "SELECT jb::text FROM %s WHERE k = 1"},
		{"bit", "SELECT b8::text FROM %s WHERE k = 1"},
		{"varbit", "SELECT vb::text FROM %s WHERE k = 1"},
		{"interval", "SELECT it::text FROM %s WHERE k = 1"},
		{"bytea", "SELECT encode(ba, 'hex') FROM %s WHERE k = 1"},
	} {
		var s, d string
		if err := env.db.QueryRow(fmt.Sprintf(probe.query, src)).Scan(&s); err != nil {
			t.Fatalf("read %s from %s: %v", probe.label, src, err)
		}
		if err := env.db.QueryRow(fmt.Sprintf(probe.query, dst)).Scan(&d); err != nil {
			t.Fatalf("read %s from %s: %v", probe.label, dst, err)
		}
		if s != d {
			t.Errorf("%s came back as %q, source holds %q", probe.label, d, s)
		}
	}
}

// TestPGLive_ReplaceModeIsDeleteOnly proves the two halves of the replace port: the
// table is emptied with the statement the tool has always used, and the MySQL-only
// referential-check toggle is not sent here at all.
func TestPGLive_ReplaceModeIsDeleteOnly(t *testing.T) {
	env := newPGLive(t)
	src := env.pgTable(t, "rep_src")
	dst := env.pgTable(t, "rep_dst")
	env.fillSource(t, src)
	env.exec(t, fmt.Sprintf("CREATE TABLE %s (LIKE %s)", dst, src))
	env.exec(t, "INSERT INTO "+dst+" VALUES (901, 'leftover', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL)")

	file := env.export(t, src, "del", "rep.del")
	out, code := env.runCLI(t, append(append([]string{"import"}, env.base...),
		"-i", file, "-t", dst, "-f", "del", "--mode", "replace", "--log-level", "info")...)
	if code != 0 {
		t.Fatalf("replace import failed: exit %d\n%s", code, out)
	}
	if strings.Contains(out, "FOREIGN_KEY_CHECKS") {
		t.Errorf("a MySQL session setting was sent to PostgreSQL:\n%s", out)
	}
	if !strings.Contains(out, "DELETE FROM") {
		// The clear is logged at info level; a run that silently skipped it would
		// leave row 901 behind, which the count below also catches.
		t.Logf("DELETE statement not visible in the info log (level or wording may have changed):\n%s", out)
	}
	if got := env.rowCount(t, dst); got != env.rowCount(t, src) {
		t.Errorf("replace left %d rows, want the file's %d", got, env.rowCount(t, src))
	}
	var leftover int
	if err := env.db.QueryRow("SELECT COUNT(*) FROM " + dst + " WHERE id = 901").Scan(&leftover); err != nil {
		t.Fatalf("count leftover: %v", err)
	}
	if leftover != 0 {
		t.Error("--mode replace kept a row the file does not contain")
	}
}

// TestPGLive_ReplaceRespectsForeignKey is the difference from MySQL stated as a
// case: this engine has no way to switch referential checks off, so a replace on a
// referenced table fails loudly instead of orphaning rows.
func TestPGLive_ReplaceRespectsForeignKey(t *testing.T) {
	env := newPGLive(t)
	parent := env.pgTable(t, "rep_parent")
	child := env.pgTable(t, "rep_child")
	env.exec(t, "CREATE TABLE "+parent+" (id integer PRIMARY KEY, v text)")
	env.exec(t, "CREATE TABLE "+child+" (id integer PRIMARY KEY, p integer REFERENCES "+parent+"(id))")
	env.exec(t, "INSERT INTO "+parent+" VALUES (1,'p1'),(2,'p2')")
	env.exec(t, "INSERT INTO "+child+" VALUES (10,1)")

	path := filepath.Join(env.dir, "parent.del")
	if err := os.WriteFile(path, []byte("\"id\",\"v\"\n\"3\",\"p3\"\n"), 0o644); err != nil {
		t.Fatalf("write parent.del: %v", err)
	}
	out, code := env.runCLI(t, append(append([]string{"import"}, env.base...),
		"-i", path, "-t", parent, "-f", "del", "--mode", "replace")...)
	if code != 1 {
		t.Fatalf("a replace that breaks a foreign key must exit 1, got %d\n%s", code, out)
	}
	if !strings.Contains(out, "23503") {
		t.Errorf("failure does not name the SQLSTATE PostgreSQL reports:\n%s", out)
	}
	var rows int
	if err := env.db.QueryRow("SELECT COUNT(*) FROM " + parent).Scan(&rows); err != nil {
		t.Fatalf("count parent: %v", err)
	}
	if rows != 2 {
		t.Errorf("the failed replace changed the table: %d rows, want the original 2", rows)
	}
}

// counters reads the three categories out of a run's completion record.
func counters(t *testing.T, out string) (inserted, updated, unchanged int) {
	t.Helper()
	line := firstLineContaining(out, "import completed")
	if line == "" {
		t.Fatalf("no completion record in output:\n%s", out)
	}
	for _, field := range []struct {
		name string
		to   *int
	}{{"inserted=", &inserted}, {"updated=", &updated}, {"unchanged=", &unchanged}} {
		i := strings.Index(line, field.name)
		if i < 0 {
			t.Fatalf("%s missing from %q", field.name, line)
		}
		rest := line[i+len(field.name):]
		end := strings.IndexAny(rest, " \t")
		if end < 0 {
			end = len(rest)
		}
		n, err := strconv.Atoi(rest[:end])
		if err != nil {
			t.Fatalf("parse %s in %q: %v", field.name, line, err)
		}
		*field.to = n
	}
	return inserted, updated, unchanged
}

// TestPGLive_InsertUpdateCounters is the reason the upsert clause is what it is.
// PostgreSQL counts one affected row for a new row and the same one for a rewritten
// row, so an affected-rows total cannot split them, and an unguarded DO UPDATE
// rewrites a row whose values already match. With the target named, the guard in
// place and (xmax = 0) returned per row, the three categories come out the way
// MySQL's 1/2/0 arithmetic reports them.
func TestPGLive_InsertUpdateCounters(t *testing.T) {
	env := newPGLive(t)
	table := env.pgTable(t, "ui_cnt")
	env.exec(t, "CREATE TABLE "+table+" (id integer PRIMARY KEY, v text, w integer)")
	env.exec(t, "INSERT INTO "+table+" VALUES (1,'a',10),(2,'b',20)")

	path := filepath.Join(env.dir, "ui_cnt.del")
	body := "\"id\",\"v\",\"w\"\n\"1\",\"a\",\"10\"\n\"2\",\"B\",\"20\"\n\"3\",\"c\",\"30\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	args := append(append([]string{"import"}, env.base...),
		"-i", path, "-t", table, "-f", "del", "--mode", "insert_update", "--log-level", "info")

	out, code := env.runCLI(t, args...)
	if code != 0 {
		t.Fatalf("insert_update failed: exit %d\n%s", code, out)
	}
	if ins, upd, unc := counters(t, out); ins != 1 || upd != 1 || unc != 1 {
		t.Errorf("first run reported inserted=%d updated=%d unchanged=%d, want 1 1 1 (one new, one rewritten, one identical)",
			ins, upd, unc)
	}

	// Same file again: nothing may change, and the run must say so.
	out, code = env.runCLI(t, args...)
	if code != 0 {
		t.Fatalf("second insert_update failed: exit %d\n%s", code, out)
	}
	if ins, upd, unc := counters(t, out); ins != 0 || upd != 0 || unc != 3 {
		t.Errorf("re-run reported inserted=%d updated=%d unchanged=%d, want 0 0 3", ins, upd, unc)
	}
	var sum int
	if err := env.db.QueryRow("SELECT COALESCE(SUM(w),0) FROM " + table).Scan(&sum); err != nil {
		t.Fatalf("sum: %v", err)
	}
	if sum != 60 {
		t.Errorf("stored values disagree with the file: SUM(w) = %d, want 10+20+30", sum)
	}
}

// TestPGLive_InsertUpdateOnSoleUniqueKey covers the table with no primary key at
// all: one unique key leaves nothing to guess, so the run names it and proceeds.
func TestPGLive_InsertUpdateOnSoleUniqueKey(t *testing.T) {
	env := newPGLive(t)
	table := env.pgTable(t, "ui_uk")
	env.exec(t, "CREATE TABLE "+table+" (a integer UNIQUE, b text)")
	env.exec(t, "INSERT INTO "+table+" VALUES (1,'old')")

	path := filepath.Join(env.dir, "ui_uk.del")
	if err := os.WriteFile(path, []byte("\"a\",\"b\"\n\"1\",\"new\"\n\"2\",\"fresh\"\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	out, code := env.runCLI(t, append(append([]string{"import"}, env.base...),
		"-i", path, "-t", table, "-f", "del", "--mode", "insert_update", "--log-level", "info")...)
	if code != 0 {
		t.Fatalf("insert_update on a sole unique key failed: exit %d\n%s", code, out)
	}
	if ins, upd, unc := counters(t, out); ins != 1 || upd != 1 || unc != 0 {
		t.Errorf("reported inserted=%d updated=%d unchanged=%d, want 1 1 0", ins, upd, unc)
	}
	var b string
	if err := env.db.QueryRow("SELECT b FROM " + table + " WHERE a = 1").Scan(&b); err != nil {
		t.Fatalf("read rewritten row: %v", err)
	}
	if b != "new" {
		t.Errorf("unique-key row still holds %q", b)
	}
}

// TestPGLive_InsertUpdateRefusesAmbiguousKey stops the run before it touches data.
// MySQL's ON DUPLICATE KEY UPDATE matches whichever unique key collides; an
// ON CONFLICT that names one key does not fire on the other, so a table with two
// unique keys and no primary key has no honest answer, and guessing one would
// import different rows than the same file imports on MySQL.
func TestPGLive_InsertUpdateRefusesAmbiguousKey(t *testing.T) {
	env := newPGLive(t)
	table := env.pgTable(t, "ui_two")
	env.exec(t, "CREATE TABLE "+table+" (a integer UNIQUE, b integer UNIQUE, c text)")

	path := filepath.Join(env.dir, "ui_two.del")
	if err := os.WriteFile(path, []byte("\"a\",\"b\"\n\"7\",\"x\"\n\"8\",\"y\"\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	out, code := env.runCLI(t, append(append([]string{"import"}, env.base...),
		"-i", path, "-t", table, "-f", "del", "--mode", "insert_update")...)
	if code != 1 {
		t.Fatalf("an unresolvable conflict target must exit 1, got %d\n%s", code, out)
	}
	if !strings.Contains(out, "has no primary key and 2 unique keys") {
		t.Errorf("refusal does not name the keys it found:\n%s", out)
	}
	if got := env.rowCount(t, table); got != 0 {
		t.Errorf("the refused run wrote %d rows; the decision must come before any data", got)
	}
}

// TestPGLive_InsertUpdateWithoutAnyKeyWarns pins the third key shape: a table with
// neither a primary key nor a unique key has nothing for ON CONFLICT to name, so
// the run says so and writes plain INSERTs — the same answer MySQL's mode gives on
// a keyless table, and the reason the counters then report every row as new.
func TestPGLive_InsertUpdateWithoutAnyKeyWarns(t *testing.T) {
	env := newPGLive(t)
	table := env.pgTable(t, "ui_none")
	env.exec(t, "CREATE TABLE "+table+" (a integer, b text)")

	path := filepath.Join(env.dir, "ui_none.del")
	if err := os.WriteFile(path, []byte("\"a\",\"b\"\n\"1\",\"x\"\n\"2\",\"y\"\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	args := append(append([]string{"import"}, env.base...),
		"-i", path, "-t", table, "-f", "del", "--mode", "insert_update", "--log-level", "info")
	out, code := env.runCLI(t, args...)
	if code != 0 {
		t.Fatalf("keyless insert_update must still import: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "has no PRIMARY or UNIQUE key") {
		t.Errorf("the run did not say that its upsert has no key to name:\n%s", out)
	}
	if ins, _, _ := counters(t, out); ins != 2 {
		t.Errorf("a keyless table reported inserted=%d, want 2 — every row is new when nothing can collide", ins)
	}
	// And a second run really does duplicate, which is what the warning exists for.
	if _, code = env.runCLI(t, args...); code != 0 {
		t.Fatalf("second keyless run failed: exit %d", code)
	}
	if got := env.rowCount(t, table); got != 4 {
		t.Errorf("keyless re-import left %d rows, want 4 — two runs of two rows with no key to merge on", got)
	}
}

// TestPGLive_InsertUpdateDuplicateRowsInOneBatch pins the one place where the
// upsert says no to the FILE instead of to the server. PostgreSQL forbids a single
// statement from writing the same target row twice, so a file that carries one
// conflict key twice inside one batch fails with SQLSTATE 21000 and the whole
// transaction rolls back, while MySQL's ON DUPLICATE KEY UPDATE applies such rows
// in file order and keeps the last. The same command on the same file therefore
// exits 0 on one engine and 1 on the other, and imex neither reorders the rows nor
// drops one to make that go away — deduplicating would decide what the user's data
// ends up being.
//
// Measured here against MariaDB 10.6.23 and PostgreSQL 14.24: the MySQL run stores
// (1,'second') and (2,'other') and reports inserted=2 updated=1 unchanged=0; the pg
// run exits 1 and leaves the table empty. Splitting the file one row per statement
// with --batch-size 1 is the shape both engines agree on — that run also stores
// (1,'second') and (2,'other') and reports the same three counters.
func TestPGLive_InsertUpdateDuplicateRowsInOneBatch(t *testing.T) {
	env := newPGLive(t)
	table := env.pgTable(t, "ui_dup")
	env.exec(t, "CREATE TABLE "+table+" (id integer PRIMARY KEY, v text)")

	path := filepath.Join(env.dir, "ui_dup.del")
	body := "\"id\",\"v\"\n\"1\",\"first\"\n\"1\",\"second\"\n\"2\",\"other\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	out, code := env.runCLI(t, append(append([]string{"import"}, env.base...),
		"-i", path, "-t", table, "-f", "del", "--mode", "insert_update")...)
	if code != 1 {
		t.Fatalf("a file that writes one row twice in a batch must exit 1 on this engine, got %d\n%s", code, out)
	}
	if !strings.Contains(out, "cannot affect row a second time") || !strings.Contains(out, "SQLSTATE 21000") {
		t.Errorf("the failure did not name the server's own reason:\n%s", out)
	}
	if got := env.rowCount(t, table); got != 0 {
		t.Errorf("the refused run stored %d rows, want 0 — the batch fails inside the import transaction, so nothing lands", got)
	}

	// One row per statement removes the collision: the second copy conflicts with
	// the row the first statement wrote, which is an ordinary update.
	out, code = env.runCLI(t, append(append([]string{"import"}, env.base...),
		"-i", path, "-t", table, "-f", "del", "--mode", "insert_update",
		"--batch-size", "1", "--log-level", "info")...)
	if code != 0 {
		t.Fatalf("--batch-size 1 must take the same file: exit %d\n%s", code, out)
	}
	if ins, upd, unc := counters(t, out); ins != 2 || upd != 1 || unc != 0 {
		t.Errorf("reported inserted=%d updated=%d unchanged=%d, want 2 1 0 — the same split the MySQL run of this file reports",
			ins, upd, unc)
	}
	if got := env.rowCount(t, table); got != 2 {
		t.Fatalf("stored %d rows, want 2", got)
	}
	var v string
	if err := env.db.QueryRow("SELECT v FROM " + table + " WHERE id = 1").Scan(&v); err != nil {
		t.Fatalf("read the duplicated key back: %v", err)
	}
	if v != "second" {
		t.Errorf("key 1 holds %q, want \"second\" — the later row in the file wins, as it does on MySQL", v)
	}
}

// TestPGLive_ParallelImportMatchesSingleThread retires the --threads refusal. The
// parallel path shares the reader and the batching with the serial one, so the only
// thing threads may change is how the batches are spread over connections — which
// must not change a byte of what lands.
func TestPGLive_ParallelImportMatchesSingleThread(t *testing.T) {
	env := newPGLive(t)
	src := env.pgTable(t, "thr_src")
	one := env.pgTable(t, "thr_one")
	four := env.pgTable(t, "thr_four")
	env.fillSource(t, src)
	env.exec(t, fmt.Sprintf("CREATE TABLE %s (LIKE %s INCLUDING ALL)", one, src))
	env.exec(t, fmt.Sprintf("CREATE TABLE %s (LIKE %s INCLUDING ALL)", four, src))
	// pgSourceDDL keeps id NOT NULL but unkeyed, which is what the MySQL fixture
	// does too; an upsert needs a key to name, so this case gives the target one.
	env.exec(t, "ALTER TABLE "+four+" ADD PRIMARY KEY (id)")

	file := env.export(t, src, "del", "thr.del")
	env.importFile(t, one, "del", file)
	out, code := env.runCLI(t, append(append([]string{"import"}, env.base...),
		"-i", file, "-t", four, "-f", "del", "--threads", "4", "--log-level", "info")...)
	if code != 0 {
		t.Fatalf("--threads 4 import failed: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "threads=4") {
		t.Errorf("the completion record does not report the thread count:\n%s", out)
	}
	if got, want := env.rowCount(t, four), env.rowCount(t, one); got != want {
		t.Fatalf("parallel import stored %d rows, serial stored %d", got, want)
	}
	// One dump, two tables: the values may not depend on which connection wrote them.
	var differs int
	if err := env.db.QueryRow(fmt.Sprintf(
		"SELECT COUNT(*) FROM (SELECT * FROM %s EXCEPT ALL SELECT * FROM %s) d", one, four)).Scan(&differs); err != nil {
		t.Fatalf("compare tables: %v", err)
	}
	if differs != 0 {
		t.Errorf("%d rows differ between the serial and the parallel import", differs)
	}

	// The same guarantee for the counting path, where each parallel writer runs a
	// RETURNING query instead of reading an affected-rows total.
	out, code = env.runCLI(t, append(append([]string{"import"}, env.base...),
		"-i", file, "-t", four, "-f", "del", "--mode", "insert_update", "--threads", "4", "--log-level", "info")...)
	if code != 0 {
		t.Fatalf("parallel insert_update failed: exit %d\n%s", code, out)
	}
	if ins, upd, unc := counters(t, out); ins != 0 || upd != 0 || unc != env.rowCount(t, one) {
		t.Errorf("parallel re-import of identical rows reported inserted=%d updated=%d unchanged=%d, want 0 0 %d",
			ins, upd, unc, env.rowCount(t, one))
	}
}
