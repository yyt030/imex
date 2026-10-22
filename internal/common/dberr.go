package common

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"

	"github.com/go-sql-driver/mysql"
)

// transientMySQLCodes lists server/client error numbers that indicate a
// connection-level failure rather than a problem with the statement itself.
// After one of these the connection is unusable and the server has rolled back
// any open transaction, so the caller may reconnect and replay.
//
// Deliberately absent:
//   - 1213 (deadlock) and 1205 (lock wait timeout): these are contention
//     errors. With innodb_rollback_on_timeout=OFF only the statement is rolled
//     back, so blindly replaying the surrounding transaction could duplicate
//     rows; they are surfaced to the operator instead.
//   - 1153 (packet too large): retrying cannot succeed, the batch must shrink.
//   - 1045 (access denied): credentials will not heal themselves.
var transientMySQLCodes = map[uint16]string{
	1040: "too many connections",
	1053: "server shutdown in progress",
	1152: "aborted connection",
	1927: "connection was killed",
	2006: "server has gone away",
	2013: "lost connection during query",
	2055: "lost connection at reading authorization packet",
}

// transientSubstrings catches connection errors that reach us as plain text,
// e.g. from a proxy layer that never produced a typed error.
var transientSubstrings = []string{
	"invalid connection",
	"bad connection",
	"broken pipe",
	"connection reset",
	"connection refused",
	"connection was killed",
	"server has gone away",
	"lost connection",
	"unexpected eof",
	"i/o timeout",
}

// IsTransient reports whether err is worth retrying after reconnecting.
//
// Context errors are never transient: the caller asked to stop, or a deadline
// expired and retrying would just burn the remaining budget. Statement-level
// errors (syntax, constraint, packet size) are also not transient.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, mysql.ErrInvalidConn) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) {
		return true
	}

	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return transientMySQLCodes[mysqlErr.Number] != ""
	}

	// A socket read/write timeout surfaces as a net.Error; any other net.OpError
	// (dial failures, resets) is likewise a connection problem.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}

	msg := strings.ToLower(err.Error())
	for _, s := range transientSubstrings {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// TransientErrorCode returns the MySQL error number carried by err, or 0 when
// err did not come from the server. It is used for log context.
func TransientErrorCode(err error) uint16 {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number
	}
	return 0
}
