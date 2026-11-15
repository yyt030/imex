package backend

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"github.com/imex/internal/config"
)

// TestDamengSessionVarStatements pins the DM8 grammar the option is translated
// into. ALTER SESSION SET is how DM changes a session-level ini parameter (the
// DISQL "SET ..." commands are client-side display switches); the name goes
// into a quoted literal and the value arrives already SQL-literal-quoted from
// internal/common. Sorting makes every connection see the same order.
func TestDamengSessionVarStatements(t *testing.T) {
	if got := damengSessionVarStatements(nil); len(got) != 0 {
		t.Fatalf("no vars must produce no statements, got %v", got)
	}
	vars := map[string]string{
		"SORT_BUF_SIZE":    "2",
		"RESULT_SET_LIMIT": "'100'",
		"ENABLE_MONITOR":   "on",
	}
	want := []string{
		"ALTER SESSION SET 'ENABLE_MONITOR' = on",
		"ALTER SESSION SET 'RESULT_SET_LIMIT' = '100'",
		"ALTER SESSION SET 'SORT_BUF_SIZE' = 2",
	}
	got := damengSessionVarStatements(vars)
	if len(got) != len(want) {
		t.Fatalf("statements = %v, want %v", got, want)
	}
	wantNames := []string{"ENABLE_MONITOR", "RESULT_SET_LIMIT", "SORT_BUF_SIZE"}
	for i := range want {
		if got[i].stmt != want[i] {
			t.Errorf("statement %d = %q, want %q", i, got[i].stmt, want[i])
		}
		if got[i].name != wantNames[i] {
			t.Errorf("name %d = %q, want %q", i, got[i].name, wantNames[i])
		}
	}
}

var errFakeSessionVar = errors.New("fake ALTER SESSION SET failure")

// fakeExecerConn implements driver.Conn plus driver.ExecerContext, the shape of
// the real DM DmConnection. It records every statement it is asked to run.
type fakeExecerConn struct {
	execed []string
	failAt int // index to fail on; -1 never fails
	closed bool
}

func (c *fakeExecerConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("Prepare must not be used by the ExecerContext path")
}
func (c *fakeExecerConn) Close() error { c.closed = true; return nil }
func (c *fakeExecerConn) Begin() (driver.Tx, error) {
	return nil, errors.New("Begin is not used by --session-var")
}
func (c *fakeExecerConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.execed = append(c.execed, query)
	if c.failAt >= 0 && len(c.execed)-1 == c.failAt {
		return nil, errFakeSessionVar
	}
	return driver.RowsAffected(0), nil
}

// fakePrepareConn implements only driver.Conn, driving the Prepare fallback in
// execDamengSessionVar. It proves the wrapper does not assume a concrete driver.
type fakePrepareConn struct {
	prepared []string
	closed   bool
}

type fakePrepareStmt struct{}

func (fakePrepareStmt) Close() error  { return nil }
func (fakePrepareStmt) NumInput() int { return 0 }
func (fakePrepareStmt) Exec([]driver.Value) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}
func (fakePrepareStmt) Query([]driver.Value) (driver.Rows, error) { return nil, errors.New("not used") }

func (c *fakePrepareConn) Prepare(query string) (driver.Stmt, error) {
	c.prepared = append(c.prepared, query)
	return fakePrepareStmt{}, nil
}
func (c *fakePrepareConn) Close() error { c.closed = true; return nil }
func (c *fakePrepareConn) Begin() (driver.Tx, error) {
	return nil, errors.New("Begin is not used by --session-var")
}

type fakeConnector struct {
	conn       driver.Conn
	connectErr error
	calls      int
}

func (c *fakeConnector) Connect(context.Context) (driver.Conn, error) {
	c.calls++
	if c.connectErr != nil {
		return nil, c.connectErr
	}
	return c.conn, nil
}
func (c *fakeConnector) Driver() driver.Driver { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return nil, errors.New("not used") }

// TestDamengSessionVarConnectorReplaysOnEveryConnect pins the property the
// mechanism exists for: a fresh physical connection gets the whole statement
// list, in order, every time. database/sql calls Connect once per new pooled
// connection, so the main connection, each shard and each import worker are all
// covered by this one hook.
func TestDamengSessionVarConnectorReplaysOnEveryConnect(t *testing.T) {
	inner := &fakeConnector{}
	c := &damengSessionVarConnector{inner: inner, vars: []damengSessionVar{{name: "A", stmt: "A"}, {name: "B", stmt: "B"}}}
	for round := 0; round < 2; round++ {
		conn := &fakeExecerConn{failAt: -1}
		inner.conn = conn
		got, err := c.Connect(context.Background())
		if err != nil {
			t.Fatalf("round %d Connect: %v", round, err)
		}
		if got != conn {
			t.Fatalf("round %d returned a different connection", round)
		}
		if len(conn.execed) != 2 || conn.execed[0] != "A" || conn.execed[1] != "B" {
			t.Errorf("round %d statements = %v, want [A B]", round, conn.execed)
		}
		if conn.closed {
			t.Errorf("round %d connection must stay open", round)
		}
	}
	if inner.calls != 2 {
		t.Errorf("inner Connect calls = %d, want 2", inner.calls)
	}
}

// TestDamengSessionVarConnectorFailureClosesConnection pins the failure mode:
// a rejected ALTER SESSION SET closes the half-configured connection and
// returns an error that names the option, keeps the driver's server error in
// the Unwrap chain (so IsTransient still says "do not retry") and exits.
func TestDamengSessionVarConnectorFailureClosesConnection(t *testing.T) {
	conn := &fakeExecerConn{failAt: 1}
	c := &damengSessionVarConnector{
		inner: &fakeConnector{conn: conn},
		vars: []damengSessionVar{
			{name: "SORT_BUF_SIZE", stmt: "A"},
			{name: "RESULT_SET_LIMIT", stmt: "B"},
			{name: "HJ_BUF_SIZE", stmt: "C"},
		},
	}
	got, err := c.Connect(context.Background())
	if got != nil {
		t.Errorf("a failed SET must not return a connection")
	}
	if !errors.Is(err, errFakeSessionVar) {
		t.Errorf("error = %v, want the driver error to stay in the chain", err)
	}
	var svErr *damengSessionVarError
	if !errors.As(err, &svErr) || svErr.name != "RESULT_SET_LIMIT" {
		t.Errorf("error = %v, want a damengSessionVarError naming RESULT_SET_LIMIT", err)
	}
	if !strings.Contains(err.Error(), "--session-var RESULT_SET_LIMIT") ||
		!strings.Contains(err.Error(), "ALTER SESSION SET failed") ||
		!strings.Contains(err.Error(), errFakeSessionVar.Error()) {
		t.Errorf("error must name the option, the statement kind and the server reason: %v", err)
	}
	if !conn.closed {
		t.Errorf("a failed SET must close the connection")
	}
	if len(conn.execed) != 2 {
		t.Errorf("statements after failure = %v, want [A B]", conn.execed)
	}
}

// TestDamengSessionVarFailureNamesOptionNotValue pins the "explicit hint" part:
// the error names --session-var <name> but never echoes the value, which may be
// a secret, exactly as the info log prints names only.
func TestDamengSessionVarFailureNamesOptionNotValue(t *testing.T) {
	conn := &fakeExecerConn{failAt: 0}
	c := &damengSessionVarConnector{
		inner: &fakeConnector{conn: conn},
		vars: []damengSessionVar{{
			name: "SORT_BUF_SIZE",
			stmt: "ALTER SESSION SET 'SORT_BUF_SIZE' = 'SuperSecret123'",
		}},
	}
	_, err := c.Connect(context.Background())
	if err == nil {
		t.Fatal("a rejected SET must return an error")
	}
	want := "--session-var SORT_BUF_SIZE: ALTER SESSION SET failed: " + errFakeSessionVar.Error()
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	if strings.Contains(err.Error(), "SuperSecret123") {
		t.Errorf("value leaked into the error: %v", err)
	}
	if !errors.Is(err, errFakeSessionVar) {
		t.Errorf("error = %v, want the driver error in the chain", err)
	}
}

func TestDamengSessionVarConnectorConnectErrorSkipsSET(t *testing.T) {
	want := errors.New("dial failed")
	c := &damengSessionVarConnector{
		inner: &fakeConnector{connectErr: want},
		vars:  []damengSessionVar{{name: "A", stmt: "A"}},
	}
	if _, err := c.Connect(context.Background()); !errors.Is(err, want) {
		t.Errorf("Connect error = %v, want %v", err, want)
	}
}

func TestDamengSessionVarConnectorPrepareFallback(t *testing.T) {
	conn := &fakePrepareConn{}
	c := &damengSessionVarConnector{
		inner: &fakeConnector{conn: conn},
		vars:  []damengSessionVar{{name: "A", stmt: "A"}, {name: "B", stmt: "B"}},
	}
	if _, err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if len(conn.prepared) != 2 || conn.prepared[0] != "A" || conn.prepared[1] != "B" {
		t.Errorf("prepared = %v, want [A B]", conn.prepared)
	}
	if conn.closed {
		t.Errorf("a successful SET must not close the connection")
	}
}

// TestOpenDamengSessionVarDBIsLazy exercises the production entry point with a
// real (unreachable) DM DSN: building the decorated DB must not dial, must
// expose the real DM driver, and must be closeable. SET failures only surface
// on the caller's PingWithRetry, exactly as on pg's AfterConnect.
func TestOpenDamengSessionVarDBIsLazy(t *testing.T) {
	cfg := &config.Config{User: "u", Password: "pw", Host: "127.0.0.1", Port: 5236}
	dsn, err := buildDamengDSN(cfg)
	if err != nil {
		t.Fatalf("buildDamengDSN: %v", err)
	}
	db, err := openDamengSessionVarDB(dsn, map[string]string{"SORT_BUF_SIZE": "2"})
	if err != nil {
		t.Fatalf("openDamengSessionVarDB: %v", err)
	}
	defer db.Close()
	if db.Stats().OpenConnections != 0 {
		t.Errorf("Open must stay lazy, got %d open connections", db.Stats().OpenConnections)
	}
	if db.Driver() == nil {
		t.Errorf("decorated DB must still expose the DM driver")
	}
}

// TestDamengOpenHonoursVars pins the branch in damengBackend.Open: a Config
// with Vars takes the decorated path, and a Config without Vars stays on the
// plain sql.Open("dm", dsn) path. Both are lazy; the difference is invisible
// from the outside until a connection is actually opened, which is the point —
// no byte of the DSN or config record changes on a run without --session-var.
func TestDamengOpenHonoursVars(t *testing.T) {
	base := &config.Config{User: "u", Password: "pw", Host: "127.0.0.1", Port: 5236}
	for _, withVars := range []bool{false, true} {
		cfg := *base
		if withVars {
			cfg.Vars = map[string]string{"SORT_BUF_SIZE": "2"}
		}
		db, err := (damengBackend{}).Open(&cfg)
		if err != nil {
			t.Fatalf("Open(withVars=%v): %v", withVars, err)
		}
		if db.Stats().OpenConnections != 0 {
			t.Errorf("Open(withVars=%v) must be lazy", withVars)
		}
		db.Close()
	}
}

// Keep the compiler honest about the interfaces the fakes are meant to model.
var (
	_ driver.Connector     = (*fakeConnector)(nil)
	_ driver.Driver        = fakeDriver{}
	_ driver.Conn          = (*fakeExecerConn)(nil)
	_ driver.ExecerContext = (*fakeExecerConn)(nil)
	_ driver.Conn          = (*fakePrepareConn)(nil)
	_ driver.Connector     = (*damengSessionVarConnector)(nil)
)
