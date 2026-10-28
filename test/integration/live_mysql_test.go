package integration

// Live MySQL end-to-end tests. They are skipped unless IMEX_TEST_DSN is
// set, so `go test ./...` stays hermetic while still compiling this file:
//
//	IMEX_TEST_DSN='root:pass@tcp(127.0.0.1:3306)/imex_it' go test ./test/integration -run Live -v
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
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
	"github.com/imex/internal/export"
	_import "github.com/imex/internal/import"
)

type liveEnv struct {
	db   *sql.DB
	base config.Config
}

func newLiveEnv(t *testing.T) *liveEnv {
	t.Helper()

	dsn := os.Getenv("IMEX_TEST_DSN")
	if dsn == "" {
		t.Skip("set IMEX_TEST_DSN to run live MySQL integration tests")
	}

	mc, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse IMEX_TEST_DSN: %v", err)
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
		ProgressInterval:   config.DefaultProgressInterval,
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
	name := fmt.Sprintf("imex_it_%s_%d", prefix, time.Now().UnixNano())
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
	// SQL export is data-only by default; --create is what opts a dump into
	// carrying the table structure, which --mode create imports need.
	cfg.Create = opts.create
	if opts.progressInterval > 0 {
		cfg.ProgressInterval = opts.progressInterval
	}
	if opts.batchSize > 0 {
		cfg.BatchSize = opts.batchSize
	}

	if opts.messages != "" {
		defer teeLogToFile(t, opts.messages)()
	}

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
	// progressInterval compresses the progress ticker so a test can make the
	// record repeat; zero keeps the shipped default.
	progressInterval time.Duration
	// batchSize moves the hand-off option the parallel path does not read; zero
	// keeps the shipped default, which is the case that must stay quiet.
	batchSize int
	// create opts an SQL export into emitting a CREATE TABLE statement, exactly
	// as --create does on the command line. Only the --mode create round trip
	// needs it; every other export is the data-only default.
	create bool
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

	if opts.messages != "" {
		defer teeLogToFile(t, opts.messages)()
	}

	if opts.threads > 0 {
		cfg.ImportThreads = opts.threads
	}
	cfg.CommitCount = opts.commitCount
	if opts.batchSize > 0 {
		cfg.BatchSize = opts.batchSize
	}
	if opts.statementSize > 0 {
		cfg.StatementSize = opts.statementSize
	}
	if opts.progressInterval > 0 {
		cfg.ProgressInterval = opts.progressInterval
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
	// statementSize moves the import-side per-statement byte cap; zero keeps the
	// shipped default, which is "no extra cap over max_allowed_packet".
	statementSize int
	// progressInterval compresses the progress ticker; zero keeps the default.
	progressInterval time.Duration
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
			exportMsg := filepath.Join(dir, "export-"+format+".log")
			importMsg := filepath.Join(dir, "import-"+format+".log")

			env.export(t, exportOptions{table: src, format: format, output: file, messages: exportMsg, threads: 1})
			env.importFile(t, importOptions{
				input: file, format: format, mode: config.ImportModeInsert, table: dst,
				messages: importMsg, threads: 1, commitCount: 3, batchSize: 2,
			})

			assertSameRows(t, env.snapshotTable(t, src), env.snapshotTable(t, dst))
			assertMessageFile(t, exportMsg, "export config", "export completed")
			assertMessageFile(t, importMsg, "import config", "import completed")
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
	env.export(t, exportOptions{table: src, format: "sql", output: sqlFile, threads: 1, create: true})
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

// teeLogToFile hands the --messages sink over to path for the duration of a
// library-level run. newLiveEnv leaves the global logger at ERROR to keep test
// output readable, so the level has to come up for the file to hold the run's
// records — they go to the file only, so the harness output stays clean either
// way. The returned func closes the file and restores the harness level.
func teeLogToFile(t *testing.T, path string) func() {
	t.Helper()

	if err := common.OpenLogFile(path); err != nil {
		t.Fatalf("open log file: %v", err)
	}
	common.InitLogger(config.LogLevelInfo)
	return func() {
		if err := common.CloseLogFile(); err != nil {
			t.Errorf("close log file: %v", err)
		}
		common.InitLogger(config.LogLevelError)
	}
}

// assertMessageFile checks that a --messages file is exactly the run's command
// log: the wanted records are present, no DB2-style report survives, and every
// line is a log record in the default java layout.
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
	if i := strings.Index(text, "SQL31"); i >= 0 {
		end := i + 60
		if end > len(text) {
			end = len(text)
		}
		t.Errorf("message file %s still carries the DB2-style report: %s", path, text[i:end])
	}
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if !logRecordLine.MatchString(line) {
			t.Errorf("message file %s line is not a log record: %q", path, line)
		}
	}
}

// logRecordLine matches the java layout's fixed prefix:
// "2026-01-02 15:04:05.123 [1234] INFO message ...".
var logRecordLine = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3} \[\d+\] [A-Z]+ `)

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

// growTable doubles the table `times` times server-side, which is how a test
// gets a run long enough for the progress ticker to fire repeatedly without
// paying for a million client-side inserts. ids are shifted by the current row
// count so the primary key stays unique.
func (e *liveEnv) growTable(t *testing.T, table string, times int) {
	t.Helper()
	for i := 0; i < times; i++ {
		var n int
		if err := e.db.QueryRow("SELECT COUNT(*) FROM `" + table + "`").Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		e.exec(t, "INSERT INTO `"+table+"` (id, name, amount, note, created, payload, flag) "+
			"SELECT id + ?, name, amount, note, created, payload, flag FROM `"+table+"`", n)
	}
}

// TestLive_ProgressNeverOutlivesCompletion pins the record order operators
// asked for: the summary is the last thing a run writes. A "… progress" line
// after "… completed" reads as a job that is still going, and the window is
// real — a report already in flight when the work finishes cannot be recalled.
// The exporter and importer therefore stop the ticker before writing their
// summary, and stopping waits for the goroutine to leave report. The cadence is
// --progress-interval now, so the phases run on a 1 ms clock instead of the
// shipped 5 s, and the last phase drives the compiled binary to prove the flag
// actually reaches the ticker. The library path may still be driven below the CLI
// floor, but argv may not: validation refuses anything under
// config.MinProgressInterval, so the CLI leg uses the 10 ms minimum.
func TestLive_ProgressNeverOutlivesCompletion(t *testing.T) {
	env := newCLILive(t)
	dir := t.TempDir()

	src := env.liveTable(t, "order_src")
	dst := env.liveTable(t, "order_dst")
	env.exec(t, fmt.Sprintf(liveSourceDDL, src))
	env.exec(t, "INSERT INTO `"+src+"` VALUES (1,'seed','1.0000','note','2026-01-01 00:00:00',NULL,0)")
	env.growTable(t, src, 13) // 8192 rows: a run long enough for hundreds of ticks
	env.exec(t, fmt.Sprintf("CREATE TABLE `%s` LIKE `%s`", dst, src))

	defer func(q bool) { common.Quiet = q }(common.Quiet)
	common.Quiet = false // progress records are what is under test

	// The command log is the stream an operator reads, so the ordering is checked
	// there (see teeLogToFile: with --messages attached the records go only to
	// the file).
	file := filepath.Join(dir, "order.del")
	env.export(t, exportOptions{table: src, format: "del", output: file, messages: filepath.Join(dir, "export1.log"),
		threads: 1, progressInterval: time.Millisecond})
	assertLogEndsOn(t, filepath.Join(dir, "export1.log"), "export completed")

	env.export(t, exportOptions{table: src, format: "del", output: filepath.Join(dir, "order-p.del"),
		messages: filepath.Join(dir, "export2.log"), threads: 4, progressInterval: time.Millisecond})
	assertLogEndsOn(t, filepath.Join(dir, "export2.log"), "parallel export completed")

	env.importFile(t, importOptions{input: file, format: "del", mode: config.ImportModeInsert, table: dst,
		messages: filepath.Join(dir, "import1.log"), threads: 1, commitCount: 200, batchSize: 200,
		progressInterval: time.Millisecond})
	assertLogEndsOn(t, filepath.Join(dir, "import1.log"), "import completed")

	env.exec(t, "TRUNCATE TABLE `"+dst+"`")
	env.importFile(t, importOptions{input: file, format: "del", mode: config.ImportModeInsert, table: dst,
		messages: filepath.Join(dir, "import2.log"), threads: 4, commitCount: 200, batchSize: 200,
		progressInterval: time.Millisecond})
	assertLogEndsOn(t, filepath.Join(dir, "import2.log"), "parallel import completed")

	// Same invariant through the real argv, plus the two things the flag and the
	// dropped start records changed: --progress-interval must land in the ticker,
	// and the config echo must be the record that opens the run (there is no
	// longer a "export started" line repeating part of it).
	cliLog := filepath.Join(dir, "cli-export.log")
	cliFile := filepath.Join(dir, "cli-order.del")
	out, code := env.runExportCLI(t, "-q", "SELECT * FROM `"+src+"`", "-f", "del", "-o", cliFile,
		"--log-level", "info", "--progress-interval", "10ms", "--messages", cliLog)
	if code != 0 {
		t.Fatalf("CLI export with --progress-interval: exit %d\n%s", code, out)
	}
	text := readFile(t, cliLog)
	assertLogEndsOn(t, cliLog, "export completed")
	if strings.Contains(text, " started ") {
		t.Errorf("the run still logs a start record next to the config echo:\n%s", text)
	}
	if !strings.Contains(text, "progress_interval=10ms") {
		t.Errorf("config echo does not carry the resolved cadence:\n%s", text)
	}
}

// assertLogEndsOn checks that a run's last log record is its summary and that
// progress was actually being reported — otherwise the ordering it pins would
// never have been exercised.
func assertLogEndsOn(t *testing.T, path, want string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log %s: %v", path, err)
	}
	text := string(data)
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) == 0 {
		t.Fatalf("log %s is empty", path)
	}
	if !strings.Contains(text, "progress") {
		t.Fatalf("no progress record in %s, so the ordering was not exercised:\n%s", path, text)
	}
	last := lines[len(lines)-1]
	if !strings.Contains(last, want) {
		t.Errorf("last record is %q, want the %q summary:\n%s", last, want, text)
	}
}

// TestLive_ParallelExportReportsIgnoredPipelineOptions pins the notice the
// parallel path writes for --batch-size, an option it never reads: moving it has
// to say so (the run otherwise looks like it honoured the tuning), and leaving it
// at the shipped default must not add a record to an ordinary run.
func TestLive_ParallelExportReportsIgnoredPipelineOptions(t *testing.T) {
	env := newLiveEnv(t)
	dir := t.TempDir()

	src := env.liveTable(t, "pipeign")
	env.exec(t, fmt.Sprintf(liveSourceDDL, src))
	for i := 0; i < 200; i++ {
		env.exec(t, "INSERT INTO `"+src+"` VALUES (?,?,?,?,?,?,?)",
			i+1, fmt.Sprintf("name-%03d", i), fmt.Sprintf("%d.%04d", i*1000, i),
			strings.Repeat("n", i%40), "2026-01-01 00:00:00", []byte{byte(i), byte(i >> 8)}, i%3)
	}

	const notice = "parallel export ignores pipeline options"

	tuned := filepath.Join(dir, "tuned.log")
	env.export(t, exportOptions{
		table: src, format: "del", output: filepath.Join(dir, "tuned.del"),
		threads: 4, batchSize: 5000, messages: tuned,
	})
	assertMessageFile(t, tuned, notice)

	defaults := filepath.Join(dir, "defaults.log")
	env.export(t, exportOptions{
		table: src, format: "del", output: filepath.Join(dir, "defaults.del"),
		threads: 4, messages: defaults,
	})
	if text := readFile(t, defaults); strings.Contains(text, notice) {
		t.Errorf("a run that touched neither hand-off option gained the notice:\n%s", text)
	}

	// The single-threaded path reads both options, so it must stay quiet too.
	single := filepath.Join(dir, "single.log")
	env.export(t, exportOptions{
		table: src, format: "del", output: filepath.Join(dir, "single.del"),
		threads: 1, batchSize: 5000, messages: single,
	})
	if text := readFile(t, single); strings.Contains(text, notice) {
		t.Errorf("--batch-size takes effect on the single-threaded path, but it was reported as ignored:\n%s", text)
	}
}

// statusCounter reads one server-side status counter. The name is inlined rather
// than bound because SHOW ... LIKE does not accept a placeholder on every server
// version; every caller here passes a literal.
func (e *liveEnv) statusCounter(t *testing.T, name string) int64 {
	t.Helper()

	var key, value string
	if err := e.db.QueryRow("SHOW GLOBAL STATUS LIKE '"+name+"'").Scan(&key, &value); err != nil {
		t.Fatalf("read status %s: %v", name, err)
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		t.Fatalf("status %s = %q: %v", name, value, err)
	}
	return n
}

// rowChecksum reduces a (id, name) table to rows plus a content sum, so two
// imports of the same file can be compared without hauling every row through the
// test.
func (e *liveEnv) rowChecksum(t *testing.T, table string) string {
	t.Helper()

	var rows int64
	var sum sql.NullString
	query := "SELECT COUNT(*), COALESCE(SUM(CRC32(CONCAT_WS('|', id, name))), 0) FROM `" + table + "`"
	if err := e.db.QueryRow(query).Scan(&rows, &sum); err != nil {
		t.Fatalf("checksum %s: %v", table, err)
	}
	return fmt.Sprintf("%d/%s", rows, sum.String)
}

// TestLive_ImportStatementSizeCapsStatements measures import --statement-size on
// the server instead of in the log. Two imports of the same file, into the same
// table shape, with the same --batch-size, differ only in the byte ceiling; the
// target counts the INSERT statements it executed, so a lower ceiling has to show
// up as strictly more statements with identical landed rows. That also pins the
// default: without the option the whole file still goes out as one statement.
func TestLive_ImportStatementSizeCapsStatements(t *testing.T) {
	env := newLiveEnv(t)
	dir := t.TempDir()

	src := env.liveTable(t, "stmtcap_src")
	env.exec(t, "CREATE TABLE `"+src+"` (id BIGINT NOT NULL PRIMARY KEY, name VARCHAR(200) NOT NULL) DEFAULT CHARSET=utf8mb4")
	env.exec(t, "INSERT INTO `"+src+"` VALUES (1, ?)", strings.Repeat("x", 190))
	// 2048 rows of a fixed payload, ~440 KB of SQL text: far below this server's
	// max_allowed_packet budget, so only an explicit cap can split it.
	for i := 0; i < 11; i++ {
		var n int64
		if err := env.db.QueryRow("SELECT COUNT(*) FROM `" + src + "`").Scan(&n); err != nil {
			t.Fatalf("count %s: %v", src, err)
		}
		env.exec(t, "INSERT INTO `"+src+"` (id, name) SELECT id + ?, name FROM `"+src+"`", n)
	}

	file := filepath.Join(dir, "stmtcap.del")
	env.export(t, exportOptions{table: src, format: "del", output: file})

	dstOff := env.liveTable(t, "stmtcap_off")
	dstOn := env.liveTable(t, "stmtcap_on")
	for _, table := range []string{dstOff, dstOn} {
		env.exec(t, fmt.Sprintf("CREATE TABLE `%s` LIKE `%s`", table, src))
	}

	// --batch-size stays above the row count so the byte budget, not the row
	// count, is the only ceiling that can end a statement in either run.
	const rows = 2048
	base := importOptions{
		input: file, format: "del", mode: config.ImportModeInsert,
		table: dstOff, commitCount: 10 * rows, batchSize: 10 * rows,
		messages: filepath.Join(dir, "off.log"),
	}
	before := env.statusCounter(t, "Com_insert")
	env.importFile(t, base)
	uncapped := env.statusCounter(t, "Com_insert") - before

	on := base
	on.table = dstOn
	on.statementSize = 64 << 10
	on.messages = filepath.Join(dir, "on.log")
	before = env.statusCounter(t, "Com_insert")
	env.importFile(t, on)
	capped := env.statusCounter(t, "Com_insert") - before

	t.Logf("2048 rows: %d INSERT statement(s) without --statement-size, %d with --statement-size 65536",
		uncapped, capped)

	if uncapped != 1 {
		t.Errorf("without --statement-size the file went out as %d INSERT statements, want 1", uncapped)
	}
	// ~440 KB against a 64 KiB ceiling is about seven statements. The bound is
	// loose on purpose: the assertion is that the cap split the work, not that
	// estimateRowBytes predicts the packet width to the byte.
	if capped < 5 || capped > 20 {
		t.Errorf("with --statement-size 65536 the file went out as %d INSERT statements, want about 7", capped)
	}
	if capped <= uncapped {
		t.Errorf("--statement-size changed nothing: %d statements versus %d without it", capped, uncapped)
	}

	assertMessageFile(t, base.messages, "import batch byte cap resolved",
		"statement_size=0", "limiting_factor="+defaultCapFactor(t, env.db))
	assertMessageFile(t, on.messages, "import batch byte cap resolved",
		"statement_size=65536", "limiting_factor=statement-size")

	// Smaller statements must still land the same rows.
	want := env.rowChecksum(t, src)
	for _, table := range []string{dstOff, dstOn} {
		if got := env.rowChecksum(t, table); got != want {
			t.Errorf("%s holds %s, want the source rows %s", table, got, want)
		}
	}
}
