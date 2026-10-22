package export

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/pkg/errors"
	"io"
	"os"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/gomydump/internal/common"
	"github.com/gomydump/internal/config"
)

// Exporter handles data export operations
type Exporter struct {
	config    *config.ExportConfig
	db        *sql.DB
	msg       *common.MessageWriter
	sqlParser *SQLParser
	stats     *common.Stats
}

// NewExporter creates a new exporter instance
func NewExporter(cfg *config.ExportConfig) (*Exporter, error) {
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
		common.Logger.Warn("Database ping failed, retrying", "attempt", i+1, "error", pingErr)
		time.Sleep(time.Duration(i+1) * time.Second)
	}
	if pingErr != nil {
		db.Close()
		return nil, errors.Wrap(pingErr, "failed to ping database after 3 attempts")
	}

	// Enable consistent snapshot for transactional consistency
	if _, err := db.Exec("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ"); err != nil {
		common.Logger.Warn("Failed to set isolation level", "error", err)
	}
	if _, err := db.Exec("START TRANSACTION WITH CONSISTENT SNAPSHOT"); err != nil {
		common.Logger.Warn("Failed to start consistent snapshot", "error", err)
	}

	// Initialize message writer if configured
	var msg *common.MessageWriter
	if cfg.Messages != "" {
		msg, err = common.NewMessageWriter(cfg.Messages, "Export")
		if err != nil {
			db.Close()
			return nil, errors.Wrap(err, "failed to create message writer")
		}
	}

	return &Exporter{
		config:    cfg,
		db:        db,
		msg:       msg,
		sqlParser: NewSQLParser(),
	}, nil
}

// Close closes the exporter and releases resources
func (e *Exporter) Close() error {
	if e.msg != nil {
		e.msg.Close()
	}
	if e.db != nil {
		return e.db.Close()
	}
	return nil
}

// Export performs the export operation
func (e *Exporter) Export(ctx context.Context) error {
	startTime := time.Now()
	e.stats = common.NewStats()
	stopMon := common.StartMonitor(ctx, 5*time.Second, e.stats, func(prev, curr common.StatsSnapshot) {
		// --quiet suppresses the periodic progress log line but keeps the
		// --messages file progress records (an explicit artifact).
		if !common.Quiet {
			common.Logger.Info("Export progress",
				"rows", curr.Processed,
				"rate", fmt.Sprintf("%.2f rows/sec", curr.RatePerSec()),
				"interval_rate", fmt.Sprintf("%.2f rows/sec", curr.IntervalRate(prev)),
				"elapsed", curr.Elapsed.Truncate(time.Second),
			)
		}
		if e.msg != nil {
			e.msg.WriteProgress(curr.Processed)
		}
	})
	defer stopMon()

	// 输出配置参数
	common.Logger.Info("export configuration",
		"host", e.config.Host,
		"port", e.config.Port,
		"user", e.config.User,
		"database", e.config.Database,
		"output", e.config.Output,
		"format", e.config.Format,
		"query", e.config.Query,
		"threads", e.config.Threads,
		"shard_key", e.config.ShardKey,
		"statement_size", e.config.StatementSize,
		"no_create", e.config.NoCreate,
		"no_data", e.config.NoData,
		"no_replace", e.config.NoReplace,
		"char_del", e.config.DelModifier.CharDel,
		"col_del", e.config.DelModifier.ColDel,
		"row_del", e.config.DelModifier.RowDel,
		"escape", e.config.DelModifier.Escape,
		"default_charset", e.config.DefaultCharset,
		"timestamp_fmt", e.config.DelModifier.TimestampFmt,
		"no_col_names", e.config.DelModifier.NoColNames,
		"log_level", e.config.LogLevel,
		"batch_size", e.config.BatchSize,
		"pipeline_buffer", e.config.PipelineBuffer,
		"hex_blob", e.config.HexBlob,
	)

	// --format sql writes CREATE TABLE plus INSERT statements aimed at one
	// concrete table, so a join, CTE, or derived-table result cannot be
	// represented. Reject it here, before the output file is created.
	if e.config.Format == "sql" {
		if _, err := e.sqlParser.SimpleTableName(e.config.Query); err != nil {
			return err
		}
	}

	if e.msg != nil {
		e.msg.WriteHeader(fmt.Sprintf("export -o %s -q \"%s\"", e.config.Output, e.config.Query))
		e.msg.WriteBegin(e.messageTableName())
	}

	common.Logger.Info("Starting export", "query", e.config.Query, "format", e.config.Format, "threads", e.config.Threads)

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

// warningFunc returns a callback that mirrors writer warnings into the message
// file, or nil when no message file was requested.
func (e *Exporter) warningFunc() func(string) {
	if e.msg == nil {
		return nil
	}
	return func(msg string) { e.msg.WriteWarning(msg) }
}

// messageTableName returns the table named in the query for the message file,
// or "" when it cannot be derived (joins still report their first table).
func (e *Exporter) messageTableName() string {
	if name, err := e.sqlParser.ExtractTableName(e.config.Query); err == nil {
		return name
	}
	return ""
}

// exportSingle performs single-threaded export with pipeline optimization
func (e *Exporter) exportSingle(ctx context.Context, startTime time.Time) error {
	// Detect server type BEFORE opening the streaming cursor: the single
	// export connection (MaxOpenConns=1) is held for the whole result set,
	// so any metadata query issued after this point would deadlock.
	serverType, err := common.DetectServerType(e.db)
	if err != nil {
		return errors.Wrap(err, "failed to detect server type")
	}

	// For SQL format, pre-fetch the CREATE TABLE statement while the
	// connection is still free.
	var createStmt string
	if e.config.Format == "sql" && !e.config.NoCreate {
		tableName, err := e.sqlParser.SimpleTableName(e.config.Query)
		if err != nil {
			return errors.Wrap(err, "failed to extract table name")
		}
		createStmt, err = e.getCreateTableStatement(tableName)
		if err != nil {
			return errors.Wrap(err, "failed to get CREATE TABLE statement")
		}
	}

	// Execute query with attribution tag
	taggedQuery := common.TagSQL(e.config.Query)
	rows, err := e.db.QueryContext(ctx, taggedQuery)
	if err != nil {
		preview := common.SQLPreview(e.config.Query, nil)
		common.Logger.Error("Query failed", "error", err, "sql", preview)
		if e.msg != nil {
			e.msg.WriteMessage(fmt.Sprintf("SQL3104N  Query failed: %v; SQL: %s", err, preview))
		}
		return errors.Wrapf(err, "failed to execute query (SQL: %s)", preview)
	}
	defer rows.Close()

	// Get column information
	columns, err := rows.Columns()
	if err != nil {
		if e.msg != nil {
			e.msg.WriteMessage(fmt.Sprintf("SQL3104N  Failed to get columns: %v", err))
		}
		return errors.Wrap(err, "failed to get columns")
	}

	// YEAR normalization: MySQL/MariaDB text protocol sends YEAR(0000) as the
	// raw string "0". Re-importing "0" as a string literal triggers the server's
	// 2-digit YEAR rule ("0" -> 2000), silently corrupting the value. Detect
	// YEAR columns and rewrite the raw "0" to "0000" at export time; this is a
	// stable representation across all three formats (DEL, SQL, Parquet).
	colTypes, err := rows.ColumnTypes()
	if err != nil {
		if e.msg != nil {
			e.msg.WriteMessage(fmt.Sprintf("SQL3104N  Failed to get column types: %v", err))
		}
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
	hex := e.config.HexBlob || (!e.config.NoHexBlob && config.HexBlobForCharset(e.config.DefaultCharset))
	if hex && !e.config.HexBlob && !e.config.NoHexBlob {
		// Auto-enabled by charset: log info
		common.Logger.Info("hex-blob auto-enabled by charset", "charset", e.config.DefaultCharset)
	}
	if e.config.NoHexBlob && config.HexBlobForCharset(e.config.DefaultCharset) {
		// Explicit --no-hex-blob with non-utf8mb4 charset: warn about potential corruption
		common.Logger.Warn("hex-blob explicitly disabled with non-utf8mb4 charset; raw binary bytes may be corrupted by charset conversion", "charset", e.config.DefaultCharset)
		if e.msg != nil {
			e.msg.WriteWarning(fmt.Sprintf("hex-blob explicitly disabled with non-utf8mb4 charset %q; raw binary bytes may be corrupted by charset conversion", e.config.DefaultCharset))
		}
	}

	// Create output file
	outputFile, err := os.Create(e.config.Output)
	if err != nil {
		if e.msg != nil {
			e.msg.WriteMessage(fmt.Sprintf("SQL3104N  Failed to create output file: %v", err))
		}
		return errors.Wrap(err, "failed to create output file")
	}
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
		sqlWriter := NewSQLWriter(outputFile, tableName, columns, e.config.StatementSize, !e.config.NoCreate, !e.config.NoData, serverType, createStmt, e.config.DefaultCharset)
		sqlWriter.SetBinaryFormat(binaryFlags, hex)
		sqlWriter.SetSingleRowInsert(e.config.SingleRowInsert)
		sqlWriter.SetWarningFunc(e.warningFunc())
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
		if e.msg != nil {
			e.msg.WriteMessage(fmt.Sprintf("SQL3104N  Failed to write header: %v", err))
		}
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
				if e.msg != nil {
					e.msg.WriteMessage(fmt.Sprintf("SQL3104N  Failed to write row %d: %v", rowCount+1, err))
				}
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
		if e.msg != nil {
			e.msg.WriteMessage(fmt.Sprintf("SQL3104N  Failed to write footer: %v", err))
		}
		return errors.Wrap(err, "failed to write footer")
	}

	duration := time.Since(startTime)

	common.Logger.Info("Export completed", "rows", rowCount, "bytes", common.FileSize(e.config.Output), "output", e.config.Output, "duration", duration.Truncate(time.Millisecond), "rate", fmt.Sprintf("%.2f rows/sec", float64(rowCount)/duration.Seconds()))

	if e.msg != nil {
		e.msg.WriteFooter(rowCount)
	}

	return nil
}

// exportParallel performs parallel export with multiple threads
func (e *Exporter) exportParallel(ctx context.Context, startTime time.Time) error {
	// Parquet shard files cannot be concatenated, so parallel export would
	// require re-encoding every row. Fall back to single-threaded export.
	if e.config.Format == "parquet" {
		common.Logger.Info("Parallel export is not supported for parquet format, using single-threaded export")
		return e.exportSingle(ctx, startTime)
	}

	// Check if query has complex features that don't work well with sharding
	if hasComplex, feature := e.sqlParser.HasComplexFeatures(e.config.Query); hasComplex {
		common.Logger.Warn("Query has complex features, falling back to single-threaded export", "feature", feature)
		return e.exportSingle(ctx, startTime)
	}

	// Extract table name from query using SQL parser
	tableName, err := e.sqlParser.ExtractTableName(e.config.Query)
	if err != nil {
		common.Logger.Warn("Failed to extract table name, falling back to single-threaded export", "error", err)
		return e.exportSingle(ctx, startTime)
	}

	// Detect shard key
	shardInfo, err := DetectShardKey(e.db, tableName, e.config.ShardKey)
	if err != nil {
		common.Logger.Warn("Failed to detect shard key, falling back to single-threaded export", "error", err)
		return e.exportSingle(ctx, startTime)
	}

	common.Logger.Info("Parallel export detected", "table", tableName, "shard_key", shardInfo.Columns, "threads", e.config.Threads)

	// Generate shard ranges
	shardRanges := GenerateShardRanges(shardInfo, e.config.Threads)
	if len(shardRanges) <= 1 {
		common.Logger.Warn("Only 1 shard range, falling back to single-threaded export")
		return e.exportSingle(ctx, startTime)
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

	// Create temporary files for each shard
	tempFiles := make([]string, len(shardRanges))
	for i := range shardRanges {
		tempFile := fmt.Sprintf("%s.shard_%d.tmp", e.config.Output, i)
		tempFiles[i] = tempFile
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
			rowCount, err := e.exportShard(ctx, shardIdx, shardInfo, shardRange, columns, serverType)
			resultCh <- shardResult{shardIdx: shardIdx, rowCount: rowCount, err: err}
		}(i, shardRange)
	}

	// Wait for all shards to complete
	totalRows := int64(0)
	var exportErr error
	for i := 0; i < len(shardRanges); i++ {
		result := <-resultCh
		if result.err != nil {
			common.Logger.Error("Shard export failed", "shard", result.shardIdx, "error", result.err)
			if exportErr == nil {
				exportErr = result.err
			}
		} else {
			totalRows += result.rowCount
			common.Logger.Info("Shard export completed", "shard", result.shardIdx)
		}
	}

	// If any shard failed, clean up and return error
	if exportErr != nil {
		for _, tempFile := range tempFiles {
			os.Remove(tempFile)
		}
		return exportErr
	}

	// Merge temporary files into final output
	if err := e.mergeShardFiles(tempFiles, columns, serverType); err != nil {
		return errors.Wrap(err, "failed to merge shard files")
	}

	// Clean up temporary files
	for _, tempFile := range tempFiles {
		os.Remove(tempFile)
	}

	duration := time.Since(startTime)

	total := e.stats.Snapshot()
	common.Logger.Info("Parallel export completed", "rows", total.Processed, "bytes", common.FileSize(e.config.Output), "output", e.config.Output, "shards", len(shardRanges), "duration", duration.Truncate(time.Millisecond), "rate", fmt.Sprintf("%.2f rows/sec", total.RatePerSec()))

	if e.msg != nil {
		e.msg.WriteFooter(totalRows)
	}

	return nil
}

// exportShard exports a single shard to a temporary file
func (e *Exporter) exportShard(ctx context.Context, shardIdx int, shardInfo *ShardInfo, shardRange ShardRange, columns []string, serverType common.ServerType) (int64, error) {
	// Build shard-specific query using SQL parser
	shardQuery, err := e.sqlParser.BuildShardedQuery(e.config.Query, shardInfo, shardRange)
	if err != nil {
		return 0, errors.Wrap(err, "failed to build sharded query")
	}

	// Create separate database connection for this shard
	db, err := e.createShardConnection()
	if err != nil {
		return 0, errors.Wrap(err, "failed to create shard connection")
	}
	defer db.Close()

	// Execute shard query with attribution tag
	taggedShardQuery := common.TagSQL(shardQuery)
	rows, err := db.QueryContext(ctx, taggedShardQuery)
	if err != nil {
		preview := common.SQLPreview(shardQuery, nil)
		common.Logger.Error("Shard query failed", "shard", shardIdx, "error", err, "sql", preview)
		return 0, errors.Wrapf(err, "failed to execute shard query (SQL: %s)", preview)
	}
	defer rows.Close()

	// Create temporary file for this shard
	tempFile := fmt.Sprintf("%s.shard_%d.tmp", e.config.Output, shardIdx)
	outputFile, err := os.Create(tempFile)
	if err != nil {
		return 0, errors.Wrap(err, "failed to create temporary file")
	}
	defer outputFile.Close()

	// Detect YEAR columns so "0" (the wire text for YEAR(0000)) can be
	// normalized to "0000" before writing, matching the single-threaded path.
	colTypes, err := rows.ColumnTypes()
	if err != nil {
		return 0, errors.Wrap(err, "failed to get column types")
	}

	// Compute binary flags and hex setting for this shard
	binaryFlags := common.BinaryColumnFlags(colTypes)
	hex := e.config.HexBlob || (!e.config.NoHexBlob && config.HexBlobForCharset(e.config.DefaultCharset))

	// Create writer for this shard (writeCreate=false: header written by merge)
	var writer Writer
	switch e.config.Format {
	case "del":
		delWriter := NewDelWriter(outputFile, &e.config.DelModifier, false, common.ApplyColumnKinds(colTypes))
		delWriter.SetBinaryFormat(binaryFlags, hex)
		writer = delWriter
	case "sql":
		tableName, err := e.sqlParser.ExtractTableName(e.config.Query)
		if err != nil {
			return 0, errors.Wrap(err, "failed to extract table name")
		}
		sqlWriter := NewSQLWriter(outputFile, tableName, columns, e.config.StatementSize, false, !e.config.NoData, serverType, "", e.config.DefaultCharset)
		sqlWriter.SetBinaryFormat(binaryFlags, hex)
		sqlWriter.SetSingleRowInsert(e.config.SingleRowInsert)
		sqlWriter.SetWarningFunc(e.warningFunc())
		// A fragment of a parallel dump must not lock or unlock the table on its
		// own; the merge step wraps the concatenated result in one pair.
		sqlWriter.SetSkipTableLocks(true)
		writer = sqlWriter
	default:
		return 0, errors.Errorf("unsupported format: %s", e.config.Format)
	}

	// Read and write rows using sql.RawBytes to avoid per-value allocation
	rowCount := int64(0)
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

	// Write footer
	if err := writer.WriteFooter(); err != nil {
		return 0, errors.Wrap(err, "failed to write footer")
	}

	return rowCount, nil
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
		common.Logger.Warn("Failed to set isolation level for shard", "error", err)
	}
	if _, err := db.Exec("START TRANSACTION WITH CONSISTENT SNAPSHOT"); err != nil {
		common.Logger.Warn("Failed to start consistent snapshot for shard", "error", err)
	}

	return db, nil
}

// mergeShardFiles merges all shard temporary files into the final output file
func (e *Exporter) mergeShardFiles(tempFiles []string, columns []string, serverType common.ServerType) error {
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
			// Flush the buffered writer: the shard files below are copied
			// straight to outputFile, so the header must hit disk first.
			if err := writer.Flush(); err != nil {
				return errors.Wrap(err, "failed to flush header")
			}
		}
	case "sql":
		// The merged file is opened with the same writer a serial export uses, so
		// a parallel dump can never drift from a single-threaded one in its
		// preamble or epilogue.
		tableName, err := e.sqlParser.ExtractTableName(e.config.Query)
		if err != nil {
			return errors.Wrap(err, "failed to extract table name")
		}
		var createStmt string
		if !e.config.NoCreate {
			createStmt, err = e.getCreateTableStatement(tableName)
			if err != nil {
				return errors.Wrap(err, "failed to get CREATE TABLE statement")
			}
		}
		sqlWriter = NewSQLWriter(outputFile, tableName, columns, e.config.StatementSize,
			!e.config.NoCreate, !e.config.NoData, serverType, createStmt, e.config.DefaultCharset)
		if err := sqlWriter.WriteHeader(columns); err != nil {
			return errors.Wrap(err, "failed to write header")
		}
		// The shard files are copied straight to outputFile, so the buffered
		// preamble has to hit disk first.
		if err := sqlWriter.Flush(); err != nil {
			return errors.Wrap(err, "failed to flush header")
		}
	}

	// Append each shard file
	for _, tempFile := range tempFiles {
		shardFile, err := os.Open(tempFile)
		if err != nil {
			return errors.Wrap(err, "failed to open shard file")
		}

		if _, err := io.Copy(outputFile, shardFile); err != nil {
			shardFile.Close()
			return errors.Wrap(err, "failed to copy shard file")
		}
		shardFile.Close()
	}

	// Write footer for SQL format
	if sqlWriter != nil {
		if err := sqlWriter.WriteFooter(); err != nil {
			return errors.Wrap(err, "failed to write footer")
		}
	}

	return nil
}

// getCreateTableStatement gets the CREATE TABLE statement for a table
func (e *Exporter) getCreateTableStatement(tableName string) (string, error) {
	query := common.TagSQL(fmt.Sprintf("SHOW CREATE TABLE `%s`", tableName))
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
