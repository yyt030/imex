package _import

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/gomydump/internal/common"
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
// while over-splitting nothing would fail mid-import.
const defaultMaxAllowedPacket int64 = 4 << 20

// packetSafetyMargin is the fraction of max_allowed_packet the batches stay
// under. What it has to absorb is everything estimateRowBytes does not count:
// the statement skeleton ("INSERT INTO `db`.`table` (`a`,`b`) VALUES "), the
// row separators, and the type/length prefixes the driver puts in front of each
// parameter on the binary protocol.
const packetSafetyMargin = 9 // keep batches at <= 9/10 of the packet limit

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
	row := db.QueryRow(common.TagSQL("SHOW VARIABLES LIKE 'max_allowed_packet'"))
	if err := row.Scan(&name, &value); err != nil {
		return defaultMaxAllowedPacket, err
	}
	if value <= 0 {
		return defaultMaxAllowedPacket, errors.Errorf("server reported max_allowed_packet=%d", value)
	}
	return value, nil
}

// batchByteLimit converts a packet ceiling into the per-batch byte budget.
func batchByteLimit(maxAllowedPacket int64) int64 {
	limit := maxAllowedPacket * int64(packetSafetyMargin) / 10
	if limit < 64<<10 {
		limit = 64 << 10
	}
	return limit
}

// batchBudget returns the per-batch byte ceiling for the connected server,
// resolved once and reused for every batch of the run.
func (imp *Importer) batchBudget() int64 {
	imp.packetOnce.Do(func() {
		if imp.db == nil {
			imp.maxBatchBytes = batchByteLimit(defaultMaxAllowedPacket)
			return
		}
		packet, err := fetchMaxAllowedPacket(imp.db)
		if err != nil {
			common.Logger.Warn("could not read max_allowed_packet, assuming conservative batch limit",
				"assumed_bytes", defaultMaxAllowedPacket, "error", err)
			if imp.msg != nil {
				imp.msg.WriteWarning(fmt.Sprintf("Could not read max_allowed_packet (%v); assuming %d bytes per batch", err, defaultMaxAllowedPacket))
			}
		}
		imp.maxBatchBytes = batchByteLimit(packet)
		common.Logger.Info("import batch byte cap resolved",
			"max_allowed_packet", packet,
			"batch_bytes", imp.maxBatchBytes,
		)
	})
	return imp.maxBatchBytes
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
		if imp.msg != nil {
			imp.msg.WriteWarning(fmt.Sprintf("Lowering --batch-size from %d to %d: %d columns would exceed the %d-placeholder statement limit",
				imp.cfg.BatchSize, batchSize, columns, maxPlaceholders))
		}
	}
	return batchSize, imp.batchBudget()
}
