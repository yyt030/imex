package integration

// Live MySQL end-to-end tests. They are skipped unless GOMYDUMP_TEST_DSN is
// set, so `go test ./...` stays hermetic while still compiling this file:
//
//	GOMYDUMP_TEST_DSN='root:pass@tcp(127.0.0.1:3306)/gomydump_it' go test ./test/integration -run Live -v
//
// The DSN database must already exist. Every test creates uniquely named
// tables and drops them on cleanup, so runs can share one schema.

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/gomydump/internal/common"
	"github.com/gomydump/internal/config"
	"github.com/gomydump/internal/export"
	_import "github.com/gomydump/internal/import"
)

type liveEnv struct {
	db   *sql.DB
	base config.Config
}

func newLiveEnv(t *testing.T) *liveEnv {
	t.Helper()

	dsn := os.Getenv("GOMYDUMP_TEST_DSN")
	if dsn == "" {
		t.Skip("set GOMYDUMP_TEST_DSN to run live MySQL integration tests")
	}

	mc, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse GOMYDUMP_TEST_DSN: %v", err)
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping %s: %v", mc.Addr, err)
	}

	base := config.Config{
		User:               mc.User,
		Password:           mc.Passwd,
		Database:           mc.DBName,
		LogLevel:           config.LogLevelError,
		DefaultCharset:     config.DefaultCharset,
		ConnectTimeout:     config.DefaultConnectTimeout,
		SocketReadTimeout:  config.DefaultSocketTimeout,
		SocketWriteTimeout: config.DefaultSocketTimeout,
	}
	switch mc.Net {
	case "unix":
		base.Socket = mc.Addr
	default:
		host, port, err := net.SplitHostPort(mc.Addr)
		if err != nil {
			t.Fatalf("split DSN address %q: %v", mc.Addr, err)
		}
		p, err := strconv.Atoi(port)
		if err != nil {
			t.Fatalf("parse DSN port %q: %v", port, err)
		}
		base.Host, base.Port = host, p
	}

	common.Quiet = true
	common.InitLogger(config.LogLevelError)
	return &liveEnv{db: db, base: base}
}

// liveTable returns a database-unique table name and registers its removal.
func (e *liveEnv) liveTable(t *testing.T, prefix string) string {
	t.Helper()
	name := fmt.Sprintf("gmydump_it_%s_%d", prefix, time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = e.db.Exec("DROP TABLE IF EXISTS `" + name + "`")
	})
	return name
}

func (e *liveEnv) exec(t *testing.T, query string, args ...interface{}) {
	t.Helper()
	if _, err := e.db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func (e *liveEnv) export(t *testing.T, opts exportOptions) {
	t.Helper()

	cfg := config.NewExportConfig()
	cfg.Config = e.base
	cfg.Output = opts.output
	cfg.Format = opts.format
	cfg.Query = "SELECT * FROM `" + opts.table + "`"
	cfg.Threads = opts.threads
	cfg.Messages = opts.messages

	exp, err := export.NewExporter(cfg)
	if err != nil {
		t.Fatalf("new exporter: %v", err)
	}
	defer exp.Close()
	if err := exp.Export(context.Background()); err != nil {
		t.Fatalf("export %s as %s: %v", opts.table, opts.format, err)
	}
}

type exportOptions struct {
	table    string
	format   string
	output   string
	messages string
	threads  int
}

func (e *liveEnv) importFile(t *testing.T, opts importOptions) {
	t.Helper()
	e.importFileStats(t, opts)
}

// importFileStats runs an import and returns its counter snapshot, so a test
// can assert the inserted/updated/unchanged split instead of only the rows.
func (e *liveEnv) importFileStats(t *testing.T, opts importOptions) common.StatsSnapshot {
	t.Helper()

	cfg := config.NewImportConfig()
	cfg.Config = e.base
	cfg.Input = opts.input
	cfg.Format = opts.format
	cfg.Mode = opts.mode
	cfg.Table = opts.table
	cfg.Columns = opts.columns
	cfg.Messages = opts.messages
	if opts.threads > 0 {
		cfg.ImportThreads = opts.threads
	}
	cfg.CommitCount = opts.commitCount
	if opts.batchSize > 0 {
		cfg.BatchSize = opts.batchSize
	}

	imp, err := _import.NewImporter(cfg)
	if err != nil {
		t.Fatalf("new importer: %v", err)
	}
	defer imp.Close()
	if err := imp.Import(context.Background()); err != nil {
		t.Fatalf("import %s into %s: %v", opts.input, opts.table, err)
	}
	return imp.Stats()
}

type importOptions struct {
	input       string
	format      string
	mode        config.ImportMode
	table       string
	columns     string
	messages    string
	threads     int
	commitCount int
	batchSize   int
}

// snapshotTable renders every row in a type-stable, NULL-aware text form. HEX
// and DATE_FORMAT avoid driver/session formatting differences, so two tables
// holding the same values compare byte-for-byte.
func (e *liveEnv) snapshotTable(t *testing.T, table string) []string {
	t.Helper()

	query := "SELECT id, HEX(CAST(name AS BINARY)), amount, HEX(CAST(note AS BINARY)), " +
		"DATE_FORMAT(created, '%Y-%m-%d %H:%i:%s'), HEX(payload), flag " +
		"FROM `" + table + "` ORDER BY id"
	rows, err := e.db.Query(query)
	if err != nil {
		t.Fatalf("snapshot %s: %v", table, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		vals := make([]sql.NullString, 7)
		ptrs := make([]interface{}, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			if !v.Valid {
				parts[i] = "<NULL>"
				continue
			}
			parts[i] = v.String
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s: %v", table, err)
	}
	return out
}

const liveSourceDDL = "CREATE TABLE `%s` (" +
	"id BIGINT NOT NULL PRIMARY KEY, " +
	"name VARCHAR(100), " +
	"amount DECIMAL(18,4), " +
	"note TEXT, " +
	"created DATETIME, " +
	"payload VARBINARY(32), " +
	"flag TINYINT) DEFAULT CHARSET=utf8mb4"

func (e *liveEnv) fillSource(t *testing.T, table string) {
	t.Helper()

	e.exec(t, fmt.Sprintf(liveSourceDDL, table))
	rows := []struct {
		id      int64
		name    interface{}
		amount  interface{}
		note    interface{}
		created interface{}
		payload interface{}
		flag    interface{}
	}{
		{1, "plain", "1.5000", "simple", "2026-01-02 03:04:05", []byte{0x00, 0x01, 'a'}, 1},
		{2, `O'Brien`, "-0.0001", "apostrophe", "2025-12-31 23:59:59", []byte("bin"), 0},
		{3, `say "hi"`, "12345678901234.5678", "double quote", "2024-02-29 00:00:00", []byte("q"), -1},
		{4, "line1\nline2", "0.0000", "embedded\nnewline", "2000-01-01 00:00:00", nil, nil},
		{5, "tab\tsep", "999999999999.9999", "\tcolumn-like", "1970-01-01 00:00:01", []byte("x"), 127},
		{6, "café 中文 😀", "-999999999999.9999", "multi-byte 中文", "2038-01-19 03:14:07", []byte{0x80, 0x81}, -128},
		{7, nil, nil, nil, nil, nil, nil},
		{8, strings.Repeat("wide", 20), "42.4242", strings.Repeat("n", 500), "2026-06-15 12:00:00", []byte(strings.Repeat("p", 20)), 42},
		{9, "trailing space ", "3.1416", "ends with space ", "2026-06-15 12:00:01", []byte(" "), 0},
		{10, `back\slash`, "2.7183", `path C:\temp`, "2026-06-15 12:00:02", []byte(`\`), 1},
	}
	for _, r := range rows {
		e.exec(t, "INSERT INTO `"+table+"` VALUES (?,?,?,?,?,?,?)",
			r.id, r.name, r.amount, r.note, r.created, r.payload, r.flag)
	}
}

func assertSameRows(t *testing.T, want, got []string) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("row count = %d, want %d\nwant: %v\ngot:  %v", len(got), len(want), want, got)
	}
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("row %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestLive_RoundTripEveryFormat exports the same table as DEL, SQL and Parquet
// and imports each file into a fresh copy, checking values survive exactly.
func TestLive_RoundTripEveryFormat(t *testing.T) {
	env := newLiveEnv(t)
	dir := t.TempDir()

	for _, format := range []string{"del", "sql", "parquet"} {
		t.Run(format, func(t *testing.T) {
			src := env.liveTable(t, "src_"+format)
			dst := env.liveTable(t, "dst_"+format)
			env.fillSource(t, src)
			env.exec(t, fmt.Sprintf("CREATE TABLE `%s` LIKE `%s`", dst, src))

			file := filepath.Join(dir, "roundtrip."+format)
			exportMsg := filepath.Join(dir, "export-"+format+".msg")
			importMsg := filepath.Join(dir, "import-"+format+".msg")

			env.export(t, exportOptions{table: src, format: format, output: file, messages: exportMsg, threads: 1})
			env.importFile(t, importOptions{
				input: file, format: format, mode: config.ImportModeInsert, table: dst,
				messages: importMsg, threads: 1, commitCount: 3, batchSize: 2,
			})

			assertSameRows(t, env.snapshotTable(t, src), env.snapshotTable(t, dst))
			assertMessageFile(t, exportMsg, "# Export Messages", "SQL3104N", "SQL3110N")
			assertMessageFile(t, importMsg, "# Import Messages", "SQL3109N", "SQL3110N")
		})
	}
}

// TestLive_ParallelExportImport exercises sharded export and concurrent
// import workers, including the streamed shard merge and float-exact boundaries.
func TestLive_ParallelExportImport(t *testing.T) {
	env := newLiveEnv(t)
	dir := t.TempDir()

	src := env.liveTable(t, "psrc")
	dst := env.liveTable(t, "pdst")
	env.exec(t, fmt.Sprintf(liveSourceDDL, src))
	env.exec(t, fmt.Sprintf("CREATE TABLE `%s` LIKE `%s`", dst, src))

	const total = 200
	for i := 0; i < total; i++ {
		env.exec(t, "INSERT INTO `"+src+"` VALUES (?,?,?,?,?,?,?)",
			i+1, fmt.Sprintf("name-%03d", i), fmt.Sprintf("%d.%04d", i*1000, i),
			strings.Repeat("n", i%40), "2026-01-01 00:00:00", []byte{byte(i), byte(i >> 8)}, i%3)
	}

	file := filepath.Join(dir, "parallel.del")
	env.export(t, exportOptions{table: src, format: "del", output: file, threads: 4})

	// Parallel export streams shard output into the final file, so nothing else
	// may appear next to it: the previous design wrote one temporary file per
	// shard and concatenated them, which doubled the bytes hitting the disk and
	// left the fragments behind whenever a run was interrupted.
	if entries, err := os.ReadDir(dir); err != nil {
		t.Fatalf("read output dir: %v", err)
	} else {
		for _, ent := range entries {
			if ent.Name() != "parallel.del" {
				t.Errorf("unexpected file next to the output: %s", ent.Name())
			}
		}
	}

	env.importFile(t, importOptions{
		input: file, format: "del", mode: config.ImportModeInsert, table: dst,
		threads: 4, commitCount: 17, batchSize: 5,
	})

	assertSameRows(t, env.snapshotTable(t, src), env.snapshotTable(t, dst))
}

// TestLive_ImportModes checks insert, insert_update and replace semantics on a
// table with a primary key, then creates a table from an SQL export.
func TestLive_ImportModes(t *testing.T) {
	env := newLiveEnv(t)
	dir := t.TempDir()

	target := env.liveTable(t, "modes")
	env.exec(t, "CREATE TABLE `"+target+"` (id INT NOT NULL PRIMARY KEY, v VARCHAR(20)) DEFAULT CHARSET=utf8mb4")

	writeDel := func(name string, rows ...[2]string) string {
		path := filepath.Join(dir, name)
		var b strings.Builder
		b.WriteString("\"id\",\"v\"\n")
		for _, r := range rows {
			b.WriteString(r[0])
			b.WriteString(",\"")
			b.WriteString(r[1])
			b.WriteString("\"\n")
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return path
	}
	values := func() map[string]string {
		rows, err := env.db.Query("SELECT id, v FROM `" + target + "`")
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var id int
			var v string
			if err := rows.Scan(&id, &v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out[strconv.Itoa(id)] = v
		}
		return out
	}

	env.importFile(t, importOptions{
		input:  writeDel("insert.del", [2]string{"1", "a"}, [2]string{"2", "b"}),
		format: "del", mode: config.ImportModeInsert, table: target, commitCount: 10,
	})
	if got := values(); len(got) != 2 || got["1"] != "a" || got["2"] != "b" {
		t.Fatalf("insert mode: got %v", got)
	}

	env.importFile(t, importOptions{
		input:  writeDel("upsert.del", [2]string{"1", "updated"}, [2]string{"3", "c"}),
		format: "del", mode: config.ImportModeInsertUpdate, table: target, commitCount: 10,
	})
	if got := values(); len(got) != 3 || got["1"] != "updated" || got["2"] != "b" || got["3"] != "c" {
		t.Fatalf("insert_update mode: got %v", got)
	}

	env.importFile(t, importOptions{
		input:  writeDel("replace.del", [2]string{"2", "replaced"}),
		format: "del", mode: config.ImportModeReplace, table: target, commitCount: 10,
	})
	// Replace mode empties the table first, so only the file's rows survive.
	if got := values(); len(got) != 1 || got["2"] != "replaced" {
		t.Fatalf("replace mode: got %v", got)
	}

	// Create mode: export a table as SQL and let the importer create and fill a
	// differently named table from the same file.
	src := env.liveTable(t, "createsrc")
	created := env.liveTable(t, "created")
	env.fillSource(t, src)
	sqlFile := filepath.Join(dir, "create.sql")
	env.export(t, exportOptions{table: src, format: "sql", output: sqlFile, threads: 1})
	env.importFile(t, importOptions{
		input: sqlFile, format: "sql", mode: config.ImportModeCreate, table: created, commitCount: 4,
	})
	assertSameRows(t, env.snapshotTable(t, src), env.snapshotTable(t, created))
}

// TestLive_ColumnMapping maps file columns onto differently named table
// columns.
func TestLive_ColumnMapping(t *testing.T) {
	env := newLiveEnv(t)
	dir := t.TempDir()

	target := env.liveTable(t, "mapped")
	env.exec(t, "CREATE TABLE `"+target+"` (target_id INT NOT NULL PRIMARY KEY, target_name VARCHAR(20))")

	file := filepath.Join(dir, "mapped.del")
	if err := os.WriteFile(file, []byte("\"id\",\"name\"\n1,\"one\"\n2,\"two\"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	env.importFile(t, importOptions{
		input: file, format: "del", mode: config.ImportModeInsert, table: target,
		columns: "id=target_id,name=target_name", commitCount: 10,
	})

	rows, err := env.db.Query("SELECT target_id, target_name FROM `" + target + "` ORDER BY target_id")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id int
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, fmt.Sprintf("%d=%s", id, name))
	}
	assertSameRows(t, []string{"1=one", "2=two"}, got)
}

func assertMessageFile(t *testing.T, path string, wants ...string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read message file %s: %v", path, err)
	}
	text := string(data)
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("message file %s missing %q:\n%s", path, want, text)
		}
	}
}

// TestLive_ImportCounters asserts the inserted/updated/unchanged split against
// a real server. Re-importing identical values used to be reported as
// inserted=N/updated=0 because the derivation assumed every row contributes at
// least one affected row, while ON DUPLICATE KEY UPDATE reports 0 for a row set
// to its current values.
func TestLive_ImportCounters(t *testing.T) {
	env := newLiveEnv(t)
	dir := t.TempDir()

	target := env.liveTable(t, "counters")
	env.exec(t, "CREATE TABLE `"+target+"` (id INT NOT NULL PRIMARY KEY, v VARCHAR(20)) DEFAULT CHARSET=utf8mb4")

	path := filepath.Join(dir, "counters.del")
	var b strings.Builder
	b.WriteString("\"id\",\"v\"\n")
	for _, r := range [][2]string{{"1", "a"}, {"2", "b"}, {"3", "c"}, {"4", "d"}} {
		b.WriteString(r[0] + ",\"" + r[1] + "\"\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	type counters struct{ inserted, updated, unchanged int64 }
	check := func(label string, got common.StatsSnapshot, want counters) {
		t.Helper()
		if got.Inserted != want.inserted || got.Updated != want.updated || got.Unchanged != want.unchanged {
			t.Errorf("%s: inserted=%d updated=%d unchanged=%d, want %d/%d/%d",
				label, got.Inserted, got.Updated, got.Unchanged, want.inserted, want.updated, want.unchanged)
		}
		if got.Processed != 4 {
			t.Errorf("%s: read=%d, want 4 rows", label, got.Processed)
		}
		if sum := got.Inserted + got.Updated + got.Unchanged; sum != got.Processed {
			t.Errorf("%s: inserted+updated+unchanged=%d, want read=%d", label, sum, got.Processed)
		}
	}
	run := func(mode config.ImportMode) common.StatsSnapshot {
		return env.importFileStats(t, importOptions{
			input: path, format: "del", mode: mode, table: target, commitCount: 10,
		})
	}

	check("insert into empty table", run(config.ImportModeInsert), counters{4, 0, 0})
	check("insert_update with identical values", run(config.ImportModeInsertUpdate), counters{0, 0, 4})

	env.exec(t, "UPDATE `"+target+"` SET v='stale'")
	check("insert_update with every row changed", run(config.ImportModeInsertUpdate), counters{0, 4, 0})

	env.exec(t, "DELETE FROM `"+target+"` WHERE id IN (3, 4)")
	check("insert_update with 2 new and 2 identical rows", run(config.ImportModeInsertUpdate), counters{2, 0, 2})

	// Replace mode empties the target first, so each row of the file is a new
	// insert from the server's point of view.
	check("replace after the table was emptied", run(config.ImportModeReplace), counters{4, 0, 0})
}
