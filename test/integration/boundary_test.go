package integration

// Boundary behaviour of the runtime options, driven through the real CLI against
// a live server. cmd/cli_matrix_test.go pins which values validation accepts;
// what it cannot see is what the accepted values do once a server is attached,
// because three of the ceilings involved (max_allowed_packet, max_connections and
// the driver's own interpolation ceiling) only exist on the far side of the
// connection. Every case here asserts on the record the run makes about itself,
// plus the exit code the contract promises.

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// driverCeilingBudget mirrors the importer's clamp: the driver will not
// interpolate a statement above 64 MiB, and the batch budget keeps the same 9/10
// margin the server budget uses. Kept as literals here so a change to either side
// fails the assertion instead of quietly agreeing.
const (
	driverCeilingBytes   = int64(64) << 20
	clampedBudgetBytes   = driverCeilingBytes * 9 / 10
	maxQueuedBatchBytesC = int64(1) << 30
)

// defaultCapFactor names whichever ceiling the importer applies when
// --statement-size is off: a server that advertises more than the driver can
// interpolate is clamped, a smaller one is used as is. Tests on both kinds of
// server therefore assert the same record without hard-coding this machine.
func defaultCapFactor(t *testing.T, db *sql.DB) string {
	t.Helper()
	var packet int64
	if err := db.QueryRow("SELECT @@max_allowed_packet").Scan(&packet); err != nil {
		t.Fatalf("read max_allowed_packet: %v", err)
	}
	if packet*9/10 > clampedBudgetBytes {
		return "driver_packet_cap"
	}
	return "max_allowed_packet"
}

func serverMaxAllowedPacket(t *testing.T, env *cliEnv) int64 {
	t.Helper()
	var value int64
	if err := env.db.QueryRow("SELECT @@max_allowed_packet").Scan(&value); err != nil {
		t.Fatalf("read max_allowed_packet: %v", err)
	}
	return value
}

func serverMaxConnections(t *testing.T, env *cliEnv) int64 {
	t.Helper()
	var value int64
	if err := env.db.QueryRow("SELECT @@max_connections").Scan(&value); err != nil {
		t.Fatalf("read max_connections: %v", err)
	}
	return value
}

// A server advertising more than the driver can interpolate used to hand the
// importer a budget it could not use: every batch above 64 MiB left the
// interpolated COM_QUERY path and became a server-side prepared statement, which
// measured slower (2.078s against 1.195s for the same 80 MiB). The default budget
// is now clamped and the clamp is named in the record, so the reasoning is visible
// without reading the code.
func TestBoundaryBatchBudgetIsClampedToTheDriverCeiling(t *testing.T) {
	env := newCLILive(t)
	table := env.liveTable(t, "bound_clamp")
	env.exec(t, "CREATE TABLE `"+table+"` (id INT PRIMARY KEY, v VARCHAR(200))")
	path := writeDEL(t, env.dir, table, 200)

	out, code := env.runImportCLI(t, "-i", path, "-t", table, "--log-level", "info")
	if code != 0 {
		t.Fatalf("import failed: exit %d\n%s", code, out)
	}

	serverBudget := serverMaxAllowedPacket(t, env) * 9 / 10
	line := firstLineContaining(out, "import batch byte cap resolved")
	if line == "" {
		t.Fatalf("no budget record in output:\n%s", out)
	}
	if serverBudget <= clampedBudgetBytes {
		if !strings.Contains(line, "limiting_factor=max_allowed_packet") {
			t.Errorf("a server inside the ceiling must not be clamped: %s", line)
		}
		return
	}
	if !strings.Contains(line, fmt.Sprintf("batch_bytes=%d", clampedBudgetBytes)) {
		t.Errorf("budget not clamped to %d: %s", clampedBudgetBytes, line)
	}
	if !strings.Contains(line, "limiting_factor=driver_packet_cap") {
		t.Errorf("clamp not named in the record: %s", line)
	}
}

// An explicit --statement-size may only lower the budget, never raise it, and the
// clamp on the driver's interpolation ceiling must not rewrite a number the user
// typed. Both halves are asserted on any server: which ceiling binds is read from
// the live server rather than assumed, so the same case runs on a small
// max_allowed_packet (an OceanBase tenant at its 16 MiB default) and on a 128 MB
// MariaDB without either bed skipping the other's half.
func TestBoundaryExplicitStatementSizeIsLoweredOnly(t *testing.T) {
	env := newCLILive(t)
	table := env.liveTable(t, "bound_above")
	env.exec(t, "CREATE TABLE `"+table+"` (id INT PRIMARY KEY, v VARCHAR(200))")
	path := writeDEL(t, env.dir, table, 200)

	packet := serverMaxAllowedPacket(t, env)
	packetBudget := packet * 9 / 10

	// Half one: a cap inside the server budget is taken verbatim, named as the
	// limiter, and is not the ceiling the driver complains about. A quarter of the
	// budget is inside it on every bed that can be imported into at all, and is kept
	// under the driver ceiling so the absence of the warning is meaningful.
	inside := packetBudget / 4
	if inside > clampedBudgetBytes/2 {
		inside = clampedBudgetBytes / 2
	}
	out, code := env.runImportCLI(t, "-i", path, "-t", table, "--log-level", "info",
		"--statement-size", fmt.Sprint(inside))
	if code != 0 {
		t.Fatalf("import with an explicit in-budget cap failed: exit %d\n%s", code, out)
	}
	line := firstLineContaining(out, "import batch byte cap resolved")
	for _, wantField := range []string{fmt.Sprintf("batch_bytes=%d", inside), "limiting_factor=statement-size"} {
		if !strings.Contains(line, wantField) {
			t.Errorf("an in-budget explicit cap was not honoured (%s missing from %s)", wantField, line)
		}
	}
	if strings.Contains(out, "above the driver interpolation ceiling") {
		t.Errorf("a cap below the driver ceiling was reported as if it were above it:\n%s", out)
	}

	// Half two: the largest cap the CLI accepts must never raise the budget above
	// whichever ceiling governs this server. Which one governs depends only on the
	// live value, so all three branches are the documented contract and the run
	// asserts the one its own server implies.
	const maxLegal = 268435456
	second := env.liveTable(t, "bound_above2")
	env.exec(t, "CREATE TABLE `"+second+"` (id INT PRIMARY KEY, v VARCHAR(200))")
	out, code = env.runImportCLI(t, "-i", path, "-t", second, "--log-level", "info",
		"--statement-size", fmt.Sprint(maxLegal))
	if code != 0 {
		t.Fatalf("import with the largest legal explicit cap failed: exit %d\n%s", code, out)
	}
	line = firstLineContaining(out, "import batch byte cap resolved")
	warned := strings.Contains(out, "above the driver interpolation ceiling")
	switch {
	case packetBudget > maxLegal:
		// A server advertising more than the CLI will accept: the typed number is in
		// force, it is above the driver ceiling, and that has to be said out loud.
		for _, wantField := range []string{fmt.Sprintf("batch_bytes=%d", maxLegal), "limiting_factor=statement-size"} {
			if !strings.Contains(line, wantField) {
				t.Errorf("largest legal cap not honoured (%s missing from %s)", wantField, line)
			}
		}
		if !warned {
			t.Fatalf("an explicit cap above the driver ceiling went unreported:\n%s", out)
		}
		if !strings.Contains(out, fmt.Sprintf("driver_ceiling_bytes=%d", clampedBudgetBytes)) {
			t.Errorf("the report does not name the ceiling it warns about: %s", warnNeedle(out))
		}
	case packetBudget > clampedBudgetBytes:
		// The driver ceiling binds before the typed number is ever consulted.
		if !strings.Contains(line, fmt.Sprintf("batch_bytes=%d", clampedBudgetBytes)) ||
			!strings.Contains(line, "limiting_factor=driver_packet_cap") {
			t.Errorf("budget not resolved to the driver ceiling: %s", line)
		}
		if warned {
			t.Errorf("the typed cap never came into force, so the warn is wrong: %s", warnNeedle(out))
		}
	default:
		// The server's own budget is the smallest ceiling and the cap cannot raise it.
		if strings.Contains(line, "limiting_factor=statement-size") {
			t.Errorf("an explicit cap above the server budget raised it: %s", line)
		}
		if !strings.Contains(line, fmt.Sprintf("batch_bytes=%d", packetBudget)) {
			t.Errorf("budget should stay at the server's %d: %s", packetBudget, line)
		}
		if warned {
			t.Errorf("the driver is not what binds on this server, so the warn is wrong: %s", warnNeedle(out))
		}
	}
}

// warnNeedle returns the driver-ceiling warning line, for error messages that have
// to show it rather than the whole run.
func warnNeedle(out string) string {
	return firstLineContaining(out, "above the driver interpolation ceiling")
}

// --pipeline-buffer counts batches and one batch may be as large as the byte budget,
// so 1024 batches is a memory request as much as a queue setting. Priced on a
// 200,688,900-byte / 100,000-row import at --threads 32, three repeats each, on a
// server reporting the OceanBase V4.x default max_allowed_packet=16M: depth 1024
// peaked at 76,616-152,040 KB and depth 10 at 122,740-149,412 KB — the ranges
// overlap because a 15 MB batch is too small for depth to dominate RSS. On the
// earlier 128 MB bed the same file did separate (147,340-292,356 KB at depth 1024),
// and the 280,288 KB figure predates the removal of the depth clamp and no longer
// reproduces. Depth and --threads are separate knobs — a queue and a concurrency
// level, and the queue is shared by the workers — so the run imports the depth it
// was given and reports what it costs instead of converting it.
func TestBoundaryPipelineBufferIsHonouredAndPriced(t *testing.T) {
	env := newCLILive(t)
	table := env.liveTable(t, "bound_queue")
	env.exec(t, "CREATE TABLE `"+table+"` (id INT PRIMARY KEY, v VARCHAR(200))")
	path := writeDEL(t, env.dir, table, 200)

	out, code := env.runImportCLI(t, "-i", path, "-t", table, "--log-level", "info",
		"--pipeline-buffer", "1024")
	if code != 0 {
		t.Fatalf("import failed: exit %d\n%s", code, out)
	}

	// The configured depth is what the run used: echoed as given, and no record
	// claims it was changed.
	if !strings.Contains(out, "pipeline_buffer=1024") {
		t.Errorf("the configuration record does not echo the requested depth:\n%s", out)
	}
	if strings.Contains(out, "lowering --pipeline-buffer") {
		t.Errorf("the depth was converted rather than reported:\n%s", out)
	}

	// The price has to be the product of the depth the run used and the budget it
	// resolved, both read back out of this run rather than assumed here.
	batchBytes := logField(t, out, "import batch byte cap resolved", "batch_bytes")
	record := firstLineContaining(out, "hand-off queue may retain more than the memory envelope")
	if int64(1024)*batchBytes <= maxQueuedBatchBytesC {
		// A small server budget keeps the requested depth inside the envelope, so
		// there is nothing to price and nothing to say.
		if record != "" {
			t.Errorf("a depth inside the envelope was reported anyway: %s", record)
		}
		return
	}
	if record == "" {
		t.Fatalf("the cost of the depth was not reported:\n%s", out)
	}
	for _, want_text := range []string{
		"requested=1024",
		fmt.Sprintf("batch_bytes=%d", batchBytes),
		fmt.Sprintf("queued_bytes=%d", int64(1024)*batchBytes),
		fmt.Sprintf("envelope_bytes=%d", maxQueuedBatchBytesC),
		"lower --pipeline-buffer",
	} {
		if !strings.Contains(record, want_text) {
			t.Errorf("envelope record lacks %q: %s", want_text, record)
		}
	}
}

// logField reads one numeric field out of the first record carrying needle.
func logField(t *testing.T, out, needle, field string) int64 {
	t.Helper()
	line := firstLineContaining(out, needle)
	if line == "" {
		t.Fatalf("no record containing %q:\n%s", needle, out)
	}
	m := regexp.MustCompile(`\b` + field + `=(\d+)`).FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("record %q carries no %s:\n%s", line, field, out)
	}
	value, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatalf("parse %s from %q: %v", field, line, err)
	}
	return value
}

// The thread bound (1..256) says nothing about the target server. Past its
// max_connections the extra workers retry and then fail the run *after* other
// workers have committed, so the mismatch is reported before any data moves — a
// warning, not a refusal, because the DBA may well raise the limit and the run is
// still correct if the connections are available.
func TestBoundaryWorkersOverConnectionLimitAreReported(t *testing.T) {
	env := newCLILive(t)
	table := env.liveTable(t, "bound_conns")
	env.exec(t, "CREATE TABLE `"+table+"` (id INT PRIMARY KEY, v VARCHAR(200))")
	path := writeDEL(t, env.dir, table, 50)

	const threads = 200
	if serverMaxConnections(t, env) >= threads+1 {
		t.Skipf("server accepts %d connections, the over-limit case cannot be produced", threads+1)
	}

	out, _ := env.runImportCLI(t, "-i", path, "-t", table, "--log-level", "info",
		"--threads", fmt.Sprint(threads))
	warn := firstLineContaining(out, "workers exceed the server connection limit")
	if warn == "" {
		t.Fatalf("the mismatch was not reported:\n%s", out)
	}
	for _, want_text := range []string{fmt.Sprintf("threads=%d", threads), "connections_needed=201", "max_connections="} {
		if !strings.Contains(warn, want_text) {
			t.Errorf("warning lacks %q: %s", want_text, warn)
		}
	}
}

// A parallel import whose workers fail while the reader is still producing must
// report an ordinary import failure. It used to crash instead: errCh was closed
// after the *workers* finished, while the reader goroutine was outside the same
// WaitGroup and still sent to it, so the run died with "panic: send on closed
// channel" — exit code 2 (a usage class), a truncated log, and rows already
// committed. The reader is now in the group, so the channel can only close once
// both stages are done.
// The fixture table name deliberately avoids the words below: they are the only
// markers that mean a crash, and a substring match against a name would fire on
// the log lines that merely mention the table.
func TestBoundaryParallelImportFailureExitsOneWithoutPanic(t *testing.T) {
	env := newCLILive(t)
	table := env.liveTable(t, "bound_abort")
	env.exec(t, "CREATE TABLE `"+table+"` (id INT PRIMARY KEY, v VARCHAR(200))")

	// 20,000 rows with a duplicate primary key at row 10,000: the failure lands
	// while the reader still has half the file to go, which is the window the
	// panic needed.
	const rows = 20000
	path := filepath.Join(env.dir, table+".del")
	var b strings.Builder
	b.WriteString("id,v\n")
	for i := 1; i <= rows; i++ {
		id := i
		if i == rows/2 {
			id = 1
		}
		fmt.Fprintf(&b, "%d,value-%d\n", id, i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	for attempt := 1; attempt <= 3; attempt++ {
		env.exec(t, "TRUNCATE TABLE `"+table+"`")
		out, code := env.runImportCLI(t, "-i", path, "-t", table, "--log-level", "info",
			"--threads", "4", "--pipeline-buffer", "0", "--commit-count", "100000")
		for _, crash := range []string{"panic:", "send on closed channel", "\ngoroutine "} {
			if strings.Contains(out, crash) {
				t.Fatalf("attempt %d crashed (%q) instead of reporting the failure:\n%s", attempt, crash, out)
			}
		}
		if code != 1 {
			t.Fatalf("attempt %d: exit %d, want 1\n%s", attempt, code, out)
		}
		if !strings.Contains(out, "insert batch failed") {
			t.Errorf("attempt %d: no worker failure record:\n%s", attempt, out)
		}
		if !strings.Contains(out, "command failed") {
			t.Errorf("attempt %d: the failure was not escalated as the command result:\n%s", attempt, out)
		}
	}
}

// --shard-key naming a column the table does not have used to log a warning and
// export single-threaded: the run reported success at a fraction of the speed it
// was asked for. An option the user typed explicitly is a request, so refusing it
// is the honest answer. Auto-detection keeps its fallback.
func TestBoundaryExplicitShardKeyIsRefused(t *testing.T) {
	env := newCLILive(t)
	table := env.liveTable(t, "bound_shard")
	env.exec(t, "CREATE TABLE `"+table+"` (id INT PRIMARY KEY, v VARCHAR(200))")
	env.exec(t, "INSERT INTO `"+table+"` (id, v) VALUES (1, 'a'), (2, 'b')")

	out, code := env.runExportCLI(t, "-q", "SELECT * FROM `"+table+"`",
		"-o", filepath.Join(env.dir, table+".del"), "--threads", "2", "--shard-key", "nosuchcol")
	if code != 2 {
		t.Fatalf("exit %d, want 2 (usage error)\n%s", code, out)
	}
	// slog escapes the quotes inside the error string, so the assertion is on the
	// option name and the value separately rather than on the rendered phrase.
	for _, want_text := range []string{"invalid --shard-key", "nosuchcol", table} {
		if !strings.Contains(out, want_text) {
			t.Errorf("refusal does not name %q: %s", want_text, out)
		}
	}

	// Without --shard-key the auto-detect path still has to work.
	out, code = env.runExportCLI(t, "-q", "SELECT * FROM `"+table+"`",
		"-o", filepath.Join(env.dir, table+"_auto.del"), "--threads", "2")
	if code != 0 {
		t.Fatalf("auto-detected shard key export failed: exit %d\n%s", code, out)
	}
}

// The other half of the same option: a shard key the run cannot use at all. The
// single-threaded path never samples one, so it used to export happily while the
// configuration record echoed shard_key=nosuchcol as if the run applied it — and the
// column was not even checked, because only the sharded path validates it. The run is
// still correct, so the answer is a WARN naming the option and a record field saying
// the value has no effect, not a refusal.
func TestBoundarySingleThreadedExportNamesAnIgnoredShardKey(t *testing.T) {
	env := newCLILive(t)
	table := env.liveTable(t, "bound_shard1")
	env.exec(t, "CREATE TABLE `"+table+"` (id INT PRIMARY KEY, v VARCHAR(200))")
	env.exec(t, "INSERT INTO `"+table+"` (id, v) VALUES (1, 'a'), (2, 'b')")
	// Enough rows that --threads 2 really shards, so the parallel route below is
	// observed doing what the option asks rather than falling back by accident.
	for i := 0; i < 8; i++ {
		var n int64
		if err := env.db.QueryRow("SELECT COUNT(*) FROM `" + table + "`").Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		env.exec(t, "INSERT INTO `"+table+"` (id, v) SELECT id + ?, v FROM `"+table+"`", n)
	}
	const ignoreRecord = "single-threaded export ignores --shard-key"

	// Route 1: --threads 1 with a column that does not exist. Exit 0, but the run
	// has to say the key is not used, on both the warning and the record.
	out, code := env.runExportCLI(t, "-q", "SELECT * FROM `"+table+"`",
		"-o", filepath.Join(env.dir, table+"_single.del"), "--threads", "1", "--shard-key", "nosuchcol",
		"--log-level", "info")
	if code != 0 {
		t.Fatalf("--threads 1 with an unusable shard key exited %d, want 0\n%s", code, out)
	}
	for _, want_text := range []string{ignoreRecord, "shard_key=nosuchcol", "threads=1", "fallback=false", "shard_key_ignored=true"} {
		if !strings.Contains(out, want_text) {
			t.Errorf("single-threaded run does not report %q:\n%s", want_text, out)
		}
	}

	// Route 2: a parallel request that falls back. The record was written while
	// threads>1 still looked usable, so only the warning carries the truth here.
	out, code = env.runExportCLI(t, "-q", "SELECT a.id FROM `"+table+"` a JOIN `"+table+"` b ON a.id=b.id LIMIT 10",
		"-o", filepath.Join(env.dir, table+"_join.del"), "--threads", "4", "--shard-key", "id",
		"--log-level", "info")
	if code != 0 {
		t.Fatalf("fallback export exited %d, want 0\n%s", code, out)
	}
	for _, want_text := range []string{"falling back to single-threaded", ignoreRecord, "threads=4", "fallback=true"} {
		if !strings.Contains(out, want_text) {
			t.Errorf("fallback run does not report %q:\n%s", want_text, out)
		}
	}
	if strings.Contains(out, "shard_key_ignored") {
		t.Errorf("fallback record claims the key was unusable before the run decided so:\n%s", out)
	}

	// Route 3: the parallel path, where the option is honoured. Nothing may be
	// printed about ignoring it, and no ignored marker in the record.
	out, code = env.runExportCLI(t, "-q", "SELECT * FROM `"+table+"`",
		"-o", filepath.Join(env.dir, table+"_par.del"), "--threads", "2", "--shard-key", "id",
		"--log-level", "info")
	if code != 0 {
		t.Fatalf("parallel export with a valid shard key exited %d\n%s", code, out)
	}
	if strings.Contains(out, ignoreRecord) || strings.Contains(out, "shard_key_ignored") {
		t.Errorf("parallel export reported the shard key as ignored:\n%s", out)
	}
	if !strings.Contains(out, "shard_key=id") {
		t.Errorf("parallel export record does not carry the shard key it used:\n%s", out)
	}
}

// writeDEL writes a two-column DEL fixture with `rows` rows of predictable width.
func writeDEL(t *testing.T, dir, name string, rows int) string {
	t.Helper()
	path := filepath.Join(dir, name+".del")
	var b strings.Builder
	b.WriteString("id,v\n")
	for i := 1; i <= rows; i++ {
		fmt.Fprintf(&b, "%d,%s\n", i, strings.Repeat("x", 64))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func firstLineContaining(text, needle string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}
