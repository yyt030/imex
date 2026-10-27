package integration

// Cross-option / cross-command tests driven through the real CLI.
//
// cmd/cli_matrix_test.go pins the *validation* layer (which flag combinations
// are accepted or rejected). What that cannot see is whether an option that is
// accepted actually takes effect, and takes effect the same way on both sides of
// the export/import pair. These tests therefore invoke the compiled binary with
// real argv, so flag parsing, validation, config wiring, the writer and the
// reader are all on the path for every combination.
//
// The invariants checked per family:
//
//   - round-trip equality: exporting and importing the same table with the same
//     options must return the rows byte-for-byte (liveEnv.snapshotTable);
//   - option symmetry: an option that changes the file on the export side must
//     be the option that makes the file readable on the import side;
//   - output determinism: an option that must not change bytes (thread count,
//     batching) must not change them;
//   - side-effect discipline: --messages, --no-overwrite and failure paths must
//     leave exactly the files they promise, no more.
//
// They are skipped unless IMEX_TEST_DSN is set:
//
//	CGO_ENABLED=1 IMEX_TEST_DSN='<user>:<password>@tcp(127.0.0.1:3306)/imex_test?parseTime=true' \
//	  go test -count=1 ./test/integration -run OptionMatrix -v

import (
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/imex/internal/config"
)

// cliBinary is built once per test run by TestMain and reused by every case.
var cliBinary string

func TestMain(m *testing.M) {
	if os.Getenv("IMEX_TEST_DSN") == "" {
		// Library-level tests skip themselves; nothing to build.
		os.Exit(m.Run())
	}

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve module root: %v\n", err)
		os.Exit(1)
	}
	dir, err := os.MkdirTemp("", "imex-cli-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create build dir: %v\n", err)
		os.Exit(1)
	}
	cliBinary = filepath.Join(dir, "imex")
	build := exec.Command("go", "build", "-o", cliBinary, "github.com/imex")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build imex for CLI tests: %v\n%s\n", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// cliEnv wraps liveEnv with argv plumbing for the built binary.
type cliEnv struct {
	*liveEnv
	dir   string
	base  []string // connection flags shared by every invocation
	table string   // last source table used, for content assertions
}

func newCLILive(t *testing.T) *cliEnv {
	t.Helper()

	env := newLiveEnv(t)
	dsn := os.Getenv("IMEX_TEST_DSN")
	mc, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	var host, port string
	if mc.Net != "unix" {
		host, port, err = net.SplitHostPort(mc.Addr)
		if err != nil {
			t.Fatalf("split %q: %v", mc.Addr, err)
		}
	}

	base := []string{"-u", mc.User, "-p", mc.Passwd, "-D", mc.DBName, "--log-level", "error"}
	if mc.Net == "unix" {
		base = append(base, "-S", mc.Addr)
	} else {
		base = append(base, "-h", host, "-P", port)
	}

	return &cliEnv{liveEnv: env, dir: t.TempDir(), base: base}
}

// runCLI executes the binary and returns combined output plus the exit code.
func (c *cliEnv) runCLI(t *testing.T, args ...string) (string, int) {
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
	text := strings.TrimRight(string(out), "\r\n")
	// Strip non-printable bytes so assertions on the message text stay readable
	// in terminals and logs that dislike control characters.
	return text, code
}

// runExportCLI is the failure-path counterpart of cliExport: same connection
// flags, but the caller asserts on the exit code and output itself.
func (c *cliEnv) runExportCLI(t *testing.T, args ...string) (string, int) {
	t.Helper()
	return c.runCLI(t, append([]string{"export"}, append(append([]string{}, c.base...), args...)...)...)
}

// runImportCLI is the failure-path counterpart of cliImport.
func (c *cliEnv) runImportCLI(t *testing.T, args ...string) (string, int) {
	t.Helper()
	return c.runCLI(t, append([]string{"import"}, append(append([]string{}, c.base...), args...)...)...)
}

// safeFileName turns a test case name into a single path element, so a case
// named "dd/MM/yyyy + HH:mm" does not silently become a nested directory.
func safeFileName(name string) string {
	return strings.NewReplacer("/", "-", `\`, "-", " ", "_", ":", "").Replace(name)
}

// cliExport runs an export with the given extra flags into a fresh file and
// returns its path. It fails the test if the exit code is not 0; use runCLI
// directly to assert on failures.
func (c *cliEnv) cliExport(t *testing.T, table, format, name string, extra ...string) string {
	t.Helper()
	path := filepath.Join(c.dir, safeFileName(name))
	args := append([]string{"export"}, c.base...)
	args = append(args, "-q", "SELECT * FROM `"+table+"`", "-f", format, "-o", path)
	args = append(args, extra...)
	out, code := c.runCLI(t, args...)
	if code != 0 {
		t.Fatalf("export %s as %s: exit %d\n%s", table, format, code, out)
	}
	c.table = table
	return path
}

// cliImport runs an import with the given extra flags. Like cliExport it is the
// success-path helper.
func (c *cliEnv) cliImport(t *testing.T, table, format, path string, extra ...string) {
	t.Helper()
	args := append([]string{"import"}, c.base...)
	args = append(args, "-i", path, "-t", table, "-f", format)
	args = append(args, extra...)
	out, code := c.runCLI(t, args...)
	if code != 0 {
		t.Fatalf("import %s into %s: exit %d\n%s", path, table, code, out)
	}
}

// makePair creates a source table filled with the standard tricky rows and a
// sibling table with the same structure, ready to receive an import.
func (c *cliEnv) makePair(t *testing.T, prefix string) (src, dst string) {
	t.Helper()
	src = c.liveTable(t, prefix+"_src")
	dst = c.liveTable(t, prefix+"_dst")
	c.fillSource(t, src)
	c.exec(t, fmt.Sprintf("CREATE TABLE `%s` LIKE `%s`", dst, src))
	return src, dst
}

// readFile returns the export file's text, so a case can assert that an option
// actually shaped the file and not just the row set.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// roundTrip is the workhorse assertion: export src with exportFlags, import the
// result into dst with importFlags, then compare the two tables row by row.
func (c *cliEnv) roundTrip(t *testing.T, name, format, src, dst string, exportFlags, importFlags []string) string {
	t.Helper()

	file := c.cliExport(t, src, format, name+"."+format, exportFlags...)
	if name != "" {
		t.Logf("exported %s (%d bytes)", file, fileSize(t, file))
	}
	c.cliImport(t, dst, format, file, importFlags...)
	assertSameRows(t, c.snapshotTable(t, src), c.snapshotTable(t, dst))
	return file
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return st.Size()
}

// TestOptionMatrixDELModifiers crosses every DEL layout modifier between the two
// commands: the flag that shapes the file on export must be the flag that makes
// it readable on import, and no modifier may corrupt a value.
func TestOptionMatrixDELModifiers(t *testing.T) {
	env := newCLILive(t)

	cases := []struct {
		name        string
		exportFlags []string
		importFlags []string
		// wantInFile asserts the modifier really reached the output bytes.
		wantInFile string
		// plain uses a delimiter-free fixture. --no-escape and --no-char-del write
		// fields verbatim, so the exporter refuses any value that contains a
		// structural delimiter; the standard fixture deliberately contains those.
		plain bool
	}{
		{
			name:       "默认",
			wantInFile: "\"name\"",
		},
		{
			name:        "自定义字符定界符与列定界符",
			exportFlags: []string{"--char-del", "!", "--col-del", "@"},
			importFlags: []string{"--char-del", "!", "--col-del", "@"},
			wantInFile:  "@!café",
		},
		{
			name:        "反斜杠转义",
			exportFlags: []string{"--escape", `\`},
			importFlags: []string{"--escape", `\`},
		},
		{
			name:        "不使用字符定界符",
			exportFlags: []string{"--no-char-del"},
			importFlags: []string{"--no-char-del"},
			wantInFile:  "1,plain,",
			plain:       true,
		},
		{
			// The pairing is symmetric: a dump without the column-name row has to
			// be read back without it, with the target columns named explicitly
			// because the file no longer carries them.
			name:        "不输出列名行",
			exportFlags: []string{"--no-col-names"},
			importFlags: []string{"--no-col-names", "--columns", "id,name,amount,note,created,payload,flag"},
		},
		{
			name:        "关闭转义",
			exportFlags: []string{"--no-escape"},
			importFlags: []string{"--no-escape"},
			plain:       true,
		},
		{
			name:        "CRLF 行定界符",
			exportFlags: []string{"--row-del", `\r\n`},
			importFlags: []string{"--row-del", `\r\n`},
		},
		{
			name:        "仅改列定界符为制表符",
			exportFlags: []string{"--col-del", `\t`},
			importFlags: []string{"--col-del", `\t`},
			wantInFile:  "\t\"plain\"",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var src, dst string
			if tc.plain {
				src, dst = env.makePlainPair(t, "delmod")
			} else {
				src, dst = env.makePair(t, "delmod")
			}
			file := env.roundTrip(t, tc.name, "del", src, dst, tc.exportFlags, tc.importFlags)
			if tc.wantInFile != "" && !strings.Contains(readFile(t, file), tc.wantInFile) {
				t.Errorf("output does not contain %q, so the modifier did not reach the file", tc.wantInFile)
			}
		})
	}
}

// TestOptionMatrixNoColNamesSymmetry pins what happens when the two sides of the
// --no-col-names pair are not matched. The file cannot say whether its first
// record is a header or data, so positional --columns keeps the header behaviour
// for normal dumps and the tool's only safe move is to report the record it
// dropped: a WARN record naming the fields, which --messages writes to the log
// file — and, with the option given, to nothing else.
func TestOptionMatrixNoColNamesSymmetry(t *testing.T) {
	env := newCLILive(t)
	src, dst := env.makePair(t, "ncol")

	file := env.cliExport(t, src, "del", "no_col_names.del", "--no-col-names")
	msg := filepath.Join(env.dir, "no-col-names.log")
	out, code := env.runImportCLI(t, "-i", file, "-t", dst, "-f", "del",
		"--columns", "id,name,amount,note,created,payload,flag", "--messages", msg,
		"--log-level", "warn")
	if code != 0 {
		t.Fatalf("import failed: exit %d\n%s", code, out)
	}

	if strings.TrimSpace(out) != "" {
		t.Errorf("with --messages the warning belongs to the file, not the terminal:\n%s", out)
	}
	text := readFile(t, msg)
	if !strings.Contains(text, "was treated as the column header") {
		t.Errorf("the message file must carry the warning:\n%s", text)
	}
	if n := env.rowCount(t, dst); n != 9 {
		t.Errorf("imported %d rows, want 9: the first record is consumed as a header, which is the ambiguity the warning reports", n)
	}

	// The matched pairing loses nothing.
	env.exec(t, "TRUNCATE TABLE `"+dst+"`")
	out, code = env.runImportCLI(t, "-i", file, "-t", dst, "-f", "del", "--no-col-names",
		"--columns", "id,name,amount,note,created,payload,flag")
	if code != 0 {
		t.Fatalf("matched import failed: exit %d\n%s", code, out)
	}
	assertSameRows(t, env.snapshotTable(t, src), env.snapshotTable(t, dst))
}

// makePlainPair builds the same table shape as makePair but with values that
// contain no structural delimiter (",", "\n", "\r", '"'), which the verbatim
// writing modes --no-escape and --no-char-del cannot represent.
func (c *cliEnv) makePlainPair(t *testing.T, prefix string) (src, dst string) {
	t.Helper()
	src = c.liveTable(t, prefix+"_plain_src")
	dst = c.liveTable(t, prefix+"_plain_dst")
	c.exec(t, fmt.Sprintf(liveSourceDDL, src))
	c.exec(t, "CREATE TABLE `"+dst+"` LIKE `"+src+"`")
	rows := []struct {
		id      int64
		name    interface{}
		amount  interface{}
		note    interface{}
		created interface{}
		payload interface{}
		flag    interface{}
	}{
		{1, "plain", "1.5000", "note one", "2024-01-02 03:04:05", []byte{0x01, 0x02}, 1},
		{2, `O'Brien`, "-0.0001", nil, "1970-01-01 00:00:00", nil, 0},
		{3, "café 中文", "42.4242", "multi-byte", "2038-01-19 03:14:07", []byte{0x80, 0x81}, -128},
	}
	for _, r := range rows {
		c.exec(t, "INSERT INTO `"+src+"` VALUES (?,?,?,?,?,?,?)",
			r.id, r.name, r.amount, r.note, r.created, r.payload, r.flag)
	}
	return src, dst
}

// TestOptionMatrixDELGuardRefusal crosses the verbatim writing modes with data
// that cannot be represented. The refusal itself is the contract: a clear
// message, a non-zero exit, and -- like the parallel path -- no leftover output,
// so a failed dump can never be mistaken for a complete one.
func TestOptionMatrixDELGuardRefusal(t *testing.T) {
	env := newCLILive(t)

	cases := []struct {
		name  string
		flags []string
		want  string
	}{
		{"含换行的值遇上 --no-char-del", []string{"--no-char-del"}, "would corrupt the DEL record"},
		{"含引号的值遇上 --no-escape", []string{"--no-escape"}, "would corrupt the DEL record"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := env.liveTable(t, "delguard")
			env.fillSource(t, src) // contains embedded newlines and quote characters
			path := filepath.Join(env.dir, "guard_"+safeFileName(tc.name)+".del")
			args := append([]string{"-q", "SELECT * FROM `" + src + "`", "-f", "del", "-o", path, "--threads", "1"}, tc.flags...)
			out, code := env.runExportCLI(t, args...)
			if code == 0 {
				t.Fatalf("export of unrepresentable data must fail\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("output lacks %q:\n%s", tc.want, out)
			}
			if st, err := os.Stat(path); err == nil && st.Size() > 0 {
				t.Errorf("failed export left a %d-byte partial file at %s", st.Size(), path)
			}
		})
	}
}

// TestOptionMatrixPartialOutputCleanup pins the "either complete or absent" rule
// across both export paths. The refusal has to arrive after the writer's buffer
// has already flushed, otherwise the case proves nothing: 4000 rows of 100
// characters is ~400 KiB, past the 256 KiB bufio window, and the unrepresentable
// row comes last.
func TestOptionMatrixPartialOutputCleanup(t *testing.T) {
	env := newCLILive(t)

	src := env.liveTable(t, "partial_src")
	env.exec(t, fmt.Sprintf(liveSourceDDL, src))
	const bulk = 3000
	filler := strings.Repeat("x", 100) // keeps the run past the 256 KiB write buffer
	var sb strings.Builder
	sb.WriteString("INSERT INTO `" + src + "` VALUES ")
	for i := 1; i <= bulk; i++ {
		if i > 1 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "(%d,'%s',1.0000,'x','2024-01-02 03:04:05',0x01,1)", i, filler)
	}
	env.exec(t, sb.String())
	env.exec(t, "INSERT INTO `"+src+"` VALUES (?,?,?,?,?,?,?)",
		int64(bulk+1), "bad\nvalue", "2.0000", "n", "2024-01-02 03:04:06", []byte{0x02}, 1)

	for _, threads := range []string{"1", "4"} {
		t.Run("threads="+threads, func(t *testing.T) {
			path := filepath.Join(env.dir, "partial-"+threads+".del")
			out, code := env.runExportCLI(t, "-q", "SELECT * FROM `"+src+"` ORDER BY id",
				"-f", "del", "-o", path, "--threads", threads, "--no-char-del")
			if code == 0 {
				t.Fatalf("export must refuse the unrepresentable row\n%s", out)
			}
			st, err := os.Stat(path)
			if err != nil {
				return // removed: the contract holds
			}
			t.Errorf("failed %s-thread export left %d bytes of partial output; a truncated dump must not survive the run (serial path: %v)",
				threads, st.Size(), firstLines(out, 2))
		})
	}
}

// statementCapSlack is how far a completed multi-row INSERT may exceed
// --statement-size. The writer tests the cap before appending a tuple, counting
// the pending VALUES and the column list but not the ", " that joins the new
// tuple or the ";\n" that closes the statement, so four bytes is the ceiling.
const statementCapSlack = 4

// TestOptionMatrixSQLBatching asserts the SQL layout contract from both sides:
// the default writes one INSERT per row, --extended-insert merges them (bounded
// by --statement-size), and every layout still round-trips to the same rows.
func TestOptionMatrixSQLBatching(t *testing.T) {
	env := newCLILive(t)
	src, _ := env.makePair(t, "sqlbatch")

	// One row per INSERT is the default layout, so the baseline is the statement
	// count to beat: any merging option must produce strictly fewer.
	baseline := env.cliExport(t, src, "sql", "baseline.sql")
	baseText := readFile(t, baseline)
	baseInserts := strings.Count(baseText, "INSERT INTO ")
	if baseInserts != 10 {
		t.Fatalf("default SQL layout is %d statements for a 10-row table, want one per row:\n%s", baseInserts, firstLines(baseText, 12))
	}

	cases := []struct {
		name  string
		flags []string
		// wantInserts is the exact statement count; a negative value means "merged,
		// so fewer than the one-row-per-statement baseline".
		wantInserts int
		// maxStmtBytes bounds every generated statement when the option is a size cap.
		maxStmtBytes int
	}{
		// The narrowest row statement here is over 160 bytes and the widest is near
		// 800, so a 512-byte cap has to be compared against those: it merges the
		// narrow rows and still leaves the wide row on its own line. A cap below
		// every row merges nothing at all and the file is back to one INSERT per
		// row, which the writer now warns about once (pinned in
		// internal/export/sql_writer_test.go) — the shape is what this table checks.
		{"--extended-insert 上限低于单行时退回逐行布局", []string{"--extended-insert", "--statement-size", "64"}, 10, 0},
		{"--extended-insert 合并成多行语句", []string{"--extended-insert"}, -1, 0},
		{"--extended-insert 按 512 字节攒批", []string{"--extended-insert", "--statement-size", "512"}, -1, 512},
		{"单给 --statement-size 不改变默认布局", []string{"--statement-size", "512"}, 10, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file := env.cliExport(t, src, "sql", "variant_"+tc.name, tc.flags...)
			text := readFile(t, file)

			n := strings.Count(text, "INSERT INTO ")
			switch {
			case tc.wantInserts > 0 && n != tc.wantInserts:
				t.Errorf("generated %d INSERT statements, want exactly %d", n, tc.wantInserts)
			case tc.wantInserts < 0 && n >= baseInserts:
				t.Errorf("--extended-insert merged nothing: %d statements, baseline is %d", n, baseInserts)
			}
			if tc.maxStmtBytes > 0 {
				for _, line := range strings.Split(text, "\n") {
					if !strings.HasPrefix(line, "INSERT INTO ") {
						continue
					}
					// A single row larger than the cap must still be emitted whole
					// (graceful degradation), so only multi-row statements are bound.
					// The cap is checked as "pending VALUES + new tuple + column list",
					// so the ", " join and the ";\n" terminator land on top of it: four
					// bytes is the most a completed statement can run over.
					if strings.Count(line, "), (") > 0 && len(line) > tc.maxStmtBytes+statementCapSlack {
						t.Errorf("multi-row statement is %d bytes, over the %d-byte --statement-size: %s",
							len(line), tc.maxStmtBytes, firstLines(line, 1))
					}
				}
			}

			dst := env.liveTable(t, "sqlbatch_dst")
			env.exec(t, fmt.Sprintf("CREATE TABLE `%s` LIKE `%s`", dst, src))
			env.cliImport(t, dst, "sql", file)
			assertSameRows(t, env.snapshotTable(t, src), env.snapshotTable(t, dst))
		})
	}
}

// TestOptionMatrixParallelOutput pins what --threads must and must not change.
// A parallel DEL dump is byte-identical to a serial one, because the shard pipes
// are drained in shard order by a single writer. The default SQL layout is
// byte-identical for the same reason plus one more: a row is a whole statement,
// so a shard boundary never leaves an open INSERT to flush. With
// --extended-insert the boundary does cost something, and the invariant is
// bounded and exact -- at most threads-1 additional statements, and the byte
// difference is precisely the size of those headers -- so neither rows nor
// escaping can drift.
func TestOptionMatrixParallelOutput(t *testing.T) {
	env := newCLILive(t)
	src, _ := env.makePair(t, "parout")

	serialDEL := readFile(t, env.cliExport(t, src, "del", "serial.del"))
	serialSQLText := readFile(t, env.cliExport(t, src, "sql", "serial.sql"))
	mergedText := readFile(t, env.cliExport(t, src, "sql", "serial_merged.sql", "--extended-insert"))
	header := insertHeader(mergedText)
	if header == "" {
		t.Fatal("serial SQL dump has no INSERT header to measure against")
	}
	if strings.Count(serialSQLText, header) != 10 {
		t.Fatalf("serial default dump has %d INSERT headers for a 10-row table, want one per row",
			strings.Count(serialSQLText, header))
	}

	for _, threads := range []string{"2", "4"} {
		t.Run("threads="+threads, func(t *testing.T) {
			delFile := env.cliExport(t, src, "del", "parallel_"+threads+".del", "--threads", threads)
			if got := readFile(t, delFile); got != serialDEL {
				t.Errorf("parallel DEL is not byte-identical to serial (%d vs %d bytes)", len(got), len(serialDEL))
			}

			sqlFile := env.cliExport(t, src, "sql", "parallel_"+threads+".sql", "--threads", threads)
			if got := readFile(t, sqlFile); got != serialSQLText {
				t.Errorf("parallel single-row SQL dump is not byte-identical to serial (%d vs %d bytes)",
					len(got), len(serialSQLText))
			}

			mergedFile := env.cliExport(t, src, "sql", "parallel_merged_"+threads+".sql",
				"--threads", threads, "--extended-insert")
			got := readFile(t, mergedFile)

			nSerial := strings.Count(mergedText, header)
			nParallel := strings.Count(got, header)
			extra := nParallel - nSerial
			if extra < 0 || extra > 4 { // 4 shards => at most 3 boundaries, 1 to spare
				t.Errorf("parallel dump has %d INSERT headers vs %d serial, extra=%d out of bounds",
					nParallel, nSerial, extra)
			}
			// Each extra statement costs exactly one header: the row separator ", "
			// and the statement terminator + newline ";\n" are the same width.
			if delta := len(got) - len(mergedText); delta != extra*len(header) {
				t.Errorf("parallel dump is %d bytes larger than serial for %d extra statements (header %d bytes)",
					delta, extra, len(header))
			}

			dst := env.liveTable(t, "parout_dst")
			env.exec(t, fmt.Sprintf("CREATE TABLE `%s` LIKE `%s`", dst, src))
			env.cliImport(t, dst, "sql", sqlFile)
			assertSameRows(t, env.snapshotTable(t, src), env.snapshotTable(t, dst))
		})
	}
}

// insertHeader returns the "INSERT INTO ... VALUES " prefix shared by the
// statements of a SQL dump, or "" when the dump has none.
func insertHeader(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "INSERT INTO ") {
			if idx := strings.Index(line, " VALUES "); idx >= 0 {
				return line[:idx+len(" VALUES ")]
			}
		}
	}
	return ""
}

// TestOptionMatrixDateFormats crosses --date-fmt and --timestamp-fmt between the
// commands. The exporter formats DATE/DATETIME text with the configured pattern
// and the importer must read that same pattern back; a mismatch on either side
// silently shifts or drops dates.
func TestOptionMatrixDateFormats(t *testing.T) {
	env := newCLILive(t)

	cases := []struct {
		name  string
		flags []string
		// wantDateText is what row id=3 ("2024-02-29 00:00:00") must look like.
		wantDateText string
	}{
		{"dd-MM-yyyy 全精度时刻", []string{"--date-fmt", "dd-MM-yyyy", "--timestamp-fmt", "dd-MM-yyyy HH:mm:ss"}, "29-02-2024 00:00:00"},
		{"yyyyMMdd", []string{"--date-fmt", "yyyyMMdd", "--timestamp-fmt", "yyyyMMdd HHmmss"}, "20240229 000000"},
		{"默认", nil, "2024-02-29 00:00:00"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, dst := env.makePair(t, "datefmt")
			file := env.roundTrip(t, tc.name, "del", src, dst, tc.flags, tc.flags)
			if !strings.Contains(readFile(t, file), tc.wantDateText) {
				t.Errorf("expected %q in the file, so --date-fmt/--timestamp-fmt must shape the written text", tc.wantDateText)
			}
		})
	}

	// A format without "ss" cannot carry the seconds, so the round trip keeps the
	// date and zeroes the seconds. Nothing warns about it, which is exactly why the
	// outcome is pinned: a precision loss the user asked for must stay confined to
	// the seconds, and no other field may move.
	t.Run("丢失秒的格式", func(t *testing.T) {
		src, dst := env.makePair(t, "datelossy")
		flags := []string{"--timestamp-fmt", "yyyy-MM-dd HH:mm"}
		file := env.cliExport(t, src, "del", "lossy_seconds.del", flags...)
		if !strings.Contains(readFile(t, file), "2026-01-02 03:04") {
			t.Error("--timestamp-fmt without ss must drop the seconds from the file")
		}
		env.cliImport(t, dst, "del", file, flags...)
		before, after := env.snapshotTable(t, src), env.snapshotTable(t, dst)
		for i := range before {
			want := strings.Split(before[i], "|")
			got := strings.Split(after[i], "|")
			if len(want) < 5 || len(got) < 5 {
				t.Fatalf("unexpected snapshot shape: %v / %v", want, got)
			}
			if len(want[4]) >= 16 && len(got[4]) >= 16 {
				want[4], got[4] = want[4][:16]+":00", got[4][:16]+":00"
			}
			if fmt.Sprint(want) != fmt.Sprint(got) {
				t.Errorf("row %d changed beyond the truncated seconds:\n got %v\nwant %v", i, got, want)
			}
		}
	})
}

// TestOptionMatrixNoDataCreate crosses the SQL export defaults (--create, which
// is off by default, and --no-data) with the import modes, including the
// combinations that can only fail and must fail with a usable message rather
// than an empty table.
func TestOptionMatrixNoDataCreate(t *testing.T) {
	env := newCLILive(t)
	src, _ := env.makePair(t, "nodelete")

	t.Run("默认 SQL 导出仅数据", func(t *testing.T) {
		file := env.cliExport(t, src, "sql", "data_only.sql")
		if strings.Contains(readFile(t, file), "CREATE TABLE") {
			t.Error("SQL export defaults to data-only and must not emit a CREATE TABLE statement")
		}
		dst := env.liveTable(t, "data_dst")
		env.exec(t, fmt.Sprintf("CREATE TABLE `%s` LIKE `%s`", dst, src))
		env.cliImport(t, dst, "sql", file)
		assertSameRows(t, env.snapshotTable(t, src), env.snapshotTable(t, dst))
	})

	t.Run("--create 写入结构且保留数据", func(t *testing.T) {
		file := env.cliExport(t, src, "sql", "with_create.sql", "--create")
		text := readFile(t, file)
		if !strings.Contains(text, "CREATE TABLE") {
			t.Error("--create must emit a CREATE TABLE statement")
		}
		if !strings.Contains(text, "INSERT INTO") {
			t.Error("--create must not swallow the data")
		}
	})

	t.Run("仅结构 --create --no-data + mode create", func(t *testing.T) {
		file := env.cliExport(t, src, "sql", "structure_only.sql", "--create", "--no-data")
		dst := env.liveTable(t, "struct_dst")
		env.cliImport(t, dst, "sql", file, "--mode", string(config.ImportModeCreate))
		if n := env.rowCount(t, dst); n != 0 {
			t.Errorf("structure-only import left %d rows, want 0", n)
		}
	})

	t.Run("默认仅数据 + mode create 必须明确失败", func(t *testing.T) {
		file := env.cliExport(t, src, "sql", "data_only_for_create.sql")
		dst := env.liveTable(t, "data_create_dst")
		out, code := env.runImportCLI(t, "-i", file, "-t", dst, "-f", "sql", "--mode", "create")
		if code == 0 {
			t.Fatalf("importing a data-only file with --mode create must not succeed\n%s", out)
		}
		if code != 1 && code != 2 {
			t.Errorf("unexpected exit code %d\n%s", code, out)
		}
		t.Logf("exit=%d output=%s", code, firstLines(out, 3))
	})
}

// TestOptionMatrixSliceOptions crosses --skip-count / --row-count with the
// single-threaded and parallel import paths, so the two paths must slice the
// input identically.
func TestOptionMatrixSliceOptions(t *testing.T) {
	env := newCLILive(t)
	src := env.liveTable(t, "slice_src")
	env.fillSource(t, src)

	// 10 rows in the fixture; take the middle slice.
	file := env.cliExport(t, src, "del", "slice.del")

	cases := []struct {
		name  string
		flags []string
		want  []int64
	}{
		{"跳过 3 行", []string{"--skip-count", "3"}, []int64{4, 5, 6, 7, 8, 9, 10}},
		{"只读前 4 行", []string{"--row-count", "4"}, []int64{1, 2, 3, 4}},
		{"跳过 2 行取 3 行", []string{"--skip-count", "2", "--row-count", "3"}, []int64{3, 4, 5}},
		{"跳过 2 行取 3 行 + 并行", []string{"--skip-count", "2", "--row-count", "3", "--threads", "4"}, []int64{3, 4, 5}},
		{"跳过 2 行取 3 行 + 小批 + 小提交", []string{"--skip-count", "2", "--row-count", "3", "--batch-size", "1", "--commit-count", "1"}, []int64{3, 4, 5}},
	}

	for _, tc := range cases {
		for _, threads := range []string{"", "--threads"} {
			name := tc.name
			flags := append([]string{}, tc.flags...)
			if threads != "" {
				name += "（并行 2）"
				flags = append(flags, "--threads", "2")
			}
			t.Run(name, func(t *testing.T) {
				dst := env.liveTable(t, "slice_dst")
				env.exec(t, fmt.Sprintf("CREATE TABLE `%s` LIKE `%s`", dst, src))
				env.cliImport(t, dst, "del", file, append(flags, "--commit-count", "5")...)
				got := env.ids(t, dst)
				if fmt.Sprint(got) != fmt.Sprint(tc.want) {
					t.Errorf("imported ids %v, want %v", got, tc.want)
				}
			})
		}
	}
}

// TestOptionMatrixMessages pins what --messages/-m produces: the file is the
// command log itself, in the same records, order and layout the run would have
// printed — not the DB2-style SQL3xxxN report the option used to write — and it
// is the log's only destination, so the terminal stays silent for the whole run.
func TestOptionMatrixMessages(t *testing.T) {
	env := newCLILive(t)
	src, _ := env.makePair(t, "msg")

	cases := []struct {
		name   string
		format string
		flags  []string
		done   string
	}{
		{"单线程 DEL", "del", nil, "export completed"},
		{"单线程 SQL", "sql", nil, "export completed"},
		{"单线程 Parquet", "parquet", nil, "export completed"},
		{"并行 DEL", "del", []string{"--threads", "4"}, "parallel export completed"},
		{"并行 SQL", "sql", []string{"--threads", "4"}, "parallel export completed"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slug := safeFileName(tc.name)
			msg := filepath.Join(env.dir, "msg-"+slug+".log")
			args := []string{"-q", "SELECT * FROM `" + src + "`",
				"-f", tc.format, "-o", filepath.Join(env.dir, "msg-"+slug+"."+tc.format),
				"--messages", msg, "--log-level", "info"}
			args = append(args, tc.flags...)
			out, code := env.runExportCLI(t, args...)
			if code != 0 {
				t.Fatalf("export failed: exit %d\n%s", code, out)
			}

			text := readFile(t, msg)
			lines := splitLines(text)
			if len(lines) == 0 {
				t.Fatalf("message file is empty")
			}
			// The version banner is the first record of a run, so it must be the
			// file's first line: the file starts where the run starts.
			if !strings.Contains(lines[0], "imex starting") || !strings.Contains(lines[0], "command=export") {
				t.Errorf("first line is not the banner: %q", lines[0])
			}
			for _, want := range []string{"export config", tc.done, "rows=10"} {
				if !strings.Contains(text, want) {
					t.Errorf("message file lacks %q:\n%s", want, text)
				}
			}
			if strings.Contains(text, "SQL31") {
				t.Errorf("the DB2-style report is still being written:\n%s", text)
			}
			// Every line is a log record in the default layout, and the file is
			// the run's only account of it: nothing is left on the command line.
			for _, line := range lines {
				if !logRecordLine.MatchString(line) {
					t.Errorf("not a log record: %q", line)
				}
			}
			if strings.TrimSpace(out) != "" {
				t.Errorf("--messages must keep the terminal silent, got:\n%s", out)
			}
		})
	}

	t.Run("短选项-m", func(t *testing.T) {
		msg := filepath.Join(env.dir, "shorthand.log")
		out, code := env.runExportCLI(t, "-q", "SELECT * FROM `"+src+"`",
			"-f", "del", "-o", filepath.Join(env.dir, "shorthand.del"),
			"-m", msg, "--log-level", "info")
		if code != 0 {
			t.Fatalf("export failed: exit %d\n%s", code, out)
		}
		text := readFile(t, msg)
		lines := splitLines(text)
		if len(lines) == 0 || !strings.Contains(lines[0], "imex starting") {
			t.Errorf("-m must open the log file before the banner: %q", text)
		}
		if strings.TrimSpace(out) != "" {
			t.Errorf("-m must keep the terminal silent, got:\n%s", out)
		}
	})

	t.Run("布局跟随log-format", func(t *testing.T) {
		msg := filepath.Join(env.dir, "textfmt.log")
		out, code := env.runExportCLI(t, "-q", "SELECT * FROM `"+src+"`",
			"-f", "del", "-o", filepath.Join(env.dir, "textfmt.del"),
			"--messages", msg, "--log-level", "info", "--log-format", "text")
		if code != 0 {
			t.Fatalf("export failed: exit %d\n%s", code, out)
		}
		text := readFile(t, msg)
		if !strings.Contains(text, `msg="imex starting"`) || !strings.Contains(text, "level=INFO") {
			t.Errorf("--log-format text must reach the file:\n%s", text)
		}
		if strings.Contains(text, "SQL31") {
			t.Errorf("the DB2-style report is still being written:\n%s", text)
		}
	})

	t.Run("级别过滤同样生效", func(t *testing.T) {
		msg := filepath.Join(env.dir, "quiet.log")
		out, code := env.runExportCLI(t, "-q", "SELECT * FROM `"+src+"`",
			"-f", "del", "-o", filepath.Join(env.dir, "quiet.del"),
			"--messages", msg, "--log-level", "error")
		if code != 0 {
			t.Fatalf("export failed: exit %d\n%s", code, out)
		}
		// --log-level applies to the file as it does to stderr, and a successful
		// run has no ERROR record. The one line left is the version banner: it is
		// printed by the root hook before RunE applies --log-level, so it is the
		// only INFO record any level can still see.
		lines := splitLines(readFile(t, msg))
		if len(lines) != 1 || !strings.Contains(lines[0], "imex starting") {
			t.Errorf("--log-level error must leave only the banner, got: %#v", lines)
		}
		// The banner is an INFO record and the run asked for ERROR only, yet it is
		// in the file — the level did not send it to stderr instead. The file is
		// the destination, the level is the filter, and the two are independent.
		if strings.TrimSpace(out) != "" {
			t.Errorf("the file's single line must not also be on the terminal, got:\n%s", out)
		}
	})

	t.Run("失败记录进文件", func(t *testing.T) {
		msg := filepath.Join(env.dir, "failed.log")
		out, code := env.runExportCLI(t,
			"-q", "SELECT * FROM `no_such_table_msg`",
			"-f", "del", "-o", filepath.Join(env.dir, "failed.del"),
			"-m", msg, "--log-level", "info")
		if code == 0 {
			t.Fatalf("export of a missing table must fail\n%s", out)
		}
		text := readFile(t, msg)
		if !strings.Contains(text, "command failed") {
			t.Errorf("the failure must be recorded in the file:\n%s", text)
		}
		if !strings.Contains(text, "no_such_table_msg") {
			t.Errorf("the file must carry the real cause, not just the wrapper:\n%s", text)
		}
		// A failing run is silent too: the exit code is what the operator sees,
		// the file is what the operator reads.
		if strings.TrimSpace(out) != "" {
			t.Errorf("a failed run with -m must still keep the terminal silent, got:\n%s", out)
		}
	})
}

// splitLines turns a log file into records, dropping the trailing newline.
func splitLines(text string) []string {
	text = strings.TrimRight(text, "\r\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// TestOptionMatrixNoOverwrite checks that refusing to overwrite leaves the old
// file untouched on every path. This matters in particular for the parallel
// export, which now streams straight into the final file and would otherwise
// truncate it before discovering the refusal.
func TestOptionMatrixNoOverwrite(t *testing.T) {
	env := newCLILive(t)
	src, _ := env.makePair(t, "nooverwrite")

	for _, threads := range []string{"1", "4"} {
		t.Run("threads="+threads, func(t *testing.T) {
			path := filepath.Join(env.dir, "keep-"+threads+".del")
			const sentinel = "SENTINEL MUST SURVIVE\n"
			if err := os.WriteFile(path, []byte(sentinel), 0o644); err != nil {
				t.Fatalf("write sentinel: %v", err)
			}
			out, code := env.runExportCLI(t, "-q", "SELECT * FROM `"+src+"`",
				"-f", "del", "-o", path, "--threads", threads, "--no-overwrite")
			if code == 0 {
				t.Fatalf("--no-overwrite must refuse an existing output\n%s", out)
			}
			if got := readFile(t, path); got != sentinel {
				t.Errorf("existing output was modified:\n%s", firstLines(got, 2))
			}
			if !strings.Contains(out, "already exists") {
				t.Errorf("expected an 'already exists' message, got:\n%s", out)
			}
		})
	}
}

// TestOptionMatrixCharsets crosses --default-charset with the binary columns and
// with non-ASCII text. Two different guarantees are in play:
//
//   - a byte-safe-on-ASCII charset must round-trip ASCII text and switch binary
//     columns to 0x literals automatically (config.HexBlobForCharset), so the
//     bytes survive the connection;
//   - a non-UTF-8 charset cannot represent every character, so characters the
//     server cannot map are replaced at the protocol level. That is the server's
//     documented conversion, not a tool bug, so the case pins the behaviour
//     instead of demanding equality.
func TestOptionMatrixCharsets(t *testing.T) {
	env := newCLILive(t)

	for _, charset := range []string{"utf8mb4", "latin1", "ascii", "gbk"} {
		t.Run(charset, func(t *testing.T) {
			src := env.liveTable(t, "cs_src")
			dst := env.liveTable(t, "cs_dst")
			env.exec(t, "CREATE TABLE `"+src+"` (id BIGINT PRIMARY KEY, name VARCHAR(50), payload VARBINARY(32)) DEFAULT CHARSET=utf8mb4")
			env.exec(t, "CREATE TABLE `"+dst+"` LIKE `"+src+"`")
			// ASCII text plus binary bytes that are not valid UTF-8 in any charset.
			env.exec(t, "INSERT INTO `"+src+"` VALUES (1,'plain text',0x8081FF),(2,'quote''s',0x0001),(3,NULL,NULL)")

			flags := []string{"--default-charset", charset}
			file := env.cliExport(t, src, "del", "charset_"+charset+".del", flags...)
			env.cliImport(t, dst, "del", file, flags...)

			if got, want := env.csSnapshot(t, dst), env.csSnapshot(t, src); got != want {
				t.Errorf("round-trip with --default-charset %s changed rows:\n got %s\nwant %s", charset, got, want)
			}

			text := readFile(t, file)
			if charset == "utf8mb4" {
				if strings.Contains(text, "0x8081") {
					t.Error("utf8mb4 must keep raw binary bytes, not 0x literals")
				}
			} else if !strings.Contains(text, "0x8081") {
				t.Errorf("--default-charset %s must auto-switch binary columns to 0x literals, file was:\n%s",
					charset, firstLines(text, 12))
			}
		})
	}

	// A charset that cannot map a character replaces it on the wire. The tool
	// cannot undo that, so this case pins the consequence instead of pretending
	// it is lossless: text may degrade, binary must not (0x literals).
	t.Run("非 UTF-8 字符集丢失不可映射字符", func(t *testing.T) {
		src, dst := env.makePair(t, "cs_lossy")
		file := env.cliExport(t, src, "del", "lossy_latin1.del", "--default-charset", "latin1")
		env.cliImport(t, dst, "del", file, "--default-charset", "latin1")

		before, after := env.snapshotTable(t, src), env.snapshotTable(t, dst)
		if len(before) != len(after) {
			t.Fatalf("row count changed: %d before, %d after", len(before), len(after))
		}
		lost := 0
		for i := range before {
			// column 5 (payload, index 5) must survive thanks to 0x literals
			pBefore := strings.Split(before[i], "|")[5]
			pAfter := strings.Split(after[i], "|")[5]
			if pBefore != pAfter {
				t.Errorf("row %d payload changed under latin1: %s -> %s (hex-blob auto-switch broken)",
					i+1, pBefore, pAfter)
			}
			if before[i] != after[i] {
				lost++
			}
		}
		if lost == 0 {
			t.Error("latin1 round-trip unexpectedly preserved every character; if the conversion behaviour changed, update this case")
		}
		t.Logf("latin1 lost the text of %d/%d rows while every binary column survived", lost, len(before))
	})
}

// csSnapshot renders the small charset-matrix table as one comparable string.
func (c *cliEnv) csSnapshot(t *testing.T, table string) string {
	t.Helper()
	rows, err := c.db.Query("SELECT id, HEX(CAST(name AS BINARY)), HEX(payload) FROM `" + table + "` ORDER BY id")
	if err != nil {
		t.Fatalf("snapshot %s: %v", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id int64
		var name, payload sql.NullString
		if err := rows.Scan(&id, &name, &payload); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		out = append(out, fmt.Sprintf("%d|%s|%s", id, name.String, payload.String))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s: %v", table, err)
	}
	return strings.Join(out, " ")
}

// rowCount and ids are small helpers for the slice/mode assertions.
func (c *cliEnv) rowCount(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := c.db.QueryRow("SELECT COUNT(*) FROM `" + table + "`").Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func (c *cliEnv) ids(t *testing.T, table string) []int64 {
	t.Helper()
	rows, err := c.db.Query("SELECT id FROM `" + table + "` ORDER BY id")
	if err != nil {
		t.Fatalf("ids %s: %v", table, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan id: %v", err)
		}
		out = append(out, id)
	}
	return out
}

func firstLines(text string, n int) string {
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = append(lines[:n], "…")
	}
	return strings.Join(lines, "\n")
}
