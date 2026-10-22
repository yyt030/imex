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
		{"invalid conn", mysql.ErrInvalidConn, true},
		{"eof", io.EOF, true},
		{"unexpected eof", io.ErrUnexpectedEOF, true},
		{"wrapped eof", wrapped(io.EOF), true},
		{"econnreset", syscall.ECONNRESET, true},
		{"epipe", syscall.EPIPE, true},
		{"gone away", &mysql.MySQLError{Number: 2006, Message: "server has gone away"}, true},
		{"lost during query", &mysql.MySQLError{Number: 2013, Message: "lost connection"}, true},
		{"too many connections", &mysql.MySQLError{Number: 1040}, true},
		{"killed", &mysql.MySQLError{Number: 1927}, true},
		{"wrapped gone away", wrapped(&mysql.MySQLError{Number: 2006}), true},
		// Statement-level errors must not be retried: a retry cannot succeed
		// and replaying the transaction would be wasteful or wrong.
		{"syntax", &mysql.MySQLError{Number: 1064, Message: "You have an error in your SQL syntax"}, false},
		{"packet too large", &mysql.MySQLError{Number: 1153, Message: "Got a packet bigger than 'max_allowed_packet'"}, false},
		{"deadlock", &mysql.MySQLError{Number: 1213}, false},
		{"lock wait timeout", &mysql.MySQLError{Number: 1205}, false},
		{"access denied", &mysql.MySQLError{Number: 1045}, false},
		{"duplicate key", &mysql.MySQLError{Number: 1062}, false},
		{"plain error", errors.New("unsupported format: xml"), false},
		// Text-only connection failures (proxy layers) are recognised by text.
		{"text broken pipe", errors.New("write tcp 10.0.0.1:3306: broken pipe"), true},
		{"text invalid connection", errors.New("invalid connection"), true},
		{"net timeout", &net.OpError{Op: "read", Net: "tcp", Err: &timeoutError{}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTransient(tt.err); got != tt.want {
				t.Fatalf("IsTransient(%v) = %v, want %v", tt.err, got, tt.want)
			}
			// Wrapping must not change the classification.
			if tt.err != nil {
				if got := IsTransient(wrapped(tt.err)); got != tt.want {
					t.Fatalf("IsTransient(wrapped %v) = %v, want %v", tt.err, got, tt.want)
				}
			}
		})
	}
}

func TestTransientErrorCode(t *testing.T) {
	if got := TransientErrorCode(&mysql.MySQLError{Number: 2013}); got != 2013 {
		t.Fatalf("got %d, want 2013", got)
	}
	if got := TransientErrorCode(errors.New("no code")); got != 0 {
		t.Fatalf("got %d, want 0", got)
	}
}

type timeoutError struct{}

func (e *timeoutError) Error() string   { return "i/o timeout" }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return true }
