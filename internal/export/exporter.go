package export

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/pkg/errors"
	"os"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

// Exporter handles data export operations
type Exporter struct {
	config    *config.ExportConfig
	db        *sql.DB
	sqlParser *SQLParser
	stats     *common.Stats

	// connectDur is the dial + ping + consistent-snapshot cost measured before
	// Export starts. It is reported as its own field because the run's duration
	// starts after it: on a remote server this is often the largest fixed cost
	// of a small export, and an operator reading "0.02s" for a five-row query
	// needs to see where the other second went.
	connectDur time.Duration

	// openShard overrides how a parallel export shard dials the server. Tests set
	// it to an in-process connector so the order in which a shard releases its
	// connection and hands over its tail bytes can be observed without a
	// database (see TestExportShard_ReleasesConnectionBeforePipeDrain).
	openShard func() (*sql.DB, error)

	// stopMonitor ends the periodic progress record. It is held on the struct
	// because the completion summary is logged by the export methods, which must
	// stop the monitor first: a progress record after "export completed" reads as
	// a run that is still going.
	stopMonitor func()
}

// NewExporter creates a new exporter instance
func NewExporter(cfg *config.ExportConfig) (*Exporter, error) {
	// The parameters go out before the connection is dialled: this is the record
	// an operator reads when the run cannot reach the server at all, so it must
	// not depend on the dial succeeding.
	logExportConfig(cfg)

	// Build DSN using mysql.Config to properly escape special characters
	dsn := common.BuildDSN(&cfg.Config)

	dialStart := time.Now()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, errors.Wrap(err, "failed to connect to database")
	}

	// Configure connection pool for optimal performance
	db.SetMaxOpenConns(1) // Single-threaded export uses one connection
	db.SetMaxIdleConns(1)
	// The consistent snapshot is started with a raw START TRANSACTION, so the
	// connection carrying it goes back to the idle pool between statements.
	// Ageing it would let database/sql close it as "idle", which silently rolls
	// the snapshot back and continues reading without consistency. Export pools
	// are therefore not aged; only import pools are (see common.ApplyPoolHardening).
	db.SetConnMaxIdleTime(0)
	db.SetConnMaxLifetime(0)

	// Test the connection. Only a failure a reconnect can heal is retried; a
	// server rejection (bad credentials, unknown database, unknown session
	// variable) returns on the first try — see common.PingWithRetry. Before this,
	// a typo in --session-var or a wrong password waited a fixed 6 seconds and was
	// reported as a connectivity failure.
	if pingErr := common.PingWithRetry(db); pingErr != nil {
		db.Close()
		return nil, pingErr
	}

	// Enable consistent snapshot for transactional consistency
	if _, err := db.Exec("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ"); err != nil {
		common.Logger.Warn("failed to set isolation level", "error", err)
	}
	if _, err := db.Exec("START TRANSACTION WITH CONSISTENT SNAPSHOT"); err != nil {
		common.Logger.Warn("failed to start consistent snapshot", "error", err)
	}

	return &Exporter{
		config:     cfg,
		db:         db,
		sqlParser:  NewSQLParser(),
		connectDur: time.Since(dialStart),
	}, nil
}

// Close closes the exporter and releases resources
func (e *Exporter) Close() error {
	if e.db != nil {
		return e.db.Close()
	}
	return nil
}

// Export performs the export operation
func (e *Exporter) Export(ctx context.Context) error {
	startTime := time.Now()
	e.stats = common.NewStats()
	e.stopMonitor = common.StartMonitor(ctx, e.config.EffectiveProgressInterval(), e.stats, func(prev, curr common.StatsSnapshot) {
		// --quiet suppresses the periodic progress record. --messages receives the
		// command log itself rather than a report of its own, so a suppressed
		// record is absent from the file as well.
		if !common.Quiet {
			common.Logger.Info("export progress",
				"rows", curr.Processed,
				"rate", fmt.Sprintf("%.2f rows/sec", curr.RatePerSec()),
				"interval_rate", fmt.Sprintf("%.2f rows/sec", curr.IntervalRate(prev)),
				"elapsed", common.Seconds(curr.Elapsed),
			)
		}
	})
	defer e.stopMonitor()

	// --format sql writes CREATE TABLE plus INSERT statements aimed at one
	// concrete table, so a join, CTE, or derived-table result cannot be
	// represented. Reject it here, before the output file is created.
	if e.config.Format == "sql" {
		if _, err := e.sqlParser.SimpleTableName(e.config.Query); err != nil {
			return err
		}
	}

	// Check if output file exists and --no-overwrite is set
	if e.config.NoOverwrite {
		if _, err := os.Stat(e.config.Output); err == nil {
			return errors.Errorf("output file %s already exists, remove --no-overwrite to overwrite", e.config.Output)
		}
	}

	// Check if parallel export is requested
	if e.config.Threads > 1 {
		return e.exportParallel(ctx, startTime)
	}

	// Single-threaded export
	return e.exportSingle(ctx, startTime)
}

// removeIncompleteOutput drops the output file a failed run left behind, so that
// both export paths keep the same promise: the file either holds the whole dump
// or is not there. It is only called after the output file has been created, so
// a failure before that point cannot delete a pre-existing file.
func (e *Exporter) removeIncompleteOutput() {
	switch err := os.Remove(e.config.Output); {
	case err == nil:
		common.Logger.Warn("incomplete output file removed", "output", e.config.Output)
	case os.IsNotExist(err):
		// Already gone: nothing to clean up.
	default:
		common.Logger.Warn("failed to remove incomplete output file", "output", e.config.Output, "error", err)
	}
}

// warnIfShardKeyIgnored says out loud what the single-threaded path does with a
// requested shard key: nothing. Only exportParallel samples a key to cut the query
// into ranges, so an operator who set --shard-key and ended up here — through
// --threads 1, or because a join/CTE or an unsplittable result fell back — would
// otherwise read the configuration record's shard_key field as a promise the run
// does not keep. It is a WARN and not a usage error because the run is still
// correct: the query it exports is the query that gets exported, only the
// parallelism was never going to happen. The sharded path is also the only one that
// validates the column, so an unknown key never reaches here as an error.
func (e *Exporter) warnIfShardKeyIgnored() {
	if e.config.ShardKey == "" {
		return
	}
	common.Logger.Warn("single-threaded export ignores --shard-key",
		"shard_key", e.config.ShardKey,
		"threads", e.config.Threads,
		"fallback", e.config.Threads > 1,
		"hint", "a shard key only drives a parallel export: raise --threads above 1 with a single-table query, or drop --shard-key")
}

// exportSingle performs single-threaded export with pipeline optimization
func (e *Exporter) exportSingle(ctx context.Context, startTime time.Time) (err error) {
	e.warnIfShardKeyIgnored()

	// For SQL format, pre-fetch the CREATE TABLE statement while the
	// connection is still free. Only when the dump was asked to carry it
	// (--create).
	var createStmt string
	if e.config.Format == "sql" && e.config.Create {
		schema, tableName, err := e.sqlParser.ExtractTableRef(e.config.Query)
		if err != nil {
			return errors.Wrap(err, "failed to extract table name")
		}
		createStmt, err = e.getCreateTableStatement(schema, tableName)
		if err != nil {
			return errors.Wrap(err, "failed to get CREATE TABLE statement")
		}
	}

	// Execute query with attribution tag
	taggedQuery := common.TagSQL(e.config.Query)
	common.LogInteractionSQL(e.config.Query)
	queryStart := time.Now()
	rows, err := e.db.QueryContext(ctx, taggedQuery)
	if err != nil {
		preview := common.SQLPreview(e.config.Query, nil)
		common.Logger.Error("query failed", "error", err, "sql", preview)
		return errors.Wrapf(err, "failed to execute query (SQL: %s)", preview)
	}
	defer rows.Close()

	// Get column information
	columns, err := rows.Columns()
	if err != nil {
		return errors.Wrap(err, "failed to get columns")
	}

	// YEAR normalization: MySQL/MariaDB text protocol sends YEAR(0000) as the
	// raw string "0". Re-importing "0" as a string literal triggers the server's
	// 2-digit YEAR rule ("0" -> 2000), silently corrupting the value. Detect
	// YEAR columns and rewrite the raw "0" to "0000" at export time; this is a
	// stable representation across all three formats (DEL, SQL, Parquet).
	colTypes, err := rows.ColumnTypes()
	if err != nil {
		return errors.Wrap(err, "failed to get column types")
	}
	isYear := make([]bool, len(columns))
	anyYear := false
	for i, ct := range colTypes {
		if strings.EqualFold(ct.DatabaseTypeName(), "YEAR") {
			isYear[i] = true
			anyYear = true
		}
	}

	// Compute effective hex-blob setting and log the decision.
	// HexBlob forces on; NoHexBlob forces off; both false means automatic
	// (hex enabled when charset is not byte-safe, i.e. not utf8mb4/utf8).
	binaryFlags := common.BinaryColumnFlags(colTypes)
	hex := effectiveHexBlob(e.config)
	if hex && !e.config.HexBlob && !e.config.NoHexBlob {
		// Auto-enabled by charset: log info
		common.Logger.Info("hex-blob auto-enabled by charset", "charset", e.config.DefaultCharset)
	}
	if e.config.NoHexBlob && config.HexBlobForCharset(e.config.DefaultCharset) {
		// Explicit --no-hex-blob with non-utf8mb4 charset: warn about potential corruption
		common.Logger.Warn("hex-blob explicitly disabled with non-utf8mb4 charset; raw binary bytes may be corrupted by charset conversion", "charset", e.config.DefaultCharset)
	}

	// Create output file
	outputFile, err := os.Create(e.config.Output)
	if err != nil {
		return errors.Wrap(err, "failed to create output file")
	}
	// A dump is either complete or absent. Once the output file exists, every
	// later failure has to take it along, exactly as the parallel path already
	// does: a truncated file carries the dump's name and looks finished. This
	// defer is registered before the Close below so that Close runs first, which
	// is what lets the removal succeed on Windows as well.
	defer func() {
		if err != nil {
			e.removeIncompleteOutput()
		}
	}()

	defer outputFile.Close()

	// Create appropriate writer based on format
	var writer Writer
	switch e.config.Format {
	case "del":
		delWriter := NewDelWriter(outputFile, &e.config.DelModifier, !e.config.DelModifier.NoColNames, common.ApplyColumnKinds(colTypes))
		delWriter.SetBinaryFormat(binaryFlags, hex)
		writer = delWriter
	case "sql":
		tableName, err := e.sqlParser.ExtractTableName(e.config.Query)
		if err != nil {
			return errors.Wrap(err, "failed to extract table name")
		}
		sqlWriter := NewSQLWriter(outputFile, tableName, columns, e.config.StatementSize, e.config.Create, !e.config.NoData, createStmt, e.config.DefaultCharset)
		sqlWriter.SetBinaryFormat(binaryFlags, hex)
		sqlWriter.SetSingleRowInsert(!e.config.ExtendedInsert)
		writer = sqlWriter
	case "parquet":
		parquetWriter, err := NewParquetWriter(outputFile, columns, binaryFlags, e.config.ParquetCompression)
		if err != nil {
			return errors.Wrap(err, "failed to create parquet writer")
		}
		writer = parquetWriter
	default:
		return errors.Errorf("unsupported format: %s", e.config.Format)
	}

	// Write header/columns
	if err := writer.WriteHeader(columns); err != nil {
		return errors.Wrap(err, "failed to write header")
	}

	// Pipeline: Reader goroutine → Channel → Writer (main goroutine)
	type rowBatch struct {
		data [][]sql.RawBytes
		err  error
	}

	batchSize := e.config.BatchSize
	if batchSize <= 0 {
		batchSize = config.DefaultBatchSize
	}
	pipelineBuffer := e.config.PipelineBuffer
	if pipelineBuffer < 0 {
		pipelineBuffer = config.DefaultPipelineBuffer
	}
	rowCh := make(chan rowBatch, pipelineBuffer)
	errCh := make(chan error, 1)

	// firstRowAt is set once, by the reader, on the first row the server hands
	// back: the gap between executing the query and that moment is the query's
	// own latency (a filesort, a lock wait, a cold buffer pool), which is
	// invisible in a single end-to-end duration. The write below reads it after
	// rowCh is closed, so the channel close is the only synchronisation needed.
	var firstRowAt time.Time

	// Stage 1: Reader goroutine - 从数据库读取数据
	//
	// sql.RawBytes is only valid until the next rows.Next() call because the
	// driver reuses its read buffer. Since rows are handed off to another
	// goroutine through a channel, every value must be copied out here before
	// it is sent, otherwise the writer later observes the last row's bytes.
	go func() {
		defer close(rowCh)

		stall := common.NewStallWatch(scanStallThreshold)
		seen := int64(0)

		batch := make([][]sql.RawBytes, 0, batchSize)
		valuePtrs := make([]interface{}, len(columns))
		rawValues := make([]sql.RawBytes, len(columns))
		for i := range rawValues {
			valuePtrs[i] = &rawValues[i]
		}

		for rows.Next() {
			seen++
			if gap, stalled := stall.Observe(); stalled {
				common.Logger.Warn("export scan stalled",
					"rows_so_far", seen, "gap", common.Seconds(gap),
					"hint", "no row arrived for this long: a filesort, a lock wait or a stalled socket is holding the scan")
			}
			if firstRowAt.IsZero() {
				firstRowAt = time.Now()
			}
			if err := rows.Scan(valuePtrs...); err != nil {
				errCh <- errors.Wrap(err, "failed to scan row")
				return
			}

			// Copy each RawBytes into its own backing array. nil stays nil (NULL).
			rowData := make([]sql.RawBytes, len(columns))
			for i := range columns {
				if rawValues[i] != nil {
					buf := make([]byte, len(rawValues[i]))
					copy(buf, rawValues[i])
					// Normalize YEAR(0000): the text protocol returns "0", which
					// re-imports as 2000. Rewrite the bare "0" to "0000".
					if anyYear && isYear[i] && len(buf) == 1 && buf[0] == '0' {
						buf = []byte("0000")
					}
					rowData[i] = buf
				}
			}

			batch = append(batch, rowData)

			// 批量发送
			if len(batch) >= batchSize {
				select {
				case rowCh <- rowBatch{data: batch}:
					batch = make([][]sql.RawBytes, 0, batchSize)
				case <-ctx.Done():
					errCh <- ctx.Err()
					return
				}
			}
		}

		// 发送剩余数据
		if len(batch) > 0 {
			select {
			case rowCh <- rowBatch{data: batch}:
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
		}

		// 检查迭代错误
		if err := rows.Err(); err != nil {
			errCh <- errors.Wrap(err, "row iteration error")
		}
	}()

	// Stage 2: Writer (main goroutine) - 写入文件
	rowCount := int64(0)

	for batch := range rowCh {
		for _, rowData := range batch.data {
			if err := writer.WriteRawRow(rowData); err != nil {
				return errors.Wrap(err, "failed to write row")
			}

			rowCount++
			e.stats.AddProcessed(1)
		}
	}

	// 检查 reader goroutine 的错误
	select {
	case err := <-errCh:
		return err
	default:
	}

	loopEnd := time.Now()

	// The footer and the close are timed as their own segment: a parquet footer
	// re-encodes the last row group and a close flushes whatever the OS still
	// holds, so on a large export or a network filesystem they can take longer
	// than the last million rows. Closing here (rather than only in the deferred
	// Close) is what makes that time attributable; the deferred Close then finds
	// the file already closed and is a no-op.
	finishStart := time.Now()
	if err := writer.WriteFooter(); err != nil {
		return errors.Wrap(err, "failed to write footer")
	}
	if cerr := outputFile.Close(); cerr != nil {
		return errors.Wrap(cerr, "failed to close output file")
	}
	finishDur := time.Since(finishStart)

	duration := time.Since(startTime)

	// The four segments partition the run: setup (connect, snapshot and metadata
	// up to the query), first_row (query execution, output file creation and
	// header write up to the first row), write (first row to the last row
	// written) and finish (footer + close). A slow query is therefore visible
	// even when it returns five rows.
	setupDur := queryStart.Sub(startTime)
	firstRowDur := loopEnd.Sub(queryStart)
	writeDur := time.Duration(0)
	if !firstRowAt.IsZero() {
		firstRowDur = firstRowAt.Sub(queryStart)
		writeDur = loopEnd.Sub(firstRowAt)
	}

	// Stop the progress ticker before the summary and take its drained counters:
	// a run must not end on a "progress" record, which reads as a job that is
	// still going.
	e.stopMonitor()

	common.Logger.Info("export completed", "rows", rowCount, "bytes", common.FileSize(e.config.Output), "output", e.config.Output, "duration", common.Seconds(duration), "rate", fmt.Sprintf("%.2f rows/sec", float64(rowCount)/duration.Seconds()),
		"connect", common.Seconds(e.connectDur), "setup", common.Seconds(setupDur),
		"first_row", common.Seconds(firstRowDur), "write", common.Seconds(writeDur), "finish", common.Seconds(finishDur))

	return nil
}

// exportParallel performs parallel export with multiple threads
func (e *Exporter) exportParallel(ctx context.Context, startTime time.Time) error {
	// Parquet shard files cannot be concatenated, so parallel export would
	// require re-encoding every row. Fall back to single-threaded export.
	if e.config.Format == "parquet" {
		common.Logger.Info("parallel export is not supported for parquet format, using single-threaded export")
		return e.exportSingle(ctx, startTime)
	}

	// Check if query has complex features that don't work well with sharding
	if hasComplex, feature := e.sqlParser.HasComplexFeatures(e.config.Query); hasComplex {
		common.Logger.Warn("query has complex features, falling back to single-threaded export", "feature", feature)
		return e.exportSingle(ctx, startTime)
	}

	// Extract table name from query using SQL parser
	tableName, err := e.sqlParser.ExtractTableName(e.config.Query)
	if err != nil {
		common.Logger.Warn("failed to extract table name, falling back to single-threaded export", "error", err)
		return e.exportSingle(ctx, startTime)
	}

	// Detect shard key. An explicit --shard-key that the server does not have is a
	// refused request, not a hint: silently exporting single-threaded would report
	// success while dropping the instruction, and the user would see a run that is
	// several times slower with no reason. Auto-detection keeps its fallback.
	shardInfo, err := DetectShardKey(e.db, tableName, e.config.ShardKey)
	if err != nil {
		if e.config.ShardKey != "" {
			return common.Usagef("invalid --shard-key %q for table %q: %v", e.config.ShardKey, tableName, err)
		}
		common.Logger.Warn("failed to detect shard key, falling back to single-threaded export", "error", err)
		return e.exportSingle(ctx, startTime)
	}

	common.Logger.Info("parallel export detected", "table", tableName, "shard_key", shardInfo.Columns, "threads", e.config.Threads)

	// Every shard worker holds its own connection, so a thread count above the
	// server's ceiling is a likely mid-run failure rather than a slowdown. The
	// CLI bound (1..256) knows nothing about the target, which is why this is read
	// from the server and reported here instead of being validated statically.
	if maxConns, err := common.MaxConnections(ctx, e.db); err == nil {
		if need := e.config.Threads + 1; maxConns > 0 && need > maxConns {
			common.Logger.Warn("workers exceed the server connection limit, workers that cannot connect will retry and may fail the export",
				"threads", e.config.Threads, "connections_needed", need, "max_connections", maxConns,
				"hint", "lower --threads, or raise max_connections on the server")
		}
	} else {
		common.Logger.Debug("could not read max_connections", "error", err)
	}

	// Generate shard ranges: more ranges than workers, so a range that turns out
	// bigger than the estimate is drained by a worker instead of by a whole slot.
	numRanges := shardRangeCount(e.config.Threads)
	shardRanges := planShardRanges(e.db, tableName, shardInfo, numRanges)
	if len(shardRanges) <= 1 {
		common.Logger.Warn("only 1 shard range, falling back to single-threaded export")
		return e.exportSingle(ctx, startTime)
	}
	if len(shardRanges) < numRanges {
		common.Logger.Info("shard ranges fewer than planned", "planned", numRanges, "ranges", len(shardRanges))
	}

	// The parallel path never reads --batch-size/--pipeline-buffer: shard
	// look-ahead comes from a fixed byte budget divided across the shards
	// (shardStreamQueueBlocks), not from those two. The CLI refuses the pair when
	// --threads>1 rather than dropping the instruction on the floor
	// (cmd.validateParallelExportOptions), so this notice is what a caller driving
	// the library directly gets instead of a refusal.
	if e.pipelineOptionsIgnoredByParallel() {
		blocks := shardStreamQueueBlocks(len(shardRanges))
		common.Logger.Info("parallel export ignores pipeline options",
			"batch_size", e.config.BatchSize,
			"pipeline_buffer", e.config.PipelineBuffer,
			"look_ahead_per_shard", fmt.Sprintf("%.1f MiB", float64(blocks*shardBlockSize)/(1<<20)),
			"applies_to", "single-threaded export only")
	}

	// Get columns from a sample query with attribution tag
	sampleQuery := e.config.Query + " LIMIT 0"
	common.LogInteractionSQL(sampleQuery)
	sampleRows, err := e.db.QueryContext(ctx, common.TagSQL(sampleQuery))
	if err != nil {
		return errors.Wrap(err, "failed to get columns")
	}
	columns, err := sampleRows.Columns()
	sampleRows.Close()
	if err != nil {
		return errors.Wrap(err, "failed to get columns")
	}

	// One pipe per shard: the shard goroutines serialize into it and the stream
	// writer below pulls from them in shard order, so the output is written once
	// instead of via per-shard temporary files.
	pipes := make([]*shardPipe, len(shardRanges))
	queueBlocks := shardStreamQueueBlocks(len(shardRanges))
	for i := range pipes {
		pipes[i] = newShardPipe(ctx, queueBlocks, shardBlockSize)
	}

	// Start the shard workers: threads goroutines claim ranges in ascending order
	// from a shared cursor. Claiming in order matters — the stream writer below
	// consumes the pipes in range order, so a worker must never sit on a late
	// range while the range the writer is waiting for is still unclaimed.
	type shardResult struct {
		workerID int
		shardIdx int
		rowCount int64
		duration time.Duration
		err      error
	}
	resultCh := make(chan shardResult, len(shardRanges))

	workers := e.config.Threads
	if workers > len(shardRanges) {
		workers = len(shardRanges)
	}
	nextRange := int64(-1)
	for w := 0; w < workers; w++ {
		go func(workerID int) {
			for {
				idx := int(atomic.AddInt64(&nextRange, 1))
				if idx >= len(shardRanges) {
					return
				}
				// The shard's wall time runs from claiming the range to handing
				// over its last block, so it includes waiting for the stream
				// writer to drain an earlier shard's pipe. That is the number an
				// operator needs when one slow range holds the output back.
				shardStart := time.Now()
				rowCount, err := e.exportShard(ctx, workerID, idx, shardInfo, shardRanges[idx], columns, pipes[idx])
				resultCh <- shardResult{workerID: workerID, shardIdx: idx, rowCount: rowCount, duration: time.Since(shardStart), err: err}
			}
		}(w)
	}

	// Everything up to the first row of the first shard — detection, sampling,
	// range planning, the column sample and pipe allocation — is the parallel
	// path's setup segment.
	setupDur := time.Since(startTime)

	// Consume the shards in order while they are still running.
	streamErr := e.writeShardStream(pipes, columns)
	if streamErr != nil {
		common.Logger.Error("shard stream failed", "error", streamErr)
		for _, pipe := range pipes {
			pipe.terminate()
		}
	}

	// Collect shard results. Every producer has returned by now: a shard only
	// ends when its pipe is closed, and the stream above consumed them all (or
	// released them after an error).
	totalRows := int64(0)
	exportErr := streamErr
	for i := 0; i < len(shardRanges); i++ {
		result := <-resultCh
		if result.err != nil {
			common.Logger.Error("shard export failed", "worker", result.workerID, "shard", result.shardIdx, "error", result.err)
			if exportErr == nil {
				exportErr = result.err
			}
		} else {
			totalRows += result.rowCount
			common.Logger.Info("shard export completed", "worker", result.workerID, "shard", result.shardIdx,
				"rows", result.rowCount, "duration", common.Seconds(result.duration),
				"rate", fmt.Sprintf("%.2f rows/sec", rowsPerSecond(result.rowCount, result.duration)))
		}
	}

	// Streaming means the output file already holds whatever was written before a
	// failure. A truncated dump is worse than no dump, because it looks complete:
	// drop it and say so. (Temporary files used to hide this by leaving the
	// previous output untouched.)
	if exportErr != nil {
		e.removeIncompleteOutput()
		return exportErr
	}

	duration := time.Since(startTime)

	// Stop the progress ticker before the summary and take its drained counters:
	// a run must not end on a "progress" record, which reads as a job that is
	// still going.
	e.stopMonitor()

	total := e.stats.Snapshot()
	common.Logger.Info("parallel export completed", "rows", total.Processed, "bytes", common.FileSize(e.config.Output), "output", e.config.Output, "shards", len(shardRanges), "workers", workers, "duration", common.Seconds(duration), "rate", fmt.Sprintf("%.2f rows/sec", total.RatePerSec()),
		"connect", common.Seconds(e.connectDur), "setup", common.Seconds(setupDur))

	return nil
}

// exportShard serializes one shard into its pipe; the stream writer owns the
// output file and consumes the pipes in shard order.
func (e *Exporter) exportShard(ctx context.Context, workerID int, shardIdx int, shardInfo *ShardInfo, shardRange ShardRange, columns []string, out *shardPipe) (rowCount int64, err error) {
	// Build shard-specific query using SQL parser
	shardQuery, err := e.sqlParser.BuildShardedQuery(e.config.Query, shardInfo, shardRange)
	if err != nil {
		return 0, errors.Wrap(err, "failed to build sharded query")
	}

	// Create separate database connection for this shard
	db, err := e.openShardConn()
	if err != nil {
		return 0, errors.Wrap(err, "failed to create shard connection")
	}

	// Execute shard query with attribution tag
	taggedShardQuery := common.TagSQL(shardQuery)
	if common.DebugEnabled() {
		common.Logger.Debug("db interaction", "worker", workerID, "shard", shardIdx, "sql", common.SQLPreview(shardQuery, nil))
	}
	rows, err := db.QueryContext(ctx, taggedShardQuery)
	if err != nil {
		preview := common.SQLPreview(shardQuery, nil)
		db.Close()
		common.Logger.Error("shard query failed", "worker", workerID, "shard", shardIdx, "error", err, "sql", preview)
		return 0, errors.Wrapf(err, "failed to execute shard query (worker %d, SQL: %s)", workerID, preview)
	}

	// releaseDB ends this shard's consistent snapshot. It has to happen before the
	// shard's remaining bytes are handed over: the stream writer consumes the
	// pipes in shard order, so a shard sitting behind a slow one can wait minutes
	// for its tail block to be accepted. By then every row is already serialised
	// into memory, so keeping the connection through that wait holds a read
	// transaction and an idle socket for nothing — and on a cloud link an LB/NAT
	// idle reaper can drop exactly that socket, turning a healthy export into a
	// failed one.
	dbReleased := false
	releaseDB := func() {
		if dbReleased {
			return
		}
		dbReleased = true
		rows.Close()
		db.Close()
	}

	// Serialize the shard into its pipe. The stream writer consumes pipes in shard
	// order, so it must see a close on every exit path — including failures — or
	// it waits for an EOF that never arrives. It is registered before releaseDB's
	// defer so the database is always the first thing given up.
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = errors.Wrapf(cerr, "failed to finish shard %d output", shardIdx)
		}
	}()
	defer releaseDB()

	// Detect YEAR columns so "0" (the wire text for YEAR(0000)) can be
	// normalized to "0000" before writing, matching the single-threaded path.
	colTypes, err := rows.ColumnTypes()
	if err != nil {
		return 0, errors.Wrap(err, "failed to get column types")
	}

	// Compute binary flags and hex setting for this shard
	binaryFlags := common.BinaryColumnFlags(colTypes)
	hex := effectiveHexBlob(e.config)

	// Create writer for this shard (writeCreate=false: the preamble and the
	// CREATE TABLE statement are written by the stream writer)
	var writer Writer
	switch e.config.Format {
	case "del":
		delWriter := NewDelWriter(out, &e.config.DelModifier, false, common.ApplyColumnKinds(colTypes))
		delWriter.SetBinaryFormat(binaryFlags, hex)
		writer = delWriter
	case "sql":
		tableName, err := e.sqlParser.ExtractTableName(e.config.Query)
		if err != nil {
			return 0, errors.Wrap(err, "failed to extract table name")
		}
		sqlWriter := NewSQLWriter(out, tableName, columns, e.config.StatementSize, false, !e.config.NoData, "", e.config.DefaultCharset)
		sqlWriter.SetBinaryFormat(binaryFlags, hex)
		sqlWriter.SetSingleRowInsert(!e.config.ExtendedInsert)
		writer = sqlWriter
	default:
		return 0, errors.Errorf("unsupported format: %s", e.config.Format)
	}

	// Read and write rows using sql.RawBytes to avoid per-value allocation
	rawValues := make([]sql.RawBytes, len(columns))
	valuePtrs := make([]interface{}, len(columns))
	for i := range rawValues {
		valuePtrs[i] = &rawValues[i]
	}

	isYear := make([]bool, len(columns))
	anyYear := false
	for i, ct := range colTypes {
		if strings.EqualFold(ct.DatabaseTypeName(), "YEAR") {
			isYear[i] = true
			anyYear = true
		}
	}

	stall := common.NewStallWatch(scanStallThreshold)
	seen := int64(0)
	for rows.Next() {
		seen++
		if gap, stalled := stall.Observe(); stalled {
			common.Logger.Warn("export scan stalled", "worker", workerID, "shard", shardIdx,
				"rows_so_far", seen, "gap", common.Seconds(gap),
				"hint", "no row arrived for this long: a filesort, a lock wait or a stalled socket is holding the scan")
		}
		if err := rows.Scan(valuePtrs...); err != nil {
			return 0, errors.Wrap(err, "failed to scan row")
		}

		if anyYear {
			for i := range columns {
				if isYear[i] && len(rawValues[i]) == 1 && rawValues[i][0] == '0' {
					rawValues[i] = sql.RawBytes("0000")
				}
			}
		}

		if err := writer.WriteRawRow(rawValues); err != nil {
			return 0, errors.Wrap(err, "failed to write row")
		}

		rowCount++
		e.stats.AddProcessed(1)
	}

	if err := rows.Err(); err != nil {
		return 0, errors.Wrap(err, "row iteration error")
	}

	// Every row of this shard is now in the writer's hands, so the snapshot is no
	// longer needed. Release it here rather than at return: the footer flush and
	// the pipe close below can both block until the stream writer reaches this
	// shard, and a SQL shard can have --statement-size bytes (8 MiB by default) of
	// buffered INSERT left to hand over.
	releaseDB()

	// Write footer
	if err := writer.WriteFooter(); err != nil {
		return 0, errors.Wrap(err, "failed to write footer")
	}

	return rowCount, nil
}

// pipelineOptionsIgnoredByParallel reports whether the caller moved the two
// hand-off options that only exportSingle reads. Comparing against the shipped
// defaults rather than asking cobra keeps the notice with the code that ignores
// the values, and stays quiet for a run that never touched them.
func (e *Exporter) pipelineOptionsIgnoredByParallel() bool {
	return e.config.BatchSize != config.DefaultBatchSize ||
		e.config.PipelineBuffer != config.DefaultPipelineBuffer
}

// openShardConn dials the connection one parallel shard reads through.
func (e *Exporter) openShardConn() (*sql.DB, error) {
	if e.openShard != nil {
		return e.openShard()
	}
	return e.createShardConnection()
}

// createShardConnection creates a new database connection for a shard
func (e *Exporter) createShardConnection() (*sql.DB, error) {
	// Build DSN using the shared Config
	dsn := common.BuildDSN(&e.config.Config)

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	// Same reason as the main export pool: this connection carries a consistent
	// snapshot opened with a raw START TRANSACTION, so it must not be aged.
	db.SetConnMaxIdleTime(0)
	db.SetConnMaxLifetime(0)

	// Test connection
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	// Set isolation level for consistent snapshot
	if _, err := db.Exec("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ"); err != nil {
		common.Logger.Warn("failed to set isolation level for shard", "error", err)
	}
	if _, err := db.Exec("START TRANSACTION WITH CONSISTENT SNAPSHOT"); err != nil {
		common.Logger.Warn("failed to start consistent snapshot for shard", "error", err)
	}

	return db, nil
}

// writeShardStream writes the final output file while the shards are still
// running: the preamble first, then every shard pipe in shard order, then the
// footer. Consuming the pipes one at a time is what fixes the byte order; the
// shards stay concurrent because each of them keeps producing until its own
// queue fills up.
func (e *Exporter) writeShardStream(pipes []*shardPipe, columns []string) error {
	// Create final output file
	outputFile, err := os.Create(e.config.Output)
	if err != nil {
		return errors.Wrap(err, "failed to create output file")
	}
	defer outputFile.Close()

	// Write header for DEL format or CREATE TABLE for SQL format
	var sqlWriter *SQLWriter
	switch e.config.Format {
	case "del":
		if !e.config.DelModifier.NoColNames {
			writer := NewDelWriter(outputFile, &e.config.DelModifier, true, nil)
			if err := writer.WriteHeader(columns); err != nil {
				return errors.Wrap(err, "failed to write header")
			}
			// Flush the buffered writer: the shard blocks below are written
			// straight to outputFile, so the header must hit disk first.
			if err := writer.Flush(); err != nil {
				return errors.Wrap(err, "failed to flush header")
			}
		}
	case "sql":
		// The output is opened with the same writer a serial export uses, so
		// a parallel dump can never drift from a single-threaded one in its
		// preamble or epilogue.
		schema, tableName, err := e.sqlParser.ExtractTableRef(e.config.Query)
		if err != nil {
			return errors.Wrap(err, "failed to extract table name")
		}
		var createStmt string
		if e.config.Create {
			createStmt, err = e.getCreateTableStatement(schema, tableName)
			if err != nil {
				return errors.Wrap(err, "failed to get CREATE TABLE statement")
			}
		}
		sqlWriter = NewSQLWriter(outputFile, tableName, columns, e.config.StatementSize,
			e.config.Create, !e.config.NoData, createStmt, e.config.DefaultCharset)
		if err := sqlWriter.WriteHeader(columns); err != nil {
			return errors.Wrap(err, "failed to write header")
		}
		// The shard blocks are written straight to outputFile, so the buffered
		// preamble has to hit disk first.
		if err := sqlWriter.Flush(); err != nil {
			return errors.Wrap(err, "failed to flush header")
		}
	}

	// Append each shard, in shard order, as its blocks arrive.
	for i, pipe := range pipes {
		for block := range pipe.ch {
			if _, err := outputFile.Write(block); err != nil {
				return errors.Wrapf(err, "failed to write shard %d output", i)
			}
		}
	}

	// Write footer for SQL format
	if sqlWriter != nil {
		if err := sqlWriter.WriteFooter(); err != nil {
			return errors.Wrap(err, "failed to write footer")
		}
	}

	return nil
}

// getCreateTableStatement gets the CREATE TABLE statement for a table. schema is
// the qualifier the query carried ("" when it named none) and is used only to
// look the table up: SHOW CREATE TABLE runs in the connection's own database
// when the query did not name one, and in the named schema otherwise. The
// returned text is the server's own definition and already targets the bare
// table, so it needs no re-qualification for the dump.
func (e *Exporter) getCreateTableStatement(schema, tableName string) (string, error) {
	target := "`" + tableName + "`"
	if schema != "" {
		target = "`" + schema + "`." + target
	}
	createQuery := "SHOW CREATE TABLE " + target
	common.LogInteractionSQL(createQuery)
	query := common.TagSQL(createQuery)
	row := e.db.QueryRowContext(context.Background(), query)

	var name, createStmt string
	if err := row.Scan(&name, &createStmt); err != nil {
		return "", err
	}

	return createStmt + ";", nil
}

// Writer interface for different export formats
type Writer interface {
	WriteHeader(columns []string) error
	WriteRow(values []interface{}) error
	WriteRawRow(values []sql.RawBytes) error
	WriteFooter() error
}
