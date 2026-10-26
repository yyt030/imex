package _import

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

// scriptedDriver is a minimal database/sql driver that records execution order
// and injects failures, so the reconnect/replay rules can be tested without a
// live MySQL server.
type scriptedDriver struct {
	mu        sync.Mutex
	execs     []string
	failAt    map[int]error // 1-based Exec index -> error
	commitErr error
	begins    int
	commits   int
	rollbacks int
	opens     int
}

func (d *scriptedDriver) Open(string) (driver.Conn, error) {
	d.mu.Lock()
	d.opens++
	d.mu.Unlock()
	return &scriptedConn{d: d}, nil
}

type scriptedConn struct{ d *scriptedDriver }

func (c *scriptedConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not used by these tests")
}
func (c *scriptedConn) Close() error { return nil }

func (c *scriptedConn) Begin() (driver.Tx, error) {
	c.d.mu.Lock()
	c.d.begins++
	c.d.mu.Unlock()
	return &scriptedTx{d: c.d}, nil
}

func (c *scriptedConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	c.d.execs = append(c.d.execs, query)
	if err := c.d.failAt[len(c.d.execs)]; err != nil {
		return nil, err
	}
	return driver.RowsAffected(1), nil
}

type scriptedTx struct{ d *scriptedDriver }

func (t *scriptedTx) Commit() error {
	t.d.mu.Lock()
	t.d.commits++
	err := t.d.commitErr
	t.d.mu.Unlock()
	return err
}

func (t *scriptedTx) Rollback() error {
	t.d.mu.Lock()
	t.d.rollbacks++
	t.d.mu.Unlock()
	return nil
}

var scriptedCounter int64

func newScriptedDB(t *testing.T) (*sql.DB, *scriptedDriver) {
	t.Helper()
	drv := &scriptedDriver{failAt: map[int]error{}}
	name := fmt.Sprintf("scripted-%d", atomic.AddInt64(&scriptedCounter, 1))
	sql.Register(name, drv)
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db, drv
}

func newTestImporter() *Importer {
	return &Importer{
		cfg:   &config.ImportConfig{Mode: config.ImportModeInsert},
		stats: common.NewStats(),
	}
}

func newTestSession(t *testing.T, commitCount int64) (*writeSession, *scriptedDriver) {
	t.Helper()
	db, drv := newScriptedDB(t)
	sess := newWriteSession(newTestImporter(), db, context.Background(), commitCount, "test")
	sess.policy = common.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond}
	if err := sess.begin(); err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(sess.rollback)
	return sess, drv
}

func TestWriteSessionReplaysOpenTransactionAfterReconnect(t *testing.T) {
	sess, drv := newTestSession(t, 1000)

	if err := sess.write("INSERT A", []interface{}{"a"}, 1); err != nil {
		t.Fatalf("write A: %v", err)
	}
	// The second Exec dies with a connection-level error. MySQL rolls back the
	// open transaction, so batch A must be replayed before batch B is retried.
	drv.failAt[2] = &mysql.MySQLError{Number: 2006, Message: "server has gone away"}
	if err := sess.write("INSERT B", []interface{}{"b"}, 1); err != nil {
		t.Fatalf("write B: %v", err)
	}

	want := []string{"INSERT A", "INSERT B", "INSERT A", "INSERT B"}
	if len(drv.execs) != len(want) {
		t.Fatalf("execs = %v, want %v", drv.execs, want)
	}
	for i := range want {
		if drv.execs[i] != want[i] {
			t.Fatalf("execs = %v, want %v", drv.execs, want)
		}
	}
	if drv.begins != 2 {
		t.Fatalf("begins = %d, want 2 (initial + reconnect)", drv.begins)
	}
	if drv.rollbacks != 1 {
		t.Fatalf("rollbacks = %d, want 1 (dead transaction discarded)", drv.rollbacks)
	}
	if sess.rowsInTx != 2 || len(sess.pending) != 2 {
		t.Fatalf("rowsInTx = %d, pending = %d, want 2/2", sess.rowsInTx, len(sess.pending))
	}
}

func TestWriteSessionDoesNotRetrySemanticErrors(t *testing.T) {
	sess, drv := newTestSession(t, 1000)

	drv.failAt[1] = &mysql.MySQLError{Number: 1064, Message: "syntax error"}
	err := sess.write("INSERT bad", nil, 1)
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(drv.execs) != 1 {
		t.Fatalf("execs = %v, want a single attempt", drv.execs)
	}
	if drv.begins != 1 {
		t.Fatalf("begins = %d, want 1 (no reconnect)", drv.begins)
	}
}

func TestWriteSessionCommitIsNeverRetried(t *testing.T) {
	sess, drv := newTestSession(t, 1000)

	if err := sess.write("INSERT A", []interface{}{"a"}, 1); err != nil {
		t.Fatalf("write: %v", err)
	}
	beginsBefore := drv.begins
	drv.commitErr = &mysql.MySQLError{Number: 2006, Message: "server has gone away"}

	if err := sess.commit(); err == nil {
		t.Fatal("expected commit error")
	}
	if drv.commits != 1 {
		t.Fatalf("commits = %d, want exactly 1 attempt", drv.commits)
	}
	if drv.begins != beginsBefore {
		t.Fatalf("begins = %d, want %d (no reconnect after an ambiguous commit)", drv.begins, beginsBefore)
	}
}

func TestWriteSessionMaybeCommitAccumulatesRows(t *testing.T) {
	sess, drv := newTestSession(t, 1000)

	// Four batches of 300 rows: 1200 rows total. A modulo check on the row
	// counter (rowCount % 1000) would never fire because 300 never divides
	// 1000; the accumulated in-transaction count must.
	for i := 0; i < 4; i++ {
		if err := sess.write("INSERT", []interface{}{i}, 300); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if err := sess.maybeCommit(); err != nil {
			t.Fatalf("maybeCommit %d: %v", i, err)
		}
	}

	if drv.commits != 1 {
		t.Fatalf("commits = %d, want 1 once 1200 rows crossed 1000", drv.commits)
	}
	if drv.begins != 2 {
		t.Fatalf("begins = %d, want 2 (initial + after commit)", drv.begins)
	}
	if sess.rowsInTx != 0 {
		t.Fatalf("rowsInTx = %d, want 0 after the 1200-row transaction committed", sess.rowsInTx)
	}
}

func TestWriteSessionCommitsAtPendingByteBudget(t *testing.T) {
	sess, drv := newTestSession(t, 1000)
	sess.maxPendingBytes = 64

	payload := strings.Repeat("x", 100)
	if err := sess.write("INSERT", []interface{}{payload}, 1); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := sess.maybeCommit(); err != nil {
		t.Fatalf("maybeCommit: %v", err)
	}

	if drv.commits != 1 {
		t.Fatalf("commits = %d, want 1 when the byte budget is crossed", drv.commits)
	}
	if sess.rowsInTx != 0 || len(sess.pending) != 0 || sess.pendingBytes != 0 {
		t.Fatalf("rowsInTx = %d, pending = %d, bytes = %d, want 0/0/0",
			sess.rowsInTx, len(sess.pending), sess.pendingBytes)
	}
	if drv.begins != 2 {
		t.Fatalf("begins = %d, want 2 (initial + after commit)", drv.begins)
	}
}

func TestWriteSessionByteBudgetDoesNotCommitEarly(t *testing.T) {
	sess, drv := newTestSession(t, 1000)
	sess.maxPendingBytes = 4096

	if err := sess.write("INSERT", []interface{}{"small"}, 1); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := sess.maybeCommit(); err != nil {
		t.Fatalf("maybeCommit: %v", err)
	}
	if drv.commits != 0 {
		t.Fatalf("commits = %d, want 0 below both limits", drv.commits)
	}
	if sess.pendingBytes <= 0 {
		t.Fatalf("pendingBytes = %d, want the retained batch accounted for", sess.pendingBytes)
	}
}

func TestWriteSessionIdleCommitOnlyWhenRowsPending(t *testing.T) {
	sess, drv := newTestSession(t, 1000)

	if err := sess.idleCommit(); err != nil {
		t.Fatalf("idleCommit on empty transaction: %v", err)
	}
	if drv.commits != 0 {
		t.Fatalf("commits = %d, want 0 for an empty transaction", drv.commits)
	}

	if err := sess.write("INSERT A", []interface{}{"a"}, 7); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := sess.idleCommit(); err != nil {
		t.Fatalf("idleCommit: %v", err)
	}
	if drv.commits != 1 {
		t.Fatalf("commits = %d, want 1", drv.commits)
	}
	if drv.begins != 2 {
		t.Fatalf("begins = %d, want 2 (idle commit reopens a transaction)", drv.begins)
	}
	if sess.rowsInTx != 0 || len(sess.pending) != 0 {
		t.Fatalf("rowsInTx = %d, pending = %d, want 0/0", sess.rowsInTx, len(sess.pending))
	}
}

func TestWriteSessionReplayFailureIsReported(t *testing.T) {
	sess, drv := newTestSession(t, 1000)

	if err := sess.write("INSERT A", []interface{}{"a"}, 1); err != nil {
		t.Fatalf("write A: %v", err)
	}
	drv.failAt[2] = &mysql.MySQLError{Number: 2013}
	// Replaying A fails with a semantic error, which cannot heal on its own.
	drv.failAt[3] = &mysql.MySQLError{Number: 1064}

	err := sess.write("INSERT B", []interface{}{"b"}, 1)
	if err == nil {
		t.Fatal("expected a replay error")
	}
	if !strings.Contains(err.Error(), "replaying batch 1/1") {
		t.Fatalf("error = %v, want it to name the replay step", err)
	}
	if len(drv.execs) != 3 {
		t.Fatalf("execs = %v, want one failed attempt plus one failed replay", drv.execs)
	}
}
