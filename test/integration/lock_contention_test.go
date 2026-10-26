package integration

// Import concurrency vs. duplicate primary keys.
//
// The parallel import path is one reader goroutine feeding a bounded channel and
// N workers that each take whichever batch is next
// (internal/import/importer.go:808-918). Nothing keeps two batches that carry the
// same primary keys on the same connection, so a file that repeats a key set in a
// *different order* in a neighbouring batch is a lock-order inversion, and InnoDB
// resolves a lock-order inversion the only way it can: deadlock (error 1213),
// which rolls back the whole victim transaction.
//
// DEL and SQL both reach this through the same pipeline - the format only decides
// how rows are parsed - so the parallel cases run one fixture through both.
//
// What is pinned here, in order of how much it costs to lose:
//
//   - a single-threaded import can never report a deadlock: one session applies
//     one statement at a time, so a repeated key is the plain constraint error
//     1062. This is the deterministic half of the contract.
//   - whatever a parallel run does, it must never land a duplicate row:
//     COUNT(*) == COUNT(DISTINCT id) holds for every exit code.
//   - when the server reports contention the run fails *with that error* instead
//     of swallowing it. 1213/1205 are deliberately absent from the transient set
//     (internal/common/dberr.go:21-24, pinned by common's dberr_test.go), so a
//     deadlock is never replayed into duplicated data.
//   - the mitigation is real, and measured: the same key sets, ordered
//     consistently across batches, import in parallel without a single deadlock.
//
// Whether a given inverted run actually deadlocks depends on how the server
// interleaves the two statements, so that outcome is reported, not required; what
// is required is that the only failure it can produce is a lock error.
//
// Requires a live server; skipped unless IMEX_TEST_DSN is set:
//
//	CGO_ENABLED=1 IMEX_TEST_DSN='<user>:<password>@tcp(127.0.0.1:3306)/imex_test?parseTime=true' \
//	  go test -count=1 ./test/integration -run 'DuplicateKeys|KeyOrder' -v

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// dupKeyFixture builds a source table of groups*batchRows rows in which every key
// appears exactly twice: block b carries the key range of group b/2, ascending in
// the even block and, when invert is set, descending in the odd one. With
// --batch-size set to batchRows each block becomes exactly one write batch, so
// the two copies of a key range land in two adjacent batches - which is what
// hands them to two different workers.
//
// It returns the table name and the two export files (sql, del) of the same rows
// in the same order, so a case only has to pick a format.
func (c *cliEnv) dupKeyFixture(t *testing.T, prefix string, batchRows, groups int, invert bool) (src, sqlPath, delPath string) {
	t.Helper()

	total := batchRows * groups
	if total < 2*batchRows || total > 10000 {
		t.Fatalf("fixture needs 2*batchRows <= total <= 10000, got %d", total)
	}

	src = c.liveTable(t, prefix+"_src")
	digits := c.liveTable(t, prefix+"_digits")
	c.exec(t, "CREATE TABLE `"+src+"` (seq BIGINT NOT NULL PRIMARY KEY, id BIGINT NOT NULL, name VARCHAR(64) NOT NULL) DEFAULT CHARSET=utf8mb4")
	c.exec(t, "CREATE TABLE `"+digits+"` (n INT NOT NULL PRIMARY KEY)")
	c.exec(t, "INSERT INTO `"+digits+"` VALUES (0),(1),(2),(3),(4),(5),(6),(7),(8),(9)")

	// The offset inside a block: the block position, or its mirror for the second
	// copy when the order is inverted.
	offset := fmt.Sprintf("((s.seq - 1) MOD %d)", batchRows)
	if invert {
		offset = fmt.Sprintf(
			"(CASE WHEN (((s.seq - 1) DIV %d) MOD 2) = 0 THEN ((s.seq - 1) MOD %d) ELSE %d - 1 - ((s.seq - 1) MOD %d) END)",
			batchRows, batchRows, batchRows, batchRows)
	}
	c.exec(t, fmt.Sprintf(
		"INSERT INTO `%s` (seq, id, name) "+
			"SELECT s.seq, (((s.seq - 1) DIV %d) DIV 2 * %d) + 1 + %s, CONCAT('n', s.seq) "+
			"FROM (SELECT (d1.n + d2.n*10 + d3.n*100 + d4.n*1000 + 1) AS seq "+
			"FROM `%s` d1, `%s` d2, `%s` d3, `%s` d4) s WHERE s.seq <= %d",
		src, batchRows, batchRows, offset, digits, digits, digits, digits, total))

	var distinct, rows int64
	if err := c.db.QueryRow("SELECT COUNT(DISTINCT id), COUNT(*) FROM `"+src+"`").Scan(&distinct, &rows); err != nil {
		t.Fatalf("count fixture %s: %v", src, err)
	}
	if rows != int64(total) || distinct != int64(total/2) {
		t.Fatalf("fixture %s: %d rows / %d distinct ids, want %d / %d", src, rows, distinct, total, total/2)
	}

	query := "SELECT id, name FROM `" + src + "` ORDER BY seq"
	sqlPath = filepath.Join(c.dir, prefix+".sql")
	if out, code := c.runExportCLI(t, "-q", query, "-f", "sql", "-o", sqlPath); code != 0 {
		t.Fatalf("export %s as sql: exit %d\n%s", src, code, out)
	}
	delPath = filepath.Join(c.dir, prefix+".del")
	if out, code := c.runExportCLI(t, "-q", query, "-f", "del", "-o", delPath); code != 0 {
		t.Fatalf("export %s as del: exit %d\n%s", src, code, out)
	}
	return src, sqlPath, delPath
}

// dupTarget is the table the imports write into: one primary key and one payload
// column, matching the fixture's projection.
func (c *cliEnv) dupTarget(t *testing.T, prefix string) string {
	t.Helper()
	name := c.liveTable(t, prefix+"_dst")
	c.exec(t, "CREATE TABLE `"+name+"` (id BIGINT NOT NULL PRIMARY KEY, name VARCHAR(64) NOT NULL) DEFAULT CHARSET=utf8mb4")
	return name
}

// keyCounts reports how many rows landed and how many distinct keys they carry.
// Their equality is the no-duplicate-rows invariant: a batch that is applied twice
// (a replay after a rolled-back transaction, for instance) shows up as rows >
// distinct immediately.
func (c *cliEnv) keyCounts(t *testing.T, table string) (rows, distinct int64) {
	t.Helper()
	if err := c.db.QueryRow("SELECT COUNT(*), COUNT(DISTINCT id) FROM `"+table+"`").Scan(&rows, &distinct); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return rows, distinct
}

// deadlockCounter reads the server's cumulative deadlock count. MySQL exposes it
// as Innodb_row_lock_deadlocks and MariaDB as Innodb_deadlocks; an account
// without SHOW GLOBAL STATUS privileges, or an engine with neither, reports
// nothing and returns ok == false - the callers treat it as evidence, never as a
// requirement.
func (c *cliEnv) deadlockCounter(t *testing.T) (int64, bool) {
	t.Helper()
	for _, name := range []string{"Innodb_row_lock_deadlocks", "Innodb_deadlocks"} {
		var key, value string
		if err := c.db.QueryRow("SHOW GLOBAL STATUS LIKE '"+name+"'").Scan(&key, &value); err != nil {
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			t.Fatalf("status %s = %q: %v", name, value, err)
		}
		return n, true
	}
	return 0, false
}

// runDupImport imports one of the fixture files with the given flags.
func (c *cliEnv) runDupImport(t *testing.T, format, path, table string, extra ...string) (string, int) {
	t.Helper()
	args := append([]string{"-i", path, "-t", table, "-f", format}, extra...)
	return c.runImportCLI(t, args...)
}

// TestLive_DuplicateKeysSingleThread is the deterministic half of the contract:
// with one writer session a repeated primary key is a constraint error, never a
// deadlock, and --mode insert_update gets the whole key set in.
func TestLive_DuplicateKeysSingleThread(t *testing.T) {
	env := newCLILive(t)

	const batchRows, groups = 40, 6
	_, sqlPath, _ := env.dupKeyFixture(t, "dupst", batchRows, groups, true)
	wantIDs := batchRows * groups / 2

	t.Run("mode insert 报约束错误", func(t *testing.T) {
		dst := env.dupTarget(t, "dupst_ins")
		out, code := env.runDupImport(t, "sql", sqlPath, dst, "--mode", "insert", "--batch-size", strconv.Itoa(batchRows))
		if code == 0 {
			t.Fatalf("import of a file with repeated primary keys succeeded in mode insert; want the constraint error\n%s", out)
		}
		if !strings.Contains(out, "1062") && !strings.Contains(out, "Duplicate entry") {
			t.Errorf("exit %d without a duplicate-key error, want 1062/Duplicate entry:\n%s", code, out)
		}
		// The hazard under test only exists with more than one session. If this
		// message ever becomes a deadlock, the single-threaded path has started to
		// hold more than one transaction open.
		if strings.Contains(out, "1213") || strings.Contains(out, "Deadlock") {
			t.Errorf("single-threaded import reported a deadlock, want 1062:\n%s", out)
		}
		rows, distinct := env.keyCounts(t, dst)
		if rows != distinct {
			t.Errorf("landed %d rows over %d distinct keys: a duplicate row was written", rows, distinct)
		}
	})

	t.Run("mode insert_update 全部键落地", func(t *testing.T) {
		dst := env.dupTarget(t, "dupst_upd")
		out, code := env.runDupImport(t, "sql", sqlPath, dst, "--mode", "insert_update", "--batch-size", strconv.Itoa(batchRows))
		if code != 0 {
			t.Fatalf("mode insert_update must absorb the repeated keys, exit %d\n%s", code, out)
		}
		rows, distinct := env.keyCounts(t, dst)
		if rows != distinct {
			t.Fatalf("landed %d rows over %d distinct keys", rows, distinct)
		}
		if rows != int64(wantIDs) {
			t.Errorf("landed %d rows, want %d (one row per distinct key)", rows, wantIDs)
		}
	})
}

// TestLive_ParallelImportKeyOrderInversion puts the two copies of each key range
// in adjacent batches with opposite orders and imports with four workers, in both
// formats. The run may or may not deadlock depending on server scheduling, so the
// assertions are the ones that must hold either way: no duplicate row ever lands,
// and the only failure the tool reports is the server's own lock error - not a
// retry, not a replay, not a generic write failure.
func TestLive_ParallelImportKeyOrderInversion(t *testing.T) {
	env := newCLILive(t)

	const batchRows, groups = 40, 20
	_, sqlPath, delPath := env.dupKeyFixture(t, "dupinv", batchRows, groups, true)
	wantIDs := batchRows * groups / 2
	batch := strconv.Itoa(batchRows)

	cases := []struct {
		format string
		path   string
	}{
		{"sql", sqlPath},
		{"del", delPath},
	}
	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			dst := env.dupTarget(t, "dupinv_"+tc.format)
			before, ok := env.deadlockCounter(t)
			out, code := env.runDupImport(t, tc.format, tc.path, dst,
				"--mode", "insert_update", "--threads", "4",
				"--batch-size", batch, "--pipeline-buffer", "0")
			after, okAfter := env.deadlockCounter(t)
			delta := after - before
			if ok && okAfter {
				t.Logf("server deadlocks during run: %d", delta)
			} else {
				t.Log("server deadlock counter unavailable; relying on the reported error only")
			}

			rows, distinct := env.keyCounts(t, dst)
			if rows != distinct {
				t.Fatalf("landed %d rows over %d distinct keys: a duplicate row was written\n%s", rows, distinct, out)
			}
			if rows > int64(wantIDs) {
				t.Fatalf("landed %d rows, more than the %d keys the file carries", rows, wantIDs)
			}

			if code == 0 {
				if rows != int64(wantIDs) {
					t.Fatalf("import reported success but landed %d of %d keys", rows, wantIDs)
				}
				t.Logf("no lock cycle this run: %d keys landed, deadlock delta %d", rows, delta)
				return
			}

			// Aborted: it has to be the server's contention error, surfaced as-is.
			if !strings.Contains(out, "1213") && !strings.Contains(out, "Deadlock") &&
				!strings.Contains(out, "1205") && !strings.Contains(out, "Lock wait timeout") {
				t.Errorf("parallel import of an inverted key order failed with something other than a lock error (exit %d):\n%s", code, out)
			}
			// A deadlock rolls back the victim transaction, so the surviving rows
			// are the other workers' committed ones: fewer than the file's keys.
			t.Logf("aborted on lock contention: %d of %d keys landed, deadlock delta %d", rows, wantIDs, delta)
		})
	}
}

// TestLive_ParallelImportConsistentKeyOrder is the control that makes the
// mitigation a tested property rather than advice: the same key sets, the same
// four workers, but every batch in ascending key order. Concurrent sessions then
// queue on the same rows in the same order, which cannot form a cycle, so the run
// must complete with every key landed and no deadlock counted.
func TestLive_ParallelImportConsistentKeyOrder(t *testing.T) {
	env := newCLILive(t)

	const batchRows, groups = 40, 20
	_, sqlPath, _ := env.dupKeyFixture(t, "dupasc", batchRows, groups, false)
	wantIDs := batchRows * groups / 2

	dst := env.dupTarget(t, "dupasc")
	before, ok := env.deadlockCounter(t)
	out, code := env.runDupImport(t, "sql", sqlPath, dst,
		"--mode", "insert_update", "--threads", "4",
		"--batch-size", strconv.Itoa(batchRows), "--pipeline-buffer", "0")
	if code != 0 {
		t.Fatalf("consistent key order must import in parallel without contention errors, exit %d\n%s", code, out)
	}
	if after, okAfter := env.deadlockCounter(t); ok && okAfter && after != before {
		t.Errorf("deadlocks went from %d to %d with every batch locked in the same order", before, after)
	}

	rows, distinct := env.keyCounts(t, dst)
	if rows != distinct {
		t.Fatalf("landed %d rows over %d distinct keys: a duplicate row was written", rows, distinct)
	}
	if rows != int64(wantIDs) {
		t.Errorf("landed %d rows, want %d (one row per distinct key)", rows, wantIDs)
	}
}
