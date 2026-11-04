package backend

import (
	"context"
	"errors"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/imex/internal/common"
)

// TestTransientMySQLCodeTable pins the shipped numbers and their wording in place.
// The table decides whether a run retries or gives up, and a silently added or
// removed code would change that for every operator, so an edit here must come with
// a change record that says which failures stop ending runs.
func TestTransientMySQLCodeTable(t *testing.T) {
	want := map[uint16]string{
		1040: "too many connections",
		1053: "server shutdown in progress",
		1152: "aborted connection",
		1927: "connection was killed",
		2006: "server has gone away",
		2013: "lost connection during query",
		2055: "lost connection at reading authorization packet",
	}
	if len(transientMySQLCodes) != len(want) {
		t.Errorf("table has %d codes, want %d", len(transientMySQLCodes), len(want))
	}
	for code, wording := range want {
		if got := transientMySQLCodes[code]; got != wording {
			t.Errorf("code %d = %q, want %q", code, got, wording)
		}
	}
	// Absent on purpose: contention and statement errors, listed in the table's own
	// comment. Replaying after one of these could duplicate rows the server left in
	// place, so the operator has to see them.
	for _, code := range []uint16{1045, 1062, 1064, 1153, 1193, 1205, 1213} {
		if _, ok := transientMySQLCodes[code]; ok {
			t.Errorf("code %d must stay out of the transient table", code)
		}
	}
}

// TestMySQLTransientError pins the two halves of the dialect's answer: the driver
// sentinel and the code table, and that a code the table does not name is still
// `decided` — the server answered, so the connection is fine and the run must stop.
func TestMySQLTransientError(t *testing.T) {
	be := Must(EngineMySQL)
	cases := []struct {
		name          string
		err           error
		wantTransient bool
		wantDecided   bool
	}{
		{"sentinel", mysql.ErrInvalidConn, true, true},
		{"wrapped sentinel", errors.New("batch 3: " + mysql.ErrInvalidConn.Error()), false, false},
		{"1040 with a bare message", &mysql.MySQLError{Number: 1040}, true, true},
		{"2006", &mysql.MySQLError{Number: 2006, Message: "MySQL server has gone away"}, true, true},
		{"2013", &mysql.MySQLError{Number: 2013, Message: "Lost connection to MySQL server during query"}, true, true},
		{"1045 access denied", &mysql.MySQLError{Number: 1045, Message: "Access denied for user 'gd'@'127.0.0.1'"}, false, true},
		{"1064 syntax", &mysql.MySQLError{Number: 1064, Message: "You have an error in your SQL syntax"}, false, true},
		{"1205 lock wait", &mysql.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"}, false, true},
		// The short-circuit's whole point: an unlisted server error whose own text
		// reads like an outage must not be retried.
		{"1205 worded like an outage", &mysql.MySQLError{Number: 1205, Message: "lost connection during prepare"}, false, true},
		{"not a server error", errors.New("invalid connection"), false, false},
		{"a PostgreSQL error is not ours", &pgconn.PgError{Severity: "FATAL", Code: "57P01", Message: "admin shutdown"}, false, false},
		{"nil", nil, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			transient, decided := be.TransientError(c.err)
			if transient != c.wantTransient || decided != c.wantDecided {
				t.Fatalf("TransientError(%v) = (%v, %v), want (%v, %v)", c.err, transient, decided, c.wantTransient, c.wantDecided)
			}
		})
	}
}

// TestIsTransientWithTheShippedVerdicts is the end-to-end pin for the retry
// decision on both engines: it runs common's neutral chain with each dialect's
// method, which is exactly what the export, import, list and show paths wire
// together. Every expectation here is the behaviour MySQL runs had before this
// decision moved into the dialect, and the PostgreSQL rows are what a pg run does
// today — none of them may change in a structural round.
func TestIsTransientWithTheShippedVerdicts(t *testing.T) {
	cases := []struct {
		name string
		eng  Engine
		err  error
		want bool
		why  string
	}{
		{"bare 1040 retried", EngineMySQL, &mysql.MySQLError{Number: 1040}, true, "the table hits with no wording to match"},
		{"1045 ends the run", EngineMySQL, &mysql.MySQLError{Number: 1045, Message: "Access denied for user 'gd'@'127.0.0.1'"}, false, "credentials do not heal"},
		{"1205 with outage wording ends the run", EngineMySQL, &mysql.MySQLError{Number: 1205, Message: "lost connection during prepare"}, false, "the dialect verdict closes the case"},
		{"sentinel retried", EngineMySQL, mysql.ErrInvalidConn, true, "the driver's own dropped-connection error"},
		{"text-only outage retried", EngineMySQL, errors.New("write tcp 10.0.0.1:3306: broken pipe"), true, "neutral table"},
		{"canceled never retried", EngineMySQL, context.Canceled, false, "the caller asked to stop"},
		{"pg 08006 retried by code", EnginePG, &pgconn.PgError{Severity: "FATAL", Code: "08006", Message: "could not receive data from server: Connection reset by peer"}, true, "the code table decides before the text table"},
		{"pg admin shutdown retried", EnginePG, &pgconn.PgError{Severity: "FATAL", Code: "57P01", Message: "terminating connection due to administrator command"}, true, "the server rolled back and closed"},
		{"pg too many connections retried", EnginePG, &pgconn.PgError{Severity: "FATAL", Code: "53300", Message: "too many connections for role \"pgprobe\""}, true, "connect-time backoff can heal"},
		{"pg syntax error ends the run", EnginePG, &pgconn.PgError{Severity: "ERROR", Code: "42601", Message: "syntax error at or near \"FROM\""}, false, "statement errors are not connection errors"},
		{"pg serialization failure ends the run", EnginePG, &pgconn.PgError{Severity: "ERROR", Code: "40001", Message: "could not serialize access"}, false, "contention is surfaced, not replayed"},
		{"pg unknown commit outcome ends the run", EnginePG, &pgconn.PgError{Severity: "ERROR", Code: "08007", Message: "transaction resolution unknown"}, false, "replay could duplicate a committed batch"},
		{"pg io error ends the run", EnginePG, &pgconn.PgError{Severity: "ERROR", Code: "58030", Message: "could not write to file"}, false, "a server file error is not a dead connection"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := common.IsTransient(c.err, Must(c.eng).TransientError)
			if got != c.want {
				t.Fatalf("IsTransient(%v) on %s = %v, want %v (%s)", c.err, c.eng, got, c.want, c.why)
			}
		})
	}
}
