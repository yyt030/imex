package backend

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// transientPostgresCodes lists the SQLSTATEs that mean the connection is dead
// rather than the statement being wrong: after one of these the server has rolled
// back the open transaction and the caller may reconnect and replay. The names are
// PostgreSQL 14.24's own (share/postgresql/14/errcodes.txt); pgx surfaces the code
// as *pgconn.PgError, including when a failed connect wraps it in
// *pgconn.ConnectError.
//
// Measured on the live bed, not inferred from the manual:
//   - 57P01: SELECT pg_terminate_backend(<pid>) on a pinned *sql.Conn, then the
//     next query returns *pgconn.PgError{Code: "57P01"} with "terminating
//     connection due to administrator command".
//   - 53300: with ALTER ROLE pgprobe CONNECTION LIMIT 2 and two held connections,
//     the third connect returns *pgconn.ConnectError whose Unwrap chain contains
//     *pgconn.PgError{Code: "53300"} ("too many connections for role").
//
// Deliberately absent:
//   - 08004 (server rejected establishment): the server already refused this
//     connection for a reason retrying cannot change.
//   - 08007 (transaction resolution unknown): the commit outcome is unknown, so
//     replaying could duplicate rows.
//   - 57P04 (database dropped): reconnecting cannot recreate the database.
//   - 58030 (io error): a server-side file error aborts the statement; the
//     connection itself is not necessarily gone, so the neutral rules decide.
//   - 57014 (query canceled), 40001 (serialization failure) and 40P01 (deadlock
//     detected): contention and cancellation are surfaced to the operator for the
//     same reason MySQL's 1213/1205 are — a blind replay can duplicate work.
//   - 28xxx (authentication): credentials do not heal themselves.
var transientPostgresCodes = map[string]string{
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

// postgresServerCode pulls the SQLSTATE out of pgx's error type. ok is true for any
// *pgconn.PgError, including the codes deliberately absent above: the server
// answered, which is what closes the case.
func postgresServerCode(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code, true
	}
	return "", false
}

// postgresVerdict reads the SQLSTATE table. It mirrors common.CodeVerdict, whose
// keys are uint16 for MySQL's numbered errors; SQLSTATEs are strings, so the lookup
// lives here rather than widening the shared helper.
func postgresVerdict(err error) (bool, bool) {
	code, ok := postgresServerCode(err)
	if !ok {
		return false, false
	}
	return transientPostgresCodes[code] != "", true
}

// TransientError is the PostgreSQL half of the retry decision. Until round 134 it
// answered (false, false) for everything, leaving the neutral socket/EOF rules to
// decide; the table above is a capability change (faces ③ and ⑤), not a move.
func (postgresBackend) TransientError(err error) (bool, bool) {
	return postgresVerdict(err)
}
