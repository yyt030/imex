package backend

import (
	"errors"

	"github.com/go-sql-driver/mysql"

	"github.com/imex/internal/common"
)

// transientMySQLCodes lists the MySQL, MariaDB and OceanBase server and client
// error numbers that indicate a connection-level failure rather than a problem
// with the statement itself. After one of these the connection is unusable and
// the server has rolled back any open transaction, so the caller may reconnect
// and replay.
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

// mysqlServerCode reads the number out of the driver's own error type. ok is true
// for any *mysql.MySQLError, including the numbers above that are deliberately
// absent — the server answered, which is what closes the case.
func mysqlServerCode(err error) (uint16, bool) {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number, true
	}
	return 0, false
}

// mysqlVerdict reads the code table through common's shared machinery.
var mysqlVerdict = common.CodeVerdict(mysqlServerCode, transientMySQLCodes)

// TransientError is the MySQL half of the retry decision: the driver's own
// sentinel for a dropped connection, then the code table.
//
// The sentinel used to be tested one step earlier, inside common's neutral chain,
// next to database/sql's driver.ErrBadConn. The verdict is the same wherever it is
// consulted: nothing between the two positions matches ErrInvalidConn, and its own
// text ("invalid connection") is in the neutral table as well, so an unwired call
// path still reports it transient.
func (mysqlBackend) TransientError(err error) (bool, bool) {
	if errors.Is(err, mysql.ErrInvalidConn) {
		return true, true
	}
	return mysqlVerdict(err)
}
