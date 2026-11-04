package _import

import (
	"context"
	"math"
	"time"

	"github.com/imex/internal/backend"
	"github.com/imex/internal/common"
)

// The ceilings that used to be constants in this file moved to the backend
// package with the PostgreSQL port, because they are not universal: the
// placeholder limit belongs to each protocol's parameter count, and the packet
// ceiling is a MySQL variable the PostgreSQL server does not have. The names are
// backend.MySQLPacketCeilingFallback and backend.MySQLDriverInterpolationCap.

// packetSafetyMargin is the fraction of the packet ceiling the batches stay
// under. What it has to absorb is everything estimateRowBytes does not count:
// the statement skeleton ("INSERT INTO `db`.`table` (`a`,`b`) VALUES "), the
// row separators, and the type/length prefixes the driver puts in front of each
// parameter on the binary protocol.
const packetSafetyMargin = 9 // keep batches at <= 9/10 of the packet limit

// maxQueuedBatchBytes is the memory envelope the hand-off queue is *reported*
// against. --pipeline-buffer counts batches and one batch can be as large as the
// byte budget, so the depth alone does not say how much parsed data the queue holds.
// The depth stays where the operator put it: measured on a 200,688,900-byte /
// 100,000-row import at --threads 32 with /usr/bin/time, on a server reporting the
// OceanBase V4.x default max_allowed_packet=16M (budget 15,099,494 B), three repeats
// of each peaked at 76,616-152,040 KB in 3.78-5.07s at depth 1024 and 122,740-149,412
// KB in 4.45-4.83s at the default depth 10. The ranges overlap: with a 15 MB budget a
// single batch is small enough that the reader cannot stay that far ahead of the
// writers, so depth no longer decides peak RSS (the same file on a 128 MB server did
// separate, 147,340-292,356 KB at depth 1024). The envelope only sets the threshold
// at which the run says out loud what the depth costs; it does not change the depth
// (see resolveQueueDepth). On the 16M bed that threshold is floor(1 GiB / 15,099,494)
// = 71 batches, measured silent at 71 and priced from 72. The shipped default (10
// batches, 150,994,940 B nominal here, 603,979,770 B even where the driver clamp
// decides) sits inside it, so a default run never hears about it.
const maxQueuedBatchBytes int64 = 1 << 30

// driverBatchBudget is the byte budget a batch must stay under to keep the
// statement inside the ceiling the *driver* imposes. It reuses the same safety
// margin as the server budget, so the statement skeleton and the row separators
// are absorbed the same way. For MySQL that ceiling is go-sql-driver's own
// Config.MaxAllowedPacket, which defaults to 64 MiB and which imex does not
// override (internal/backend/dsn_mysql.go): once the interpolated text passes it the
// driver returns driver.ErrSkip (connection.go: "if len(buf)+4 >
// mc.maxAllowedPacket"), which makes database/sql fall back to server-side
// prepared statements — a different protocol path than the one the batching
// design targets, and measurably slower (one 80 MiB statement took 2.078s where
// the same data in 8 MiB COM_QUERY batches took 1.195s). A server with a large
// max_allowed_packet therefore gets *less* suitable batches if nothing clamps the
// derived budget, which is what this is for. PostgreSQL reports the protocol cap
// as its own ceiling here, so the clamp is one it cannot exceed by construction.
func driverBatchBudget(be backend.Backend) int64 {
	return batchByteLimit(be.DriverStatementCap())
}

// queuedBytes is the most parsed data the hand-off queue can retain at the given
// depth: every queued batch is capped by the byte budget, so depth times that budget
// is the memory the configured depth actually asks for. It saturates instead of
// wrapping, because both sides of the product are user-scale numbers (depth up to
// 1024, an explicit --statement-size of any width).
func queuedBytes(depth int, maxBatchBytes int64) int64 {
	if depth <= 0 || maxBatchBytes <= 0 {
		return 0
	}
	if maxBatchBytes > int64(math.MaxInt64)/int64(depth) {
		return math.MaxInt64
	}
	return int64(depth) * maxBatchBytes
}

// effectiveBatchSize clamps the configured row batch size so that
// rows*columns stays under the protocol's placeholder ceiling, which each backend
// computes for its own parameter limit. The clamp is logged once by the caller
// because it changes throughput characteristics.
func (imp *Importer) effectiveBatchSize(batchSize, columns int) int {
	max, _ := imp.backend().BatchRowCap(columns)
	if max == 0 || batchSize <= max {
		return batchSize
	}
	return max
}

// shouldFlush reports whether a batch has reached one of its two ceilings: the
// configured row count or the byte budget derived from max_allowed_packet. Both
// are checked after a row is appended, so a single oversized row is shipped on
// its own rather than dropped or split.
func shouldFlush(rows, batchSize int, batchBytes, maxBatchBytes int64) bool {
	return rows >= batchSize || batchBytes >= maxBatchBytes
}

// wouldExceedBatch reports whether appending one more row would push the batch
// past its byte budget. The callers check it *before* the append and ship the
// current batch first, so the post-append check in shouldFlush only ever sees
// an over-budget batch when a single row alone is wider than the budget. An
// empty batch therefore never splits: there is nothing to ship ahead of the row,
// and sending an empty batch would be a wasted round trip.
//
// Checking after the append was enough for that single-row case but not for two:
// with the 15,099,494-byte budget derived from a 16 MiB max_allowed_packet, two
// rows of 8,388,608 and 8,388,606 bytes fit the budget one at a time yet left
// together as a 16,777,214-byte statement, and the server answered `Error 1153
// (08S01): Got a packet bigger than 'max_allowed_packet' bytes` with 0 rows
// committed (measured on a two-row table whose rows were 8,388,608 and
// 8,388,606 bytes).
func wouldExceedBatch(batchBytes, rowBytes, maxBatchBytes int64) bool {
	if maxBatchBytes <= 0 || batchBytes == 0 {
		return false
	}
	return batchBytes+rowBytes > maxBatchBytes
}

// escapable marks the bytes go-sql-driver doubles when it interpolates a
// parameter as a quoted string: NUL, \n, \r, \x1a, backslash and both quote
// characters each gain a leading backslash.
var escapable = [256]bool{
	0x00: true, '\n': true, '\r': true, '\x1a': true,
	'\\': true, '\'': true, '"': true,
}

// escapedLen returns the width of b once the driver has quoted it.
func escapedLen(b []byte) int64 {
	n := int64(len(b))
	for _, c := range b {
		if escapable[c] {
			n++
		}
	}
	return n
}

// estimateRowBytes approximates how many bytes of SQL text one row contributes
// to a batched INSERT. String and byte-slice widths are measured exactly
// including quoting/escaping, so the estimate tracks the real packet size
// instead of guessing; the remaining scalar types are bounded by their widest
// textual form.
func estimateRowBytes(row []interface{}) int64 {
	n := int64(4) // "(), " framing
	for _, v := range row {
		switch x := v.(type) {
		case nil:
			n += 4 // NULL
		case []byte:
			n += escapedLen(x) + 2
		case string:
			n += escapedLen([]byte(x)) + 2
		case time.Time:
			n += 30 // microsecond timestamps with zone offset
		case bool:
			n += 5
		case int, int32, int64, uint, uint32, uint64:
			n += 21
		case float32, float64:
			n += 25
		default:
			n += 24
		}
	}
	return n
}

// batchByteLimit converts a packet ceiling into the per-batch byte budget. There is
// deliberately no floor: a minimum batch size cannot be worth more than the ceiling
// it is derived from. Measured with the server at max_allowed_packet=39936, the
// previous 64 KiB floor produced batch_bytes=65536 — above the server's own limit —
// and the import failed with `Error 1153 (08S01): Got a packet bigger than
// 'max_allowed_packet' bytes` while landing 0 of the 5,000 rows. A small ceiling
// therefore means small batches, which is slow but correct, and shouldFlush's
// row-count ceiling plus its flush-after-append rule keep a single wide row going out
// alone either way.
func batchByteLimit(maxAllowedPacket int64) int64 {
	return maxAllowedPacket * int64(packetSafetyMargin) / 10
}

// applyStatementCap lowers the server-derived per-batch byte budget when the
// caller gave --statement-size on the import side. It only ever lowers it: a
// batch above max_allowed_packet*9/10 fails mid-import with error 1153, which is
// what the packet budget exists to prevent. A very small explicit value is honoured
// as given, because an explicit number is a deliberate choice, and a single row
// larger than the budget still goes out as its own statement (nothing here can split
// a row).
func applyStatementCap(packetBudget int64, statementSize int) int64 {
	if statementSize <= 0 || int64(statementSize) >= packetBudget {
		return packetBudget
	}
	return int64(statementSize)
}

// batchCeilings decides which ceiling governs the per-batch byte budget and
// whether the result is still inside what the driver will accept. Three ceilings
// compete: the server's packet ceiling (the only one that can be queried), an
// explicit --statement-size (which may only lower the result), and the driver's
// own statement ceiling (see driverBatchBudget), which the first two know nothing
// about.
//
// A server budget above the driver ceiling is a trap rather than headroom: the
// resulting batch leaves the interpolated COM_QUERY path and every statement
// becomes a server-side round trip of its own. The default is therefore clamped
// and reported as the backend's driver-cap name. An explicit --statement-size
// above the ceiling is the user's own number, so it is honoured
// (aboveDriverCeiling tells the caller to report it) instead of being silently
// overridden.
func batchCeilings(be backend.Backend, packetBudget int64, statementSize int) (budget int64, limiting string, aboveDriverCeiling bool) {
	budget = applyStatementCap(packetBudget, statementSize)
	explicit := statementSize > 0 && int64(statementSize) < packetBudget
	limiting = be.BatchWords().PacketCap
	if explicit {
		limiting = "statement-size"
	}
	if driverBudget := driverBatchBudget(be); budget > driverBudget {
		if explicit {
			return budget, limiting, true
		}
		return driverBudget, be.BatchWords().DriverCap, false
	}
	return budget, limiting, false
}

// batchBudget returns the per-batch byte ceiling for the connected server,
// resolved once and reused for every batch of the run. An unconnected importer
// (a library caller, or a unit test) gets the backend's assumed ceiling rather
// than no answer, because the batching code cannot run without a budget.
func (imp *Importer) batchBudget(ctx context.Context) int64 {
	imp.packetOnce.Do(func() {
		be := imp.backend()
		words := be.BatchWords()
		packet, err := be.PacketCeiling(ctx, imp.db)
		if err != nil {
			common.Logger.Warn(words.PacketUnknown, "assumed_bytes", packet, "error", err)
		}
		budget, limiting, aboveCeiling := batchCeilings(be, batchByteLimit(packet), imp.cfg.StatementSize)
		imp.maxBatchBytes = budget
		if aboveCeiling {
			common.Logger.Warn(words.DriverOver,
				"statement_size", imp.cfg.StatementSize, "driver_ceiling_bytes", driverBatchBudget(be))
		}
		common.Logger.Info("import batch byte cap resolved",
			words.PacketCap, packet,
			"batch_bytes", imp.maxBatchBytes,
			"statement_size", imp.cfg.StatementSize,
			"limiting_factor", limiting,
		)
	})
	return imp.maxBatchBytes
}

// resolveQueueDepth keeps the configured --pipeline-buffer exactly as given and
// reports what it costs when the retained total passes maxQueuedBatchBytes. Depth and
// --threads are separate knobs — one is a queue, the other is concurrency — and the
// queue is shared by the workers rather than per-worker, so the cost of a depth is
// bytes, not lost parallelism, and the only honest place to state it is the log. A
// negative depth (a library caller, since the CLI range is 0..1024) becomes the
// synchronous hand-off, because a channel cannot have negative capacity.
func (imp *Importer) resolveQueueDepth(maxBatchBytes int64) int {
	depth := imp.cfg.PipelineBuffer
	if depth < 0 {
		depth = 0
	}
	queued := queuedBytes(depth, maxBatchBytes)
	if queued > maxQueuedBatchBytes {
		common.Logger.Warn("the hand-off queue may retain more than the memory envelope, importing at the configured depth",
			"requested", depth, "batch_bytes", maxBatchBytes,
			"queued_bytes", queued, "envelope_bytes", maxQueuedBatchBytes,
			"hint", "lower --pipeline-buffer, or lower --statement-size to shrink each batch")
	}
	imp.queueDepth = depth
	return depth
}

// resolveBatchSize clamps --batch-size to the placeholder ceiling for this table
// and logs the clamp once, because it silently changes throughput behaviour.
func (imp *Importer) resolveBatchSize(ctx context.Context, columns int) (int, int64) {
	batchSize := imp.effectiveBatchSize(imp.cfg.BatchSize, columns)
	if batchSize != imp.cfg.BatchSize {
		_, limit := imp.backend().BatchRowCap(columns)
		common.Logger.Warn(imp.backend().BatchWords().BatchSizeClamp,
			"requested", imp.cfg.BatchSize, "effective", batchSize, "columns", columns,
			"limit", limit,
		)
	}
	budget := imp.batchBudget(ctx)
	imp.resolveQueueDepth(budget)
	return batchSize, budget
}

// warnIfThreadsExceedCapacity reports the mismatch between --threads and the
// server's connection ceiling before any worker starts. Every parallel writer
// holds one connection for the whole run, so requesting more workers than the
// server accepts is not a slowdown but a likely failure: the extra workers retry
// a bounded number of times, and the first one to give up cancels the run while
// the others have already committed rows. It is a warning rather than a refusal
// because max_connections can be raised by the DBA mid-run, and because a server
// that allows the connection still imports correctly.
func (imp *Importer) warnIfThreadsExceedCapacity(ctx context.Context, threads int) {
	if imp.db == nil || threads <= 1 {
		return
	}
	maxConns, err := common.MaxConnections(ctx, imp.db, imp.backend().MaxConnectionsSQL())
	if err != nil {
		common.Logger.Debug("could not read max_connections", "error", err)
		return
	}
	// +1 for the connection the import itself runs its metadata queries on.
	if need := threads + 1; maxConns > 0 && need > maxConns {
		common.Logger.Warn("workers exceed the server connection limit, workers that cannot connect will retry and may fail the run",
			"threads", threads, "connections_needed", need, "max_connections", maxConns,
			"hint", "lower --threads, or raise max_connections on the server")
	}
}

// warnIfReplayWindowScales reports the real memory envelope of the replay buffers.
// replayBudget divides one process-wide budget across writers but keeps a floor per
// writer, so past eight workers the retained total grows with the thread count
// instead of staying at the documented budget: 256 workers hold 2 GiB. The floor
// cannot simply be dropped (a worker's window would be smaller than one batch,
// making --commit-count a dead option), so the total is reported instead.
func warnIfReplayWindowScales(threads int) {
	perWriter := replayBudget(threads)
	total := int64(threads) * int64(perWriter)
	if threads > 1 && total > DefaultMaxPendingBytes {
		common.Logger.Warn("parallel import replay window scales with threads",
			"threads", threads, "per_writer_bytes", perWriter, "total_bytes", total,
			"budgeted_total_bytes", DefaultMaxPendingBytes)
	}
}
