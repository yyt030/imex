package common

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// mysqlFixtureTable is the shape of a dialect's code table, not its content: the
// production numbers and their wording are pinned in
// internal/backend/dberr_test.go, and copying them here would let a edited table
// pass two suites while the shipped verdict changed.
var mysqlFixtureTable = map[uint16]string{
	1040: "too many connections",
	2006: "server has gone away",
	2013: "lost connection during query",
}

func mysqlFixtureNumber(err error) (uint16, bool) {
	var m *mysql.MySQLError
	if errors.As(err, &m) {
		return m.Number, true
	}
	return 0, false
}

var mysqlFixtureVerdict = CodeVerdict(mysqlFixtureNumber, mysqlFixtureTable)

func TestIsTransient(t *testing.T) {
	wrapped := func(err error) error { return fmt.Errorf("batch 4: %w", err) }

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"canceled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"wrapped canceled", wrapped(context.Canceled), false},
		{"bad conn", driver.ErrBadConn, true},
		{"eof", io.EOF, true},
		{"unexpected eof", io.ErrUnexpectedEOF, true},
		{"wrapped eof", wrapped(io.EOF), true},
		{"econnreset", syscall.ECONNRESET, true},
		{"epipe", syscall.EPIPE, true},
		{"gone away", &mysql.MySQLError{Number: 2006, Message: "server has gone away"}, true},
		{"lost during query", &mysql.MySQLError{Number: 2013, Message: "lost connection"}, true},
		{"too many connections", &mysql.MySQLError{Number: 1040}, true},
		{"wrapped gone away", wrapped(&mysql.MySQLError{Number: 2006}), true},
		// Statement-level errors must not be retried: a retry cannot succeed
		// and replaying the transaction would be wasteful or wrong.
		{"syntax", &mysql.MySQLError{Number: 1064, Message: "You have an error in your SQL syntax"}, false},
		{"packet too large", &mysql.MySQLError{Number: 1153, Message: "Got a packet bigger than 'max_allowed_packet'"}, false},
		{"deadlock", &mysql.MySQLError{Number: 1213}, false},
		{"lock wait timeout", &mysql.MySQLError{Number: 1205}, false},
		{"access denied", &mysql.MySQLError{Number: 1045}, false},
		{"duplicate key", &mysql.MySQLError{Number: 1062}, false},
		// The short-circuit is the point of `decided`: a server error the table
		// does not list ends the run even when its own message reads like a
		// connection loss. Without it this case would fall through to
		// transientSubstrings and be retried.
		{"unlisted code with outage wording", &mysql.MySQLError{Number: 1205, Message: "lost connection during prepare"}, false},
		{"plain error", errors.New("unsupported format: xml"), false},
		// Text-only connection failures (proxy layers) are recognised by text.
		{"text broken pipe", errors.New("write tcp 10.0.0.1:3306: broken pipe"), true},
		{"text invalid connection", errors.New("invalid connection"), true},
		{"net timeout", &net.OpError{Op: "read", Net: "tcp", Err: &timeoutError{}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTransient(tt.err, mysqlFixtureVerdict); got != tt.want {
				t.Fatalf("IsTransient(%v) = %v, want %v", tt.err, got, tt.want)
			}
			// Wrapping must not change the classification.
			if tt.err != nil {
				if got := IsTransient(wrapped(tt.err), mysqlFixtureVerdict); got != tt.want {
					t.Fatalf("IsTransient(wrapped %v) = %v, want %v", tt.err, got, tt.want)
				}
			}
		})
	}
}

// TestIsTransientWithoutADialectStaysNeutral pins what a nil verdict means: the
// typed codes get no vote, so only the neutral rules decide. A server rejection
// whose text happens to name a connection phrase is therefore retried here and
// not retried above — which is why every command path wires its backend in.
func TestIsTransientWithoutADialectStaysNeutral(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"bad conn still neutral", driver.ErrBadConn, true},
		{"driver sentinel by text", mysql.ErrInvalidConn, true},
		{"listed code by its own wording", &mysql.MySQLError{Number: 2006, Message: "server has gone away"}, true},
		{"listed code with a bare message", &mysql.MySQLError{Number: 1040, Message: ""}, false},
		{"unlisted code with outage wording", &mysql.MySQLError{Number: 1205, Message: "lost connection during prepare"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTransient(tt.err, nil); got != tt.want {
				t.Fatalf("IsTransient(%v, nil) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

type timeoutError struct{}

func (e *timeoutError) Error() string   { return "i/o timeout" }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return true }
