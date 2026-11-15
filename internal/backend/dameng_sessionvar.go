package backend

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sort"

	"github.com/pkg/errors"
)

// damengSessionVar is one --session-var pair rendered for DM8. Keeping the name
// next to its statement lets a failed ALTER SESSION SET name the option that
// caused it without ever echoing the value (which may be a secret).
type damengSessionVar struct {
	name string
	stmt string
}

// damengSessionVarError is a rejected ALTER SESSION SET. It is a configuration
// error, not a dead endpoint: a bad parameter name or value is rejected by every
// node in a multi-address run, so OpenWithFailover must not silently try the
// next address and bury the cause in "all addresses failed". The caller sees a
// one-line error naming --session-var <name>, the failing statement kind and
// the server's own reason, then exits.
//
// Unwrap keeps the driver error in the chain so common.IsTransient / the DM
// code verdict still see the server code and decide "server answered, do not
// retry": the run stops on the first ping instead of burning the retry backoff.
// The value is deliberately never included.
type damengSessionVarError struct {
	name  string
	cause error
}

func (e *damengSessionVarError) Error() string {
	return fmt.Sprintf("--session-var %s: ALTER SESSION SET failed: %v", e.name, e.cause)
}

func (e *damengSessionVarError) Unwrap() error { return e.cause }

// damengSessionVarStatements renders --session-var pairs as Dameng session
// parameter statements. DM8 has no MySQL-style "SET name = value" for server
// parameters: a session-level ini parameter is changed with
//
//	ALTER SESSION SET '<name>' = <value> [PURGE]
//
// (the DISQL "SET ..." commands are client-side display switches, not server
// session settings). The name goes into a single-quoted string literal, which
// is the form the DM8 system manuals spell out; ParseSessionVars has already
// constrained it to [A-Za-z][A-Za-z0-9_]*, so the literal cannot be broken
// out of. The value arrives SQL-literal-quoted from internal/common. DM8 is
// stricter than MySQL/pg here: ALTER SESSION SET accepts numeric literals
// and quoted strings, but not the bare keywords (on/off/default/...) that
// ParseSessionVars passes through for MySQL — those come back as a server-
// side syntax error at connect time. A non-numeric quoted string on a
// numeric parameter is silently stored as a garbage value by DM8 instead
// of being rejected; numeric literals are the reliable form on this engine.
//
// Names are sorted so every connection sees the same statement order. A name
// the DSN or the tool owns never reaches here: cmd rejects it in
// ParseSessionVars through IsReservedConnParam before any connection is built.
func damengSessionVarStatements(vars map[string]string) []damengSessionVar {
	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]damengSessionVar, 0, len(names))
	for _, name := range names {
		out = append(out, damengSessionVar{
			name: name,
			stmt: "ALTER SESSION SET '" + name + "' = " + vars[name],
		})
	}
	return out
}

// damengSessionVarConnector decorates the Dameng driver's own Connector so
// --session-var is replayed on every NEW physical connection, the property the
// option promises on MySQL (DSN parameters) and pg (pgx AfterConnect). The DM
// driver has neither hook, but it does implement driver.DriverContext /
// driver.Connector, so wrapping Connect gives the same lifecycle without
// changing the DSN: the SET runs inside the first real dial, Open stays lazy,
// and a bad value surfaces through the caller's PingWithRetry like any other
// connect-time failure.
//
// database/sql keeps the returned *sql.DB's connector for the pool's lifetime,
// so the main export connection, every shard connection and every parallel
// import worker connection all pass through this wrapper. A reused idle
// connection does not replay the statements — the session parameters persist
// for the life of that physical connection, exactly as on pg.
type damengSessionVarConnector struct {
	inner driver.Connector
	vars  []damengSessionVar
}

func (c *damengSessionVarConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range c.vars {
		if err := execDamengSessionVar(ctx, conn, v.stmt); err != nil {
			// A failed ALTER SESSION makes this connection unusable: close it so
			// database/sql does not put a half-configured connection in the pool.
			// The error names the option that failed and keeps the driver's
			// server code/message in the chain, so the run reports the real
			// cause and stops instead of being mistaken for a dead endpoint.
			conn.Close()
			return nil, &damengSessionVarError{name: v.name, cause: err}
		}
	}
	return conn, nil
}

func (c *damengSessionVarConnector) Driver() driver.Driver { return c.inner.Driver() }

// execDamengSessionVar runs one statement with no bind arguments. The DM
// connection implements driver.ExecerContext; the two fallbacks keep the
// wrapper correct for any driver.Connector handed to it (including the fake
// connectors the unit tests use) without depending on a concrete driver type.
func execDamengSessionVar(ctx context.Context, conn driver.Conn, stmt string) error {
	switch ex := conn.(type) {
	case driver.ExecerContext:
		_, err := ex.ExecContext(ctx, stmt, nil)
		return err
	case driver.Execer:
		_, err := ex.Exec(stmt, nil)
		return err
	default:
		prepared, err := conn.Prepare(stmt)
		if err != nil {
			return err
		}
		defer prepared.Close()
		_, err = prepared.Exec(nil)
		return err
	}
}

// openDamengSessionVarDB builds the decorated *sql.DB. The plain sql.Open
// path is reused only to recover the driver instance registered under "dm":
// database/sql exposes it through (*sql.DB).Driver(), and the DM driver's
// OpenConnector is the same call sql.Open itself makes. The probe DB is lazy
// (no socket is opened) and is closed immediately, so the run dials exactly
// once, through sql.OpenDB below.
func openDamengSessionVarDB(dsn string, vars map[string]string) (*sql.DB, error) {
	probe, err := sql.Open("dm", dsn)
	if err != nil {
		return nil, err
	}
	drv := probe.Driver()
	probe.Close()

	dc, ok := drv.(driver.DriverContext)
	if !ok {
		return nil, errors.New("Dameng driver does not implement driver.DriverContext; cannot apply --session-var")
	}
	inner, err := dc.OpenConnector(dsn)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(&damengSessionVarConnector{
		inner: inner,
		vars:  damengSessionVarStatements(vars),
	}), nil
}
