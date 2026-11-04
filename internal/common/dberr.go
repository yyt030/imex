package common

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
)

// transientSubstrings catches connection errors that reach us as plain text,
// e.g. from a proxy layer that never produced a typed error.
//
// The list is engine-neutral on purpose. Several phrases are MySQL's own wording
// ("server has gone away", "lost connection"), and a future dialect that spells a
// connection loss differently is served by its typed codes (see TransientVerdict),
// not by editing this table — removing a phrase here would silently stop retrying
// errors that are retried today, on every engine.
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

// TransientVerdict is a dialect's answer about one error.
//
// decided reports whether err is one of this engine's own typed server errors;
// when it is, transient is the final verdict and the neutral fallbacks in
// IsTransient below are not consulted. That short-circuit is not decoration: a
// server that answered "syntax error" has a working connection, and letting such
// an error reach the text table would let a message that happens to contain
// "lost connection" be retried.
//
// An engine with no code table yet returns (false, false) for everything, which is
// exactly the neutral decision it had before any table existed. Adding codes to it
// is a new capability, not a refactor, because it turns failures that end a run
// today into retries.
type TransientVerdict func(err error) (transient bool, decided bool)

// CodeVerdict builds the dialect half of the decision from one table of numeric
// server codes. numberOf pulls the code out of the engine's own error type and
// reports whether err is such an error at all; table maps the codes that mean a
// dead connection — after one of these the connection is unusable and the server
// has rolled back any open transaction, so the caller may reconnect and replay.
//
// It lives here because common cannot import internal/backend (the backend already
// depends on common, and the cycle would close), and the shape of the decision is
// the same for every engine that numbers its errors.
func CodeVerdict(numberOf func(error) (code uint16, ok bool), table map[uint16]string) TransientVerdict {
	return func(err error) (bool, bool) {
		code, ok := numberOf(err)
		if !ok {
			return false, false
		}
		return table[code] != "", true
	}
}

// IsTransient reports whether err is worth retrying after reconnecting.
//
// Context errors are never transient: the caller asked to stop, or a deadline
// expired and retrying would just burn the remaining budget. Statement-level
// errors (syntax, constraint, packet size) are not transient either — and when the
// dialect recognises the error, its verdict closes the case.
//
// verdict may be nil, which means "no dialect was wired in": the typed-code
// short-circuit is skipped and only the neutral rules below decide. Every command
// path passes the backend it resolved, so a CLI run never takes that route.
func IsTransient(err error, verdict TransientVerdict) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, driver.ErrBadConn) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) {
		return true
	}

	if verdict != nil {
		if transient, decided := verdict(err); decided {
			return transient
		}
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
