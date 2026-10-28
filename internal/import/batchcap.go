package _import

import (
	"context"
	"database/sql"
	"math"
	"time"

	"github.com/imex/internal/common"
	"github.com/pkg/errors"
)

// maxPlaceholders is the hard ceiling of a MySQL prepared statement: the
// parameter count is transmitted as an unsigned 16-bit integer, so a batch may
// never carry more than 65535 `?` markers in total. A batch of rows with C
// columns therefore holds at most 65535/C rows, regardless of --batch-size.
const maxPlaceholders = 65535

// defaultMaxAllowedPacket is assumed when the server value cannot be read
// (permission errors, proxies that hide SHOW VARIABLES). It is deliberately
// conservative: under-splitting batches only costs a few more round-trips,
// while over-splitting nothing would fail mid-import. The number is also the
// OceanBase tenant default for V2.1.x/V2.2.x/V3.1.x/V3.2.x (4,194,304), so the
// fallback matches a real OB server rather than an invented one. V4.x documents
// 16,777,216 instead, and this constant cannot be made version-aware: the only
// way to learn which OB a server is runs through SHOW VARIABLES as well
// (version_comment), so the same failure that forces the fallback also hides the
// version.
const defaultMaxAllowedPacket int64 = 4 << 20

// packetSafetyMargin is the fraction of max_allowed_packet the batches stay
// under. What it has to absorb is everything estimateRowBytes does not count:
// the statement skeleton ("INSERT INTO `db`.`table` (`a`,`b`) VALUES "), the
// row separators, and the type/length prefixes the driver puts in front of each
// parameter on the binary protocol.
const packetSafetyMargin = 9 // keep batches at <= 9/10 of the packet limit

// driverInterpolationCap is the client-side ceiling the MySQL driver applies to
// an interpolated statement. go-sql-driver/mysql defaults Config.MaxAllowedPacket
// to 64 MiB, imex does not override it (internal/common.BuildDSN), and once the
// interpolated text passes that ceiling the driver returns driver.ErrSkip
// (connection.go: "if len(buf)+4 > mc.maxAllowedPacket"), which makes
// database/sql fall back to server-side prepared statements — a different
// protocol path than the one the batching design targets, and measurably slower
// (one 80 MiB statement took 2.078s where the same data in 8 MiB COM_QUERY
// batches took 1.195s). A server with a large max_allowed_packet therefore gets
// *less* suitable batches if nothing clamps the derived budget, which is what
// driverBatchBudget is for.
const driverInterpolationCap int64 = 64 << 20

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
// statement inside the driver's interpolation ceiling. It reuses the same safety
// margin as the server budget, so the statement skeleton and the row separators
// are absorbed the same way.
func driverBatchBudget() int64 {
	return batchByteLimit(driverInterpolationCap)
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
// rows*columns stays under the protocol's placeholder ceiling. The clamp is
// logged once by the caller because it changes throughput characteristics.
func effectiveBatchSize(batchSize, columns int) int {
	if columns <= 0 {
		return batchSize
	}
	max := maxPlaceholders / columns
	if max < 1 {
		// More columns than the protocol allows placeholders for. Emitting one
		// row per statement still fails, but it fails on the server rather than
		// spinning here with an empty batch.
		max = 1
	}
	if batchSize > max {
		return max
	}
	return batchSize
}

// shouldFlush reports whether a batch has reached one of its two ceilings: the
// configured row count or the byte budget derived from max_allowed_packet. Both
// are checked after a row is appended, so a single oversized row is shipped on
// its own rather than dropped or split.
func shouldFlush(rows, batchSize int, batchBytes, maxBatchBytes int64) bool {
	return rows >= batchSize || batchBytes >= maxBatchBytes
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

// fetchMaxAllowedPacket reads the server's packet ceiling. The value is a
// server-side property of the *target* database, and a batch that exceeds it
// fails with error 1153 ("Packet too large") mid-transaction — so the import
// side, which builds its own batches from parsed rows, has to respect it even
// though the source file may contain far larger statements.
func fetchMaxAllowedPacket(db *sql.DB) (int64, error) {
	var name string
	var value int64
	showSQL := "SHOW VARIABLES LIKE 'max_allowed_packet'"
	common.LogInteractionSQL(showSQL)
	row := db.QueryRow(common.TagSQL(showSQL))
	if err := row.Scan(&name, &value); err != nil {
		return defaultMaxAllowedPacket, err
	}
	if value <= 0 {
		return defaultMaxAllowedPacket, errors.Errorf("server reported max_allowed_packet=%d", value)
	}
	return value, nil
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
// whether the result is still inside what the driver will interpolate. Three
// ceilings compete: the server's max_allowed_packet (the only one that can be
// queried), an explicit --statement-size (which may only lower the result), and
// the driver's interpolation ceiling (see driverInterpolationCap), which the
// first two know nothing about.
//
// A server budget above the driver ceiling is a trap rather than headroom: the
// resulting batch leaves the interpolated COM_QUERY path and every statement
// becomes a server-side round trip of its own. The default is therefore clamped
// and reported as "driver_packet_cap". An explicit --statement-size above the
// ceiling is the user's own number, so it is honoured (aboveDriverCeiling tells
// the caller to report it) instead of being silently overridden.
func batchCeilings(packetBudget int64, statementSize int) (budget int64, limiting string, aboveDriverCeiling bool) {
	budget = applyStatementCap(packetBudget, statementSize)
	explicit := statementSize > 0 && int64(statementSize) < packetBudget
	limiting = "max_allowed_packet"
	if explicit {
		limiting = "statement-size"
	}
	if driverBudget := driverBatchBudget(); budget > driverBudget {
		if explicit {
			return budget, limiting, true
		}
		return driverBudget, "driver_packet_cap", false
	}
	return budget, limiting, false
}

// batchBudget returns the per-batch byte ceiling for the connected server,
// resolved once and reused for every batch of the run.
func (imp *Importer) batchBudget() int64 {
	imp.packetOnce.Do(func() {
		if imp.db == nil {
			imp.maxBatchBytes = applyStatementCap(batchByteLimit(defaultMaxAllowedPacket), imp.cfg.StatementSize)
			return
		}
		packet, err := fetchMaxAllowedPacket(imp.db)
		if err != nil {
			common.Logger.Warn("could not read max_allowed_packet, assuming conservative batch limit",
				"assumed_bytes", defaultMaxAllowedPacket, "error", err)
		}
		budget, limiting, aboveCeiling := batchCeilings(batchByteLimit(packet), imp.cfg.StatementSize)
		imp.maxBatchBytes = budget
		if aboveCeiling {
			common.Logger.Warn("--statement-size is above the driver interpolation ceiling, generated statements will fall back to server-side prepared statements",
				"statement_size", imp.cfg.StatementSize, "driver_ceiling_bytes", driverBatchBudget())
		}
		common.Logger.Info("import batch byte cap resolved",
			"max_allowed_packet", packet,
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
func (imp *Importer) resolveBatchSize(columns int) (int, int64) {
	batchSize := effectiveBatchSize(imp.cfg.BatchSize, columns)
	if batchSize != imp.cfg.BatchSize {
		common.Logger.Warn("lowering --batch-size to stay under the MySQL placeholder limit",
			"requested", imp.cfg.BatchSize, "effective", batchSize, "columns", columns,
			"limit", maxPlaceholders,
		)
	}
	budget := imp.batchBudget()
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
func warnIfThreadsExceedCapacity(ctx context.Context, db *sql.DB, threads int) {
	if db == nil || threads <= 1 {
		return
	}
	maxConns, err := common.MaxConnections(ctx, db)
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
