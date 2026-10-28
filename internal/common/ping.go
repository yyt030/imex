package common

import (
	"database/sql"
	"time"

	"github.com/pkg/errors"
)

// pingRetryPolicy bounds the retries of the first db.Ping() of a run. It is a
// variable only so tests can collapse the backoff and still exercise every path.
var pingRetryPolicy = DefaultRetryPolicy()

// PingWithRetry verifies a freshly opened connection, retrying only what a
// reconnect can actually heal.
//
// The judge is IsTransient, the same one the statement paths use: a
// connection-level failure (server gone away, refused dial, reset socket) gets
// up to MaxAttempts tries with the shared backoff, while a server rejection
// returns after the first try. A rejection carries a number the server decided —
// 1045 credentials, 1049 unknown database, 1193 unknown system variable — and
// dberr.go already documents 1045 as one that "will not heal themselves".
// Retrying those three times cost a fixed 6 seconds per run and put a
// connectivity story in front of the operator's actual typo.
//
// The attempt count in the returned message is the number of tries actually
// made, so the wording stays true on both paths: "after 1 attempt" for a
// rejection, "after 3 attempts" for an outage that outlasts the backoff.
func PingWithRetry(db *sql.DB) error {
	p := pingRetryPolicy
	if p.MaxAttempts < 1 {
		p.MaxAttempts = 1
	}

	var err error
	attempts := 0
	for attempt := 1; attempt <= p.MaxAttempts; attempt++ {
		attempts = attempt
		if err = db.Ping(); err == nil {
			return nil
		}
		if !IsTransient(err) || attempt == p.MaxAttempts {
			// Either a retry cannot help, or there is no retry left. The error
			// itself is the last thing the run reports, so it does not also get a
			// "retrying" record: that record promises another try, and there is
			// not one coming.
			break
		}
		Logger.Warn("database ping failed, retrying", "attempt", attempt, "error", err)
		time.Sleep(p.Delay(attempt + 1))
	}

	word := "attempts"
	if attempts == 1 {
		word = "attempt"
	}
	return errors.Wrapf(err, "failed to ping database after %d %s", attempts, word)
}
