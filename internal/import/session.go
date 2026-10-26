package _import

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/imex/internal/common"
)

// DefaultIdleCommitTimeout is how long a writer waits for the next batch before
// committing the rows it already applied. On cloud networks an idle TCP
// connection can be reclaimed by an LB/NAT device; committing shrinks the
// uncommitted window and makes a lost connection replay far fewer rows.
const DefaultIdleCommitTimeout = 30 * time.Second

// DefaultMaxPendingBytes caps the memory retained for transaction replay.
// --commit-count bounds the number of uncommitted rows, but rows can carry
// wide BLOB/TEXT payloads, so a batch count alone does not bound memory. When
// the retained batches reach this many bytes the transaction is committed
// early, shrinking the replay window as well.
const DefaultMaxPendingBytes = 64 << 20

// pendingBatch is one INSERT that has been applied inside the current
// transaction but not yet committed. MySQL rolls the whole transaction back
// when the connection dies, so after a reconnect the writer replays these
// batches before retrying the failed one — otherwise the rows from earlier
// batches of the same transaction would be silently lost.
//
// Values are retained only for the current commit window (--commit-count rows,
// 1000 by default, or DefaultMaxPendingBytes of payload, whichever comes
// first). Readers return fresh strings/byte slices per row, so the retained
// values are not aliased by the input buffer.
type pendingBatch struct {
	sql    string
	values []interface{}
}

// batchSize estimates the memory a batch retains for replay. Only string and
// byte payloads dominate; other values contribute a small constant so that
// many tiny rows still advance the counter.
func batchSize(query string, values []interface{}) int {
	n := len(query)
	for _, v := range values {
		switch x := v.(type) {
		case string:
			n += len(x)
		case []byte:
			n += len(x)
		default:
			n += 16
		}
	}
	return n
}

// writeSession owns the transaction, the retry/replay bookkeeping and the
// commit policy for one writer (the single-threaded pipeline or one parallel
// worker). Both import paths use it so they share identical semantics: commit
// every --commit-count rows, on idle, and replay the open transaction after a
// transient connection loss.
type writeSession struct {
	imp             *Importer
	db              *sql.DB
	ctx             context.Context
	tx              *sql.Tx
	commitCount     int64
	rowsInTx        int64
	pending         []pendingBatch
	pendingBytes    int
	maxPendingBytes int
	label           string // log context: "thread 3" or "single-threaded"
	policy          common.RetryPolicy
}

func newWriteSession(imp *Importer, db *sql.DB, ctx context.Context, commitCount int64, label string) *writeSession {
	return &writeSession{
		imp:             imp,
		db:              db,
		ctx:             ctx,
		commitCount:     commitCount,
		maxPendingBytes: DefaultMaxPendingBytes,
		label:           label,
		policy:          common.DefaultRetryPolicy(),
	}
}

// begin opens the first transaction. Connection establishment itself is
// retried on transient failures: there is no state to replay yet.
func (s *writeSession) begin() error {
	return common.Retry(s.ctx, s.policy, func(int) error {
		tx, err := s.db.BeginTx(s.ctx, nil)
		if err != nil {
			return err
		}
		s.tx = tx
		return nil
	})
}

// write applies one multi-row INSERT and records it in the pending replay
// buffer. A transient connection loss is retried after reconnecting; the open
// transaction's earlier batches are replayed first.
func (s *writeSession) write(insertSQL string, values []interface{}, rows int) error {
	var res sql.Result
	err := common.Retry(s.ctx, s.policy, func(attempt int) error {
		if attempt > 1 {
			// The previous attempt failed with a connection-level error, so the
			// server rolled back the open transaction. Rebuild it from pending
			// before re-executing this batch.
			if err := s.reconnect(); err != nil {
				return err
			}
		}
		r, err := s.tx.ExecContext(s.ctx, insertSQL, values...)
		if err != nil {
			return err
		}
		res = r
		return nil
	})
	if err != nil {
		return err
	}

	s.imp.addBatchResult(int64(rows), res)
	s.rowsInTx += int64(rows)
	s.pending = append(s.pending, pendingBatch{sql: insertSQL, values: values})
	s.pendingBytes += batchSize(insertSQL, values)
	return nil
}

// reconnect discards the dead transaction, opens a new one and replays every
// uncommitted batch in order. The replay restarts from scratch if it fails,
// which is safe because the failed attempt was rolled back.
func (s *writeSession) reconnect() error {
	_ = s.tx.Rollback() // the connection is already gone; the error is expected

	tx, err := s.db.BeginTx(s.ctx, nil)
	if err != nil {
		return err
	}
	s.tx = tx

	for i, p := range s.pending {
		if _, err := s.tx.ExecContext(s.ctx, p.sql, p.values...); err != nil {
			_ = s.tx.Rollback()
			return fmt.Errorf("%s: replaying batch %d/%d after reconnect: %w",
				s.label, i+1, len(s.pending), err)
		}
	}
	return nil
}

// commit commits the open transaction. Commit is never retried: when the
// response is lost there is no way to tell whether the server applied it, and
// replaying could duplicate rows. The caller surfaces the error instead.
func (s *writeSession) commit() error {
	if err := s.tx.Commit(); err != nil {
		return err
	}
	s.rowsInTx = 0
	s.pending = nil
	s.pendingBytes = 0
	return nil
}

// maybeCommit commits once the transaction reaches the row limit or the replay
// buffer reaches its byte budget, then opens the next transaction.
func (s *writeSession) maybeCommit() error {
	if s.rowsInTx == 0 {
		return nil
	}
	rowLimit := s.commitCount > 0 && s.rowsInTx >= s.commitCount
	byteLimit := s.maxPendingBytes > 0 && s.pendingBytes >= s.maxPendingBytes
	if !rowLimit && !byteLimit {
		return nil
	}
	if err := s.commit(); err != nil {
		return err
	}
	if byteLimit && !rowLimit {
		common.Logger.Debug("committing transaction at replay buffer limit",
			"session", s.label, "bytes", DefaultMaxPendingBytes)
	}
	return s.begin()
}

// idleCommit commits a partial transaction that has been waiting for the next
// batch for too long. Nothing is done when the transaction is empty.
func (s *writeSession) idleCommit() error {
	if s.rowsInTx == 0 {
		return nil
	}
	rows := s.rowsInTx
	if err := s.commit(); err != nil {
		return err
	}
	common.Logger.Debug("committing idle transaction",
		"session", s.label, "rows", rows, "timeout", DefaultIdleCommitTimeout)
	return s.begin()
}

// rollback releases the open transaction on every exit path. After a commit it
// returns sql.ErrTxDone, which is benign.
func (s *writeSession) rollback() {
	if s.tx != nil {
		_ = s.tx.Rollback()
	}
}

// armIdleTimer stops timer and re-arms it for d, discarding any pending tick.
func armIdleTimer(timer *time.Timer, d time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}
