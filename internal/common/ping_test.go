package common

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/pkg/errors"
)

// pingFake answers db.Ping() with a scripted sequence of results and counts how
// many times the ping actually reached the driver. It exists so the retry bound of
// PingWithRetry can be measured against the two failure classes — a server
// rejection and a connection loss — without a live server, and so a test can tell
// "the run gave up" from "the run never tried again".
//
// The errors are deliberately NOT driver.ErrBadConn: database/sql retries that one
// internally, which would blur the very count these tests assert on.
type pingFake struct {
	mu    sync.Mutex
	calls int
	// results is consumed per ping; once drained, the last entry repeats, so a
	// test that wants "still down" scripts a single error.
	results []error
}

func (f *pingFake) next() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.results) == 0 {
		return nil
	}
	if len(f.results) == 1 {
		return f.results[0]
	}
	err := f.results[0]
	f.results = f.results[1:]
	return err
}

func (f *pingFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type pingConn struct{ f *pingFake }

func (c *pingConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("ping fake: no statements")
}
func (c *pingConn) Close() error               { return nil }
func (c *pingConn) Begin() (driver.Tx, error)  { return nil, errors.New("ping fake: no transactions") }
func (c *pingConn) Ping(context.Context) error { return c.f.next() }

type pingConnector struct{ f *pingFake }

func (c pingConnector) Connect(context.Context) (driver.Conn, error) { return &pingConn{f: c.f}, nil }
func (c pingConnector) Driver() driver.Driver                        { return pingDriver{} }

type pingDriver struct{}

func (pingDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("ping fake: dial through the connector")
}

// openPingDB hands back a *sql.DB whose pings land on the fake, plus the fake so
// the caller can assert on the attempt count.
func openPingDB(results ...error) (*sql.DB, *pingFake) {
	f := &pingFake{results: results}
	return sql.OpenDB(pingConnector{f: f}), f
}

// withFastPingPolicy shortens the backoff so the retry paths run in milliseconds.
// The bound itself is left at the shipped value: the number of attempts is exactly
// what these tests pin.
func withFastPingPolicy(t *testing.T) {
	t.Helper()
	orig := pingRetryPolicy
	pingRetryPolicy = RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Jitter: 0}
	t.Cleanup(func() { pingRetryPolicy = orig })
}

// TestPingSucceedsFirstTry covers the happy path: one ping, no records.
func TestPingSucceedsFirstTry(t *testing.T) {
	withFastPingPolicy(t)
	db, fake := openPingDB()
	defer db.Close()

	var err error
	out := captureStderr(t, func() { err = PingWithRetry(db) })
	if err != nil {
		t.Fatalf("healthy connection returned %v", err)
	}
	if got := fake.count(); got != 1 {
		t.Errorf("pings sent = %d, want 1", got)
	}
	if strings.Contains(out, "ping failed") {
		t.Errorf("healthy run logged a retry: %s", out)
	}
}

// TestPingDoesNotRetryServerRejections is the defect fix: a rejection the server
// decided returns on the first try, and the message counts the tries that
// actually happened. Each code here is permanently wrong for the command line as
// written — replaying it at the same server cannot produce a different answer.
func TestPingDoesNotRetryServerRejections(t *testing.T) {
	withFastPingPolicy(t)
	cases := []struct {
		name string
		err  error
		want string // substring of the server message that must survive the wrap
	}{
		{"credentials", &mysql.MySQLError{Number: 1045, Message: "Access denied for user 'gd'@'127.0.0.1'"}, "1045"},
		{"unknown database", &mysql.MySQLError{Number: 1049, Message: "Unknown database 'nodb'"}, "1049"},
		{"unknown variable", &mysql.MySQLError{Number: 1193, Message: "Unknown system variable 'not_a_variable'"}, "1193"},
		{"syntax in query", &mysql.MySQLError{Number: 1064, Message: "You have an error in your SQL syntax"}, "1064"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, fake := openPingDB(tc.err)
			defer db.Close()

			var err error
			out := captureStderr(t, func() { err = PingWithRetry(db) })
			if err == nil {
				t.Fatal("a rejection must be reported")
			}
			if got := fake.count(); got != 1 {
				t.Errorf("pings sent = %d, want 1 (permanent errors must not be retried)", got)
			}
			if strings.Contains(out, "ping failed") {
				t.Errorf("a rejection that is not retried must not claim it is: %s", out)
			}
			msg := err.Error()
			if !strings.Contains(msg, "failed to ping database after 1 attempt:") {
				t.Errorf("message lacks the honest attempt count: %s", msg)
			}
			if !strings.Contains(msg, tc.want) {
				t.Errorf("message lost the server error %s: %s", tc.want, msg)
			}
		})
	}
}

// TestPingRetriesAnOutageThenSucceeds keeps the half of the behaviour that was
// already right: a connection-level failure does get its reconnects.
func TestPingRetriesAnOutageThenSucceeds(t *testing.T) {
	withFastPingPolicy(t)
	down := errors.New("invalid connection")
	db, fake := openPingDB(down, down, nil)
	defer db.Close()

	var err error
	out := captureStderr(t, func() { err = PingWithRetry(db) })
	if err != nil {
		t.Fatalf("recovered outage reported %v", err)
	}
	if got := fake.count(); got != 3 {
		t.Errorf("pings sent = %d, want 3", got)
	}
	if n := strings.Count(out, "database ping failed, retrying"); n != 2 {
		t.Errorf("retry records = %d, want 2 (one per retry actually scheduled): %s", n, out)
	}
	if !strings.Contains(out, "attempt=2") {
		t.Errorf("second retry record missing: %s", out)
	}
}

// TestPingGivesUpAtTheBound pins the outage path's wording: three tries, two
// "retrying" records, and the last failure reported once, in the error.
func TestPingGivesUpAtTheBound(t *testing.T) {
	withFastPingPolicy(t)
	db, fake := openPingDB(errors.New("dial tcp 127.0.0.1:3306: connect: connection refused"))
	defer db.Close()

	var err error
	out := captureStderr(t, func() { err = PingWithRetry(db) })
	if err == nil {
		t.Fatal("an outage longer than the bound must be reported")
	}
	if got, want := fake.count(), pingRetryPolicy.MaxAttempts; got != want {
		t.Errorf("pings sent = %d, want %d", got, want)
	}
	if n := strings.Count(out, "database ping failed, retrying"); n != 2 {
		t.Errorf("retry records = %d, want 2: %s", n, out)
	}
	if !strings.Contains(err.Error(), "failed to ping database after 3 attempts:") {
		t.Errorf("exhausted outage should keep the shipped wording: %s", err)
	}
}

// TestPingPolicyWithNoRetryLeft covers the degenerate bound a library caller could
// configure: one attempt, no retry record, and an honest count in the message.
func TestPingPolicyWithNoRetryLeft(t *testing.T) {
	orig := pingRetryPolicy
	pingRetryPolicy = RetryPolicy{MaxAttempts: 0}
	t.Cleanup(func() { pingRetryPolicy = orig })

	db, fake := openPingDB(errors.New("invalid connection"))
	defer db.Close()

	var err error
	out := captureStderr(t, func() { err = PingWithRetry(db) })
	if err == nil {
		t.Fatal("an unreachable server must be reported")
	}
	if !strings.Contains(err.Error(), "after 1 attempt") {
		t.Errorf("message should count one attempt: %s", err)
	}
	if strings.Contains(out, "ping failed") {
		t.Errorf("no retry was scheduled, so none may be announced: %s", out)
	}
	if got := fake.count(); got != 1 {
		t.Errorf("pings sent = %d, want 1", got)
	}
}
