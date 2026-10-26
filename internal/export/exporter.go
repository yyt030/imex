package export

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/pkg/errors"
	"os"
	"strings"
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

	// Test connection with retry
	var pingErr error
	for i := 0; i < 3; i++ {
		if pingErr = db.Ping(); pingErr == nil {
			break
		}
		common.Logger.Warn("database ping failed, retrying", "attempt", i+1, "error", pingErr)
		time.Sleep(time.Duration(i+1) * time.Second)
	}
	if pingErr != nil {
		db.Close()
		return nil, errors.Wrap(pingErr, "failed to ping database after 3 attempts")
	}

	// Enable consistent snapshot for transactional consistency
	if _, err := db.Exec("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ"); err != nil {
		common.Logger.Warn("failed to set isolation level", "error", err)
	}
	if _, err := db.Exec("START TRANSACTION WITH CONSISTENT SNAPSHOT"); err != nil {
		common.Logger.Warn("failed to start consistent snapshot", "error", err)
	}

	return &Exporter{
		config:    cfg,
		db:        db,
		sqlParser: NewSQLParser(),
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
				"elapsed", curr.Elapsed.Truncate(time.Second),
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

	// Check if output file exists and --no-replace is set
	if e.config.NoReplace {
		if _, err := os.Stat(e.config.Output); err == nil {
			return errors.Errorf("output file %s already exists, remove --no-replace to overwrite", e.config.Output)
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

// exportSingle performs single-threaded export with pipeline optimization
func (e *Exporter) exportSingle(ctx context.Context, startTime time.Time) (err error) {
	// Detect server type BEFORE opening the streaming cursor: the single
	// export connection (MaxOpenConns=1) is held for the whole result set,
	// so any metadata query issued after this point would deadlock.
	serverType, err := common.DetectServerType(e.db)
	if err != nil {
		return errors.Wrap(err, "failed to detect server type")
	}

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
		sqlWriter := NewSQLWriter(outputFile, tableName, columns, e.config.StatementSize, e.config.Create, !e.config.NoData, serverType, createStmt, e.config.DefaultCharset)
		sqlWriter.SetBinaryFormat(binaryFlags, hex)
		sqlWriter.SetSingleRowInsert(!e.config.ExtendedInsert)
		writer = sqlWriter
	case "parquet":
		parquetWriter, err := NewParquetWriter(outputFile, columns, binaryFlags)
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

	// Stage 1: Reader goroutine - 从数据库读取数据
	//
	// sql.RawBytes is only valid until the next rows.Next() call because the
	// driver reuses its read buffer. Since rows are handed off to another
	// goroutine through a channel, every value must be copied out here before
	// it is sent, otherwise the writer later observes the last row's bytes.
	go func() {
		defer close(rowCh)

		batch := make([][]sql.RawBytes, 0, batchSize)
		valuePtrs := make([]interface{}, len(columns))
		rawValues := make([]sql.RawBytes, len(columns))
		for i := range rawValues {
			valuePtrs[i] = &rawValues[i]
		}

		for rows.Next() {
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

	// Write footer
	if err := writer.WriteFooter(); err != nil {
		return errors.Wrap(err, "failed to write footer")
	}

	duration := time.Since(startTime)

	// Stop the progress ticker before the summary and take its drained counters:
	// a run must not end on a "progress" record, which reads as a job that is
	// still going.
	e.stopMonitor()

	common.Logger.Info("export completed", "rows", rowCount, "bytes", common.FileSize(e.config.Output), "output", e.config.Output, "duration", duration.Truncate(time.Millisecond), "rate", fmt.Sprintf("%.2f rows/sec", float64(rowCount)/duration.Seconds()))

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

	// Detect shard key
	shardInfo, err := DetectShardKey(e.db, tableName, e.config.ShardKey)
	if err != nil {
		common.Logger.Warn("failed to detect shard key, falling back to single-threaded export", "error", err)
		return e.exportSingle(ctx, startTime)
	}

	common.Logger.Info("parallel export detected", "table", tableName, "shard_key", shardInfo.Columns, "threads", e.config.Threads)

	// Generate shard ranges
	shardRanges := GenerateShardRanges(shardInfo, e.config.Threads)
	if len(shardRanges) <= 1 {
		common.Logger.Warn("only 1 shard range, falling back to single-threaded export")
		return e.exportSingle(ctx, startTime)
	}

	// The parallel path never reads --batch-size/--pipeline-buffer: shard
	// look-ahead comes from a fixed byte budget divided across the shards
	// (shardStreamQueueBlocks), not from those two. Report it, because otherwise
	// `--threads 8 --batch-size 50000` silently changes nothing.
	if e.pipelineOptionsIgnoredByParallel() {
		blocks := shardStreamQueueBlocks(len(shardRanges))
		common.Logger.Info("parallel export ignores pipeline options",
			"batch_size", e.config.BatchSize,
			"pipeline_buffer", e.config.PipelineBuffer,
			"look_ahead_per_shard", fmt.Sprintf("%.1f MiB", float64(blocks*shardBlockSize)/(1<<20)),
			"applies_to", "single-threaded export only")
	}

	// Detect server type
	serverType, err := common.DetectServerType(e.db)
	if err != nil {
		return errors.Wrap(err, "failed to detect server type")
	}

	// Get columns from a sample query with attribution tag
	sampleRows, err := e.db.QueryContext(ctx, common.TagSQL(e.config.Query+" LIMIT 0"))
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

	// Start parallel export goroutines
	type shardResult struct {
		shardIdx int
		rowCount int64
		err      error
	}
	resultCh := make(chan shardResult, len(shardRanges))

	for i, shardRange := range shardRanges {
		go func(shardIdx int, shardRange ShardRange) {
			rowCount, err := e.exportShard(ctx, shardIdx, shardInfo, shardRange, columns, serverType, pipes[shardIdx])
			resultCh <- shardResult{shardIdx: shardIdx, rowCount: rowCount, err: err}
		}(i, shardRange)
	}

	// Consume the shards in order while they are still running.
	streamErr := e.writeShardStream(pipes, columns, serverType)
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
			common.Logger.Error("shard export failed", "shard", result.shardIdx, "error", result.err)
			if exportErr == nil {
				exportErr = result.err
			}
		} else {
			totalRows += result.rowCount
			common.Logger.Info("shard export completed", "shard", result.shardIdx)
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
	common.Logger.Info("parallel export completed", "rows", total.Processed, "bytes", common.FileSize(e.config.Output), "output", e.config.Output, "shards", len(shardRanges), "duration", duration.Truncate(time.Millisecond), "rate", fmt.Sprintf("%.2f rows/sec", total.RatePerSec()))

	return nil
}

// exportShard serializes one shard into its pipe; the stream writer owns the
// output file and consumes the pipes in shard order.
func (e *Exporter) exportShard(ctx context.Context, shardIdx int, shardInfo *ShardInfo, shardRange ShardRange, columns []string, serverType common.ServerType, out *shardPipe) (rowCount int64, err error) {
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
	rows, err := db.QueryContext(ctx, taggedShardQuery)
	if err != nil {
		preview := common.SQLPreview(shardQuery, nil)
		db.Close()
		common.Logger.Error("shard query failed", "shard", shardIdx, "error", err, "sql", preview)
		return 0, errors.Wrapf(err, "failed to execute shard query (SQL: %s)", preview)
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

	// Create writer for this shard (writeCreate=false: preamble written by the
	// stream writer, which owns the one LOCK/UNLOCK pair and the header)
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
		sqlWriter := NewSQLWriter(out, tableName, columns, e.config.StatementSize, false, !e.config.NoData, serverType, "", e.config.DefaultCharset)
		sqlWriter.SetBinaryFormat(binaryFlags, hex)
		sqlWriter.SetSingleRowInsert(!e.config.ExtendedInsert)
		// A fragment of a parallel dump must not lock or unlock the table on its
		// own; the merge step wraps the concatenated result in one pair.
		sqlWriter.SetSkipTableLocks(true)
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

	for rows.Next() {
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
func (e *Exporter) writeShardStream(pipes []*shardPipe, columns []string, serverType common.ServerType) error {
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
			e.config.Create, !e.config.NoData, serverType, createStmt, e.config.DefaultCharset)
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
	query := common.TagSQL("SHOW CREATE TABLE " + target)
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
