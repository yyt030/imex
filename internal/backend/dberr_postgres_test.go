package backend

import (
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/imex/internal/common"
)

// TestTransientPostgresCodeTable pins the SQLSTATEs and their wording. Adding or
// removing one changes which pg failures retry, so an edit has to come with a
// change record naming the failures that stop (or start) ending a run.
func TestTransientPostgresCodeTable(t *testing.T) {
	want := map[string]string{
		"08001": "sqlclient unable to establish sqlconnection",
		"08003": "connection does not exist",
		"08006": "connection failure",
		"08P01": "protocol violation",
		"53300": "too many connections",
		"57P01": "admin shutdown",
		"57P02": "crash shutdown",
		"57P03": "cannot connect now",
		"57P05": "idle session timeout",
	}
	if len(transientPostgresCodes) != len(want) {
		t.Errorf("table has %d codes, want %d", len(transientPostgresCodes), len(want))
	}
	for code, wording := range want {
		if got := transientPostgresCodes[code]; got != wording {
			t.Errorf("code %s = %q, want %q", code, got, wording)
		}
	}
	// Absent on purpose, with the reason in dberr_postgres.go: a refused connect,
	// an unknown commit outcome, a dropped database, server file I/O, statement
	// errors, contention and authentication are all decided (not transient).
	for _, code := range []string{"08004", "08007", "23505", "25P02", "28P01", "40001", "40P01", "42601", "57014", "57P04", "58030"} {
		if _, ok := transientPostgresCodes[code]; ok {
			t.Errorf("code %s must stay out of the transient table", code)
		}
	}
}

// TestPostgresTransientError checks the two halves of the answer: a SQLSTATE in the
// table is transient and decided, any other server error is decided and not
// transient (the server answered, so the connection is fine), and a non-pg error
// abstains so the neutral rules decide.
func TestPostgresTransientError(t *testing.T) {
	be := Must(EnginePG)
	cases := []struct {
		name          string
		err           error
		wantTransient bool
		wantDecided   bool
	}{
		{"57P01 admin shutdown", &pgconn.PgError{Severity: "FATAL", Code: "57P01"}, true, true},
		{"53300 too many connections", &pgconn.PgError{Severity: "FATAL", Code: "53300"}, true, true},
		{"57P03 cannot connect now", &pgconn.PgError{Severity: "FATAL", Code: "57P03"}, true, true},
		{"wrapped 08006", fmt.Errorf("query: %w", &pgconn.PgError{Severity: "FATAL", Code: "08006"}), true, true},
		{"23505 unique violation", &pgconn.PgError{Severity: "ERROR", Code: "23505"}, false, true},
		{"42601 syntax", &pgconn.PgError{Severity: "ERROR", Code: "42601"}, false, true},
		{"40001 serialization", &pgconn.PgError{Severity: "ERROR", Code: "40001"}, false, true},
		{"08007 unknown commit", &pgconn.PgError{Severity: "ERROR", Code: "08007"}, false, true},
		{"57P04 database dropped", &pgconn.PgError{Severity: "FATAL", Code: "57P04"}, false, true},
		{"58030 io error", &pgconn.PgError{Severity: "ERROR", Code: "58030"}, false, true},
		{"not a server error", io.EOF, false, false},
		{"context canceled", context.Canceled, false, false},
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

// TestPostgresVerdictClosesTheCase is the reason common's chain needs the decided
// bit: a statement error whose message happens to read like an outage must not be
// retried just because the neutral text table would match it.
func TestPostgresVerdictClosesTheCase(t *testing.T) {
	err := &pgconn.PgError{Severity: "ERROR", Code: "23505", Message: "duplicate key: connection reset by peer"}
	if common.IsTransient(err, Must(EnginePG).TransientError) {
		t.Fatal("a decided non-transient SQLSTATE fell through to the text table")
	}
}
