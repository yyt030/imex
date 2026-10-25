package export

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/errors"

	"github.com/gomydump/internal/common"
	"github.com/gomydump/internal/config"
)

// fakeShard is an in-process source for exportShard: one text column, a fixed
// number of rows, and a record of the moment database/sql closes the connection.
// It exists so a test can observe the order in which a shard gives up its
// connection and hands over its tail bytes — the same order that otherwise only
// shows up through SHOW PROCESSLIST on a live server.
type fakeShard struct {
	rows   int
	closed chan struct{}
	once   sync.Once
}

func (f *fakeShard) markClosed() { f.once.Do(func() { close(f.closed) }) }

type fakeShardConnector struct{ f *fakeShard }

func (c fakeShardConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeShardConn{f: c.f}, nil
}

func (c fakeShardConnector) Driver() driver.Driver { return fakeShardDriver{} }

type fakeShardDriver struct{}

func (fakeShardDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("fake shard driver: dial through the connector")
}

type fakeShardConn struct{ f *fakeShard }

func (c *fakeShardConn) Prepare(string) (driver.Stmt, error) { return &fakeShardStmt{f: c.f}, nil }
func (c *fakeShardConn) Close() error                        { c.f.markClosed(); return nil }
func (c *fakeShardConn) Begin() (driver.Tx, error) {
	return nil, errors.New("fake shard conn: no transactions")
}

type fakeShardStmt struct{ f *fakeShard }

func (s *fakeShardStmt) Close() error  { return nil }
func (s *fakeShardStmt) NumInput() int { return -1 }

func (s *fakeShardStmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("fake shard stmt: exec is not supported")
}

func (s *fakeShardStmt) Query([]driver.Value) (driver.Rows, error) {
	return &fakeShardRows{n: s.f.rows}, nil
}

type fakeShardRows struct{ n int }

func (r *fakeShardRows) Columns() []string { return []string{"note"} }
func (r *fakeShardRows) Close() error      { return nil }

func (r *fakeShardRows) Next(dest []driver.Value) error {
	if r.n == 0 {
		return io.EOF
	}
	r.n--
	dest[0] = []byte(strings.Repeat("x", 32))
	return nil
}

func newFakeShardExporter(t *testing.T, f *fakeShard) *Exporter {
	t.Helper()

	cfg := config.NewExportConfig()
	cfg.Format = "del"
	cfg.Query = "SELECT note FROM t"
	cfg.DelModifier.ColDel = ","
	cfg.DelModifier.CharDel = `"`
	cfg.DelModifier.RowDel = "\n"

	e := &Exporter{config: cfg, sqlParser: NewSQLParser(), stats: common.NewStats()}
	e.openShard = func() (*sql.DB, error) { return sql.OpenDB(fakeShardConnector{f: f}), nil }
	return e
}

// TestExportShard_ReleasesConnectionBeforePipeDrain pins the cleanup order that
// keeps a finished shard off the server. The stream writer consumes the shard
// pipes in shard order, so a shard sitting behind a slow one waits for its tail
// bytes to be accepted — and while it waits it must no longer hold a connection,
// because that wait is a live read transaction plus an idle socket on the
// server, and a cloud LB/NAT idle reaper that drops the socket fails the whole
// export.
func TestExportShard_ReleasesConnectionBeforePipeDrain(t *testing.T) {
	f := &fakeShard{rows: 3, closed: make(chan struct{})}
	e := newFakeShardExporter(t, f)

	// No look-ahead, and the consumer deliberately stays away until the property
	// has been checked: the only thing the shard can be waiting on is handing its
	// tail block over, which is exactly the state a shard behind a slow one is
	// stuck in.
	p := newShardPipe(context.Background(), 0, shardBlockSize)

	type outcome struct {
		rows int64
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		n, err := e.exportShard(context.Background(), 0,
			&ShardInfo{
				Columns:   []string{"note"},
				Mins:      []interface{}{int64(1)},
				Maxs:      []interface{}{int64(9)},
				IsNumeric: []bool{true},
			},
			ShardRange{Mins: []interface{}{int64(1)}, Maxs: []interface{}{int64(2)}},
			[]string{"note"}, common.ServerMySQL, p)
		done <- outcome{rows: n, err: err}
	}()

	select {
	case <-f.closed:
	case <-time.After(5 * time.Second):
		p.terminate()
		t.Fatal("exportShard still held its connection while waiting on the shard pipe")
	}

	select {
	case o := <-done:
		t.Fatalf("exportShard finished before its tail was handed over: rows=%d err=%v", o.rows, o.err)
	case <-time.After(100 * time.Millisecond):
	}

	// The consumer arrives. p.terminate() on the failure paths below releases a
	// shard that is blocked handing its tail over, so a failing assertion cannot
	// strand the goroutine on a pipe nobody reads.
	drained := make(chan []byte, 1)
	go func() {
		var out []byte
		for block := range p.ch {
			out = append(out, block...)
		}
		drained <- out
	}()

	var out []byte
	select {
	case out = <-drained:
	case <-time.After(5 * time.Second):
		p.terminate()
		t.Fatal("shard pipe was never closed")
	}

	var o outcome
	select {
	case o = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("exportShard did not return after its pipe drained")
	}

	if o.err != nil {
		t.Fatalf("exportShard failed: %v", o.err)
	}
	if o.rows != 3 {
		t.Fatalf("rows = %d, want 3", o.rows)
	}
	if n := strings.Count(string(out), "\n"); n != 3 {
		t.Fatalf("delivered %d rows in %d bytes, want 3", n, len(out))
	}
}

// TestExportShard_EmptyShardReleasesConnectionAndClosesPipe covers the shape a
// skewed parallel export spends most of its time in: a range with no rows at all
// (or no rows yet) must give its connection straight back and still close the
// pipe, or the in-order stream writer waits for an EOF that never arrives.
func TestExportShard_EmptyShardReleasesConnectionAndClosesPipe(t *testing.T) {
	f := &fakeShard{rows: 0, closed: make(chan struct{})}
	e := newFakeShardExporter(t, f)

	// Look-ahead available and nobody consuming: the shard cannot be waiting on
	// the pipe here, so the only reason it would not return is a resource it
	// refused to give up.
	p := newShardPipe(context.Background(), 4, shardBlockSize)

	done := make(chan error, 1)
	go func() {
		_, err := e.exportShard(context.Background(), 0,
			&ShardInfo{
				Columns:   []string{"note"},
				Mins:      []interface{}{int64(1)},
				Maxs:      []interface{}{int64(9)},
				IsNumeric: []bool{true},
			},
			ShardRange{Mins: []interface{}{int64(1)}, Maxs: []interface{}{int64(2)}},
			[]string{"note"}, common.ServerMySQL, p)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("exportShard on an empty range: %v", err)
		}
	case <-time.After(5 * time.Second):
		p.terminate()
		t.Fatal("exportShard did not return for an empty range")
	}

	select {
	case <-f.closed:
	default:
		t.Fatal("an empty shard kept its connection open")
	}

	// The pipe must be closed and carry nothing: exportShard returned, so its
	// deferred Close ran, and an empty range produced no bytes.
	closedCh := make(chan int, 1)
	go func() {
		n := 0
		for range p.ch {
			n++
		}
		closedCh <- n
	}()

	select {
	case n := <-closedCh:
		if n != 0 {
			t.Fatalf("empty shard delivered %d blocks it never read", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("empty shard never closed its pipe")
	}
}
