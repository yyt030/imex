package integration

// Exception-path tests: the failure side of a live import. The happy path is
// pinned elsewhere (option_matrix, data_boundary, roundtrip); this file pins
// what must happen when things go wrong:
//
//   - a hand-written DEL file whose field count drifts (the flat-stream
//     semantics, then a clean table),
//   - a mid-import type error with several batches (everything rolls back),
//   - a repeated primary key arriving through the DEL format (the SQL-format
//     counterpart lives in lock_contention_test.go),
//   - an unreachable server (connection refused, fails fast),
//   - a server that accepts TCP but never answers (bounded timeout, no hang).
//
// Every case runs the built binary through the cliEnv helpers so the exit code
// and the message a user actually sees are asserted, and the target table is
// checked afterwards: a failed import must never leave half the rows behind.
//
// Requires a live server; skipped unless IMEX_TEST_DSN is set:
//
//	CGO_ENABLED=1 IMEX_TEST_DSN='root:pass@tcp(127.0.0.1:3306)/imex_test?parseTime=true' \
//	    go test ./test/integration -run 'ExceptionPath' -v

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeDelFile writes a hand-crafted DEL file so a case controls every byte,
// including deliberately broken ones.
func writeDelFile(t *testing.T, env *cliEnv, name string, lines ...string) string {
	t.Helper()
	path := filepath.Join(env.dir, name)
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// importDelCLI imports a hand-written DEL file without a header, using an
// explicit column list (the only way a headerless file can be imported).
func (c *cliEnv) importDelCLI(t *testing.T, path, table string, extra ...string) (string, int) {
	t.Helper()
	args := []string{"-i", path, "-t", table, "-f", "del", "--header=off",
		"-c", "id,name,val"}
	args = append(args, extra...)
	return c.runImportCLI(t, args...)
}

// exceptionTarget creates the three-column table the exception cases import
// into; the shape matches the hand-written fixtures below.
func (c *cliEnv) exceptionTarget(t *testing.T, prefix string) string {
	t.Helper()
	table := c.liveTable(t, prefix)
	c.exec(t, fmt.Sprintf(
		"CREATE TABLE `%s` (id INT PRIMARY KEY, name VARCHAR(20), val INT)", table))
	return table
}

// TestLive_ExceptionPath_MalformedDelFieldCount pins what a DEL file whose rows
// disagree with the column list does. The DEL reader is a flat field stream: the
// row delimiter is just another field terminator, so a short row does not error
// on its own line — the next line's first field slides into the gap. The case
// pins the observable consequence of that semantics: the run fails with the
// server's own type error for the mis-shifted row, and the target table ends
// empty. It is not a requirement that the reader grows a field-count check, but
// any future check must keep the table clean and keep the error attributable.
func TestLive_ExceptionPath_MalformedDelFieldCount(t *testing.T) {
	env := newCLILive(t)
	dst := env.exceptionTarget(t, "exc_malformed")

	// Row 2 is one field short, so "30" from row 3 fills row 2's val and row 3
	// becomes (gamma, 30, extra) — gamma lands in the INT column and the server
	// rejects the batch.
	path := writeDelFile(t, env, "malformed.del",
		"1,alpha,10",
		"2,beta",
		"3,gamma,30,extra")

	out, code := env.importDelCLI(t, path, dst)
	if code == 0 {
		t.Fatalf("import of a field-count-mismatched file succeeded; want a server type error\n%s", out)
	}
	if !strings.Contains(out, "1366") && !strings.Contains(out, "Incorrect integer value") {
		t.Errorf("exit %d without the mis-shifted row's type error (1366), got:\n%s", code, out)
	}
	if n := countRows(t, env.liveEnv, dst); n != 0 {
		t.Errorf("failed import left %d rows behind; the table must end empty", n)
	}
}

// TestLive_ExceptionPath_MidImportRollback pins the all-or-nothing rule for a
// multi-batch import: with batch-size 1 the first two batches would happily
// land before row 3's type error, but the run must not leave them there.
func TestLive_ExceptionPath_MidImportRollback(t *testing.T) {
	env := newCLILive(t)

	t.Run("中途失败全量回滚", func(t *testing.T) {
		dst := env.exceptionTarget(t, "exc_rollback")

		path := writeDelFile(t, env, "rollback.del",
			"1,alpha,10",
			"2,beta,20",
			"notanint,gamma,30")

		out, code := env.importDelCLI(t, path, dst, "--batch-size", "1")
		if code == 0 {
			t.Fatalf("import with a type error in a later batch succeeded\n%s", out)
		}
		if !strings.Contains(out, "1366") {
			t.Errorf("exit %d without the type error (1366), got:\n%s", code, out)
		}
		if n := countRows(t, env.liveEnv, dst); n != 0 {
			t.Errorf("batches 1 and 2 landed %d rows before batch 3 failed; want a full rollback", n)
		}
	})

	t.Run("同样的合法数据逐批全部落地", func(t *testing.T) {
		dst := env.exceptionTarget(t, "exc_rollback_ok")

		path := writeDelFile(t, env, "rollback_ok.del",
			"1,alpha,10",
			"2,beta,20",
			"3,gamma,30")

		out, code := env.importDelCLI(t, path, dst, "--batch-size", "1")
		if code != 0 {
			t.Fatalf("import of well-formed rows failed: exit %d\n%s", code, out)
		}
		if n := countRows(t, env.liveEnv, dst); n != 3 {
			t.Errorf("landed %d rows, want 3", n)
		}
	})
}

// TestLive_ExceptionPath_DuplicateKeyDelFormat is the DEL-format complement of
// TestLive_DuplicateKeysSingleThread, which drives the SQL format with a
// generated fixture. Here the duplicate arrives through a hand-written DEL
// file: mode insert must surface the server's 1062 and leave the pre-existing
// rows untouched, and mode insert_update must absorb the repeated key.
func TestLive_ExceptionPath_DuplicateKeyDelFormat(t *testing.T) {
	env := newCLILive(t)

	t.Run("mode insert 报 1062 且表保持原状", func(t *testing.T) {
		dst := env.exceptionTarget(t, "exc_dup_ins")
		env.exec(t, fmt.Sprintf("INSERT INTO `%s` VALUES (1, 'alpha', 10)", dst))

		path := writeDelFile(t, env, "dup.del",
			"1,alpha-copy,99",
			"2,beta,20")

		out, code := env.importDelCLI(t, path, dst, "--mode", "insert")
		if code == 0 {
			t.Fatalf("import with a repeated primary key succeeded in mode insert\n%s", out)
		}
		if !strings.Contains(out, "1062") && !strings.Contains(out, "Duplicate entry") {
			t.Errorf("exit %d without 1062/Duplicate entry:\n%s", code, out)
		}
		if n := countRows(t, env.liveEnv, dst); n != 1 {
			t.Errorf("target holds %d rows, want the untouched pre-existing 1", n)
		}
		var name string
		if err := env.db.QueryRow(fmt.Sprintf("SELECT name FROM `%s` WHERE id = 1", dst)).Scan(&name); err != nil {
			t.Fatalf("read back key 1: %v", err)
		}
		if name != "alpha" {
			t.Errorf("pre-existing row changed: name = %q, want %q", name, "alpha")
		}
	})

	t.Run("mode insert_update 吸收重复键", func(t *testing.T) {
		dst := env.exceptionTarget(t, "exc_dup_upd")
		env.exec(t, fmt.Sprintf("INSERT INTO `%s` VALUES (1, 'alpha', 10)", dst))

		path := writeDelFile(t, env, "dup_upd.del",
			"1,alpha-copy,99",
			"2,beta,20")

		out, code := env.importDelCLI(t, path, dst, "--mode", "insert_update")
		if code != 0 {
			t.Fatalf("mode insert_update must absorb the repeated key, exit %d\n%s", code, out)
		}
		if n := countRows(t, env.liveEnv, dst); n != 2 {
			t.Errorf("target holds %d rows, want 2 (updated key 1 + new key 2)", n)
		}
		var name string
		if err := env.db.QueryRow(fmt.Sprintf("SELECT name FROM `%s` WHERE id = 1", dst)).Scan(&name); err != nil {
			t.Fatalf("read back key 1: %v", err)
		}
		if name != "alpha-copy" {
			t.Errorf("key 1 was not updated in place: name = %q, want %q", name, "alpha-copy")
		}
	})
}

// TestLive_ExceptionPath_ConnectionRefusedFailsFast pins the unreachable-server
// behavior: port 2 refuses the dial, the tool retries then reports the dial
// error verbatim and exits non-zero — quickly, not after an unbounded wait.
func TestLive_ExceptionPath_ConnectionRefusedFailsFast(t *testing.T) {
	env := newCLILive(t)

	start := time.Now()
	out, code := env.runExportCLI(t, "-q", "SELECT 1", "-f", "del",
		"-o", filepath.Join(env.dir, "refused.del"),
		"-h", "127.0.0.1", "-P", "2")
	elapsed := time.Since(start)

	if code == 0 {
		t.Fatalf("export against a refusing port succeeded\n%s", out)
	}
	if !strings.Contains(out, "connection refused") {
		t.Errorf("exit %d without the dial error, got:\n%s", code, out)
	}
	if elapsed > 30*time.Second {
		t.Errorf("connection refusal took %s; the retry loop must stay bounded", elapsed)
	}
}

// TestLive_ExceptionPath_UnresponsiveServerTimeout pins the hang safety net:
// a listener that accepts TCP but never answers the handshake must not stall
// the tool. The driver's connect timeout bounds the run; the test pins a
// generous ceiling so slow CI cannot flake, plus the deadline error text.
func TestLive_ExceptionPath_UnresponsiveServerTimeout(t *testing.T) {
	env := newCLILive(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	var mu sync.Mutex
	var conns []net.Conn
	done := make(chan struct{})
	// One cleanup, one order: close the client connections, then the listener
	// (which unblocks the accept loop), then wait for that loop to see it.
	t.Cleanup(func() {
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
		ln.Close()
		<-done
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				close(done)
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			// Accepted but never read from or written to: the client's
			// handshake never completes and the connect timeout fires.
		}
	}()

	start := time.Now()
	out, code := env.runExportCLI(t, "-q", "SELECT 1", "-f", "del",
		"-o", filepath.Join(env.dir, "timeout.del"),
		"-h", "127.0.0.1", "-P", fmt.Sprint(port))
	elapsed := time.Since(start)

	if code == 0 {
		t.Fatalf("export against a silent server succeeded\n%s", out)
	}
	if !strings.Contains(out, "context deadline exceeded") &&
		!strings.Contains(out, "timeout") && !strings.Contains(out, "Timeout") {
		t.Errorf("exit %d without a timeout error, got:\n%s", code, out)
	}
	if elapsed > 30*time.Second {
		t.Errorf("unresponsive server held the run for %s; the connect timeout must bound it", elapsed)
	}
}
