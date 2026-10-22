package _import

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"github.com/pkg/errors"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/gomydump/internal/common"
	"github.com/gomydump/internal/config"
)

// Importer handles data import operations
type Importer struct {
	cfg   *config.ImportConfig
	db    *sql.DB
	msg   *common.MessageWriter
	stats *common.Stats

	// packetOnce resolves max_allowed_packet lazily on the first batched write,
	// so an import that never reaches the INSERT stage costs no extra round-trip.
	packetOnce    sync.Once
	maxBatchBytes int64
}

// NewImporter creates a new Importer instance
func NewImporter(cfg *config.ImportConfig) (*Importer, error) {
	// Build DSN using mysql.Config to properly escape special characters
	dsn := common.BuildDSN(&cfg.Config)

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, errors.Wrap(err, "failed to connect to database")
	}

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

	// Initialize message writer if configured
	var msg *common.MessageWriter
	if cfg.Messages != "" {
		msg, err = common.NewMessageWriter(cfg.Messages, "Import")
		if err != nil {
			db.Close()
			return nil, errors.Wrap(err, "failed to create message writer")
		}
	}

	return &Importer{
		cfg: cfg,
		db:  db,
		msg: msg,
	}, nil
}

// Close closes the importer and releases resources
func (imp *Importer) Close() error {
	if imp.msg != nil {
		imp.msg.Close()
	}
	if imp.db != nil {
		return imp.db.Close()
	}
	return nil
}

// Import performs the import operation
func (imp *Importer) Import(ctx context.Context) error {
	startTime := time.Now()
	imp.stats = common.NewStats()
	stopMon := common.StartMonitor(ctx, 5*time.Second, imp.stats, func(prev, curr common.StatsSnapshot) {
		// --quiet suppresses the periodic progress log line but keeps the
		// --messages file progress records (an explicit artifact).
		if !common.Quiet {
			common.Logger.Info("Import progress",
				"read", curr.Processed,
				"inserted", curr.Inserted,
				"updated", curr.Updated,
				"failed", curr.Failed,
				"rate", fmt.Sprintf("%.2f rows/sec", curr.RatePerSec()),
				"interval_rate", fmt.Sprintf("%.2f rows/sec", curr.IntervalRate(prev)),
				"elapsed", curr.Elapsed.Truncate(time.Second),
			)
		}
		if imp.msg != nil {
			imp.msg.WriteProgress(curr.Processed)
		}
	})
	defer stopMon()

	// 输出配置参数
	common.Logger.Info("import configuration",
		"host", imp.cfg.Host,
		"port", imp.cfg.Port,
		"user", imp.cfg.User,
		"database", imp.cfg.Database,
		"input", imp.cfg.Input,
		"table", imp.cfg.Table,
		"format", imp.cfg.Format,
		"mode", imp.cfg.Mode,
		"threads", imp.cfg.ImportThreads,
		"skip_count", imp.cfg.SkipCount,
		"row_count", imp.cfg.RowCount,
		"commit_count", imp.cfg.CommitCount,
		"char_del", imp.cfg.DelModifier.CharDel,
		"col_del", imp.cfg.DelModifier.ColDel,
		"row_del", imp.cfg.DelModifier.RowDel,
		"escape", imp.cfg.DelModifier.Escape,
		"default_charset", imp.cfg.DefaultCharset,
		"timestamp_fmt", imp.cfg.DelModifier.TimestampFmt,
		"no_col_names", imp.cfg.DelModifier.NoColNames,
		"batch_size", imp.cfg.BatchSize,
		"pipeline_buffer", imp.cfg.PipelineBuffer,
		"columns", imp.cfg.Columns,
		"log_level", imp.cfg.LogLevel,
	)

	if imp.msg != nil {
		imp.msg.WriteHeader(fmt.Sprintf("import -i %s -t %s", imp.cfg.Input, imp.cfg.Table))
	}

	common.Logger.Info("Starting import", "input", imp.cfg.Input, "bytes", common.FileSize(imp.cfg.Input), "table", imp.cfg.Table, "format", imp.cfg.Format, "mode", imp.cfg.Mode, "threads", imp.cfg.ImportThreads)

	// Open input file
	file, err := os.Open(imp.cfg.Input)
	if err != nil {
		if imp.msg != nil {
			imp.msg.WriteMessage(fmt.Sprintf("SQL3104N  Failed to open input file: %v", err))
		}
		return errors.Wrap(err, "failed to open input file")
	}
	defer file.Close()

	// Create appropriate reader based on format
	var reader Reader
	switch imp.cfg.Format {
	case "del":
		reader = NewDelReader(bufio.NewReader(file), &imp.cfg.DelModifier, !imp.cfg.DelModifier.NoColNames)
	case "sql":
		reader = NewSQLReader(bufio.NewReader(file))
	case "parquet":
		parquetReader, err := NewParquetReader(file)
		if err != nil {
			return errors.Wrap(err, "failed to create parquet reader")
		}
		reader = parquetReader
	default:
		return errors.Errorf("unsupported format: %s", imp.cfg.Format)
	}

	// Read header/columns
	fileColumns, err := reader.ReadHeader()
	if err != nil {
		if imp.msg != nil {
			imp.msg.WriteMessage(fmt.Sprintf("SQL3104N  Failed to read header: %v", err))
		}
		return errors.Wrap(err, "failed to read header")
	}

	// Determine target columns: --columns > file columns
	// --columns supports two formats:
	// 1. Position-based: --columns "name,age,email" (file columns map to table columns by position)
	// 2. Name-based mapping: --columns "file_col1=table_col1,file_col2=table_col2" (explicit mapping)
	var targetColumns []string
	if imp.cfg.Columns != "" {
		if strings.Contains(imp.cfg.Columns, "=") {
			// Name-based mapping mode
			var columnMapping *ColumnMapping
			columnMapping, err = imp.applyColumnMapping(fileColumns)
			if err != nil {
				return errors.Wrap(err, "failed to apply column mapping")
			}
			targetColumns = columnMapping.TargetColumns
		} else {
			// Position-based mode
			targetColumns = strings.Split(imp.cfg.Columns, ",")
			for i, col := range targetColumns {
				targetColumns[i] = strings.TrimSpace(col)
			}
		}
	} else {
		targetColumns = fileColumns
	}

	if len(targetColumns) == 0 {
		return errors.New("no target columns available: the input file has no header row, specify --columns/-c with the target column list")
	}

	// Fail fast on target columns that do not exist on the table. DEL/Parquet
	// headers carry the original result-set column names, so an unaliased
	// expression column exported via --query (e.g. "SELECT 1, name ...")
	// would otherwise surface as a cryptic server error 1054 during INSERT —
	// and in replace mode only after the table had already been emptied.
	// SQL format is skipped: create mode legitimately runs before the table
	// exists, and its column list comes from the file's own INSERT statements.
	if imp.cfg.Format != "sql" {
		if err := imp.validateTargetColumns(ctx, targetColumns); err != nil {
			return err
		}
	}

	// Handle import modes
	switch imp.cfg.Mode {
	case config.ImportModeReplace:
		// Disable foreign key checks for replace mode
		if _, err := imp.db.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS=0"); err != nil {
			common.Logger.Warn("Failed to disable foreign key checks", "error", err)
		}
		defer func() {
			if _, err := imp.db.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS=1"); err != nil {
				common.Logger.Warn("Failed to re-enable foreign key checks", "error", err)
			}
		}()

		if err := imp.deleteTable(ctx); err != nil {
			if imp.msg != nil {
				imp.msg.WriteMessage(fmt.Sprintf("SQL3104N  Failed to delete table data: %v", err))
			}
			return errors.Wrap(err, "failed to delete table data")
		}
	case config.ImportModeCreate:
		if imp.cfg.Format != "sql" {
			return errors.New("create mode only supports SQL format")
		}
		// Execute CREATE TABLE statements if any. When -t is given, rewrite
		// each statement's target table name so the subsequent INSERT (built
		// from imp.cfg.Table) lands in the same table this created. When -t is
		// omitted, adopt the table name carried by the first CREATE statement.
		sqlReader, ok := reader.(*SQLReader)
		if ok {
			createStmts := sqlReader.GetCreateStatements()
			if imp.cfg.Table == "" {
				if len(createStmts) == 0 {
					return errors.New("create mode requires --table when the SQL file contains no CREATE TABLE statement")
				}
				name := extractCreateTableName(createStmts[0])
				if name == "" {
					return errors.New("failed to derive table name from CREATE statement; specify --table")
				}
				imp.cfg.Table = name
				common.Logger.Info("Create mode: table name derived from SQL file", "table", name)
			}
			for _, stmt := range createStmts {
				stmt = rewriteCreateTableTarget(stmt, imp.cfg.Table)
				taggedStmt := common.TagSQL(stmt)
				common.Logger.Info("Executing CREATE TABLE statement")
				if _, err := imp.db.ExecContext(ctx, taggedStmt); err != nil {
					preview := common.SQLPreview(taggedStmt, nil)
					common.Logger.Error("CREATE TABLE failed", "error", err, "sql", preview)
					if imp.msg != nil {
						imp.msg.WriteMessage(fmt.Sprintf("SQL3104N  Failed to execute CREATE TABLE: %v; SQL: %s", err, preview))
					}
					return errors.Wrapf(err, "failed to execute CREATE TABLE (SQL: %s)", preview)
				}
			}
		}
	}

	// For DEL import, teach the reader the target column kinds so the text
	// modifiers (--dec-pt/--date-fmt/--timestamp-fmt) reverse-transform only
	// numeric/temporal fields; string fields are never rewritten.
	if imp.cfg.Format == "del" {
		if delReader, ok := reader.(*DelReader); ok {
			kinds, err := imp.columnKindsFor(ctx, targetColumns)
			if err != nil {
				common.Logger.Warn("Failed to resolve target column types; DEL text modifiers disabled", "error", err)
			} else {
				delReader.SetColumnKinds(kinds)
			}
		}
	}

	// Choose parallel or single-threaded import
	var importErr error
	if imp.cfg.ImportThreads > 1 {
		importErr = imp.importParallel(ctx, reader, targetColumns, startTime)
	} else {
		importErr = imp.importSingle(ctx, reader, targetColumns, startTime)
	}
	if importErr != nil {
		// Aborted run: still surface what was accomplished before failing.
		total := imp.stats.Snapshot()
		common.Logger.Info("Import aborted",
			"read", total.Processed,
			"inserted", total.Inserted,
			"updated", total.Updated,
			"failed", total.Failed,
			"elapsed", total.Elapsed.Truncate(time.Second),
		)
	}
	return importErr
}

// validateTargetColumns checks that every target column exists on the table,
// producing a clear error instead of the server's raw 1054. A missing table is
// reported as such. Comparison is case-insensitive, matching MySQL semantics.
func (imp *Importer) validateTargetColumns(ctx context.Context, cols []string) error {
	if imp.cfg.Table == "" {
		return nil
	}
	var exists int
	err := imp.db.QueryRowContext(ctx,
		common.TagSQL("SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?"),
		imp.cfg.Database, imp.cfg.Table).Scan(&exists)
	if err != nil {
		return errors.Wrap(err, "failed to verify target table")
	}
	if exists == 0 {
		return errors.Errorf("target table %q does not exist in database %q", imp.cfg.Table, imp.cfg.Database)
	}

	placeholders := strings.Repeat("?,", len(cols))
	placeholders = placeholders[:len(placeholders)-1]
	q := common.TagSQL(fmt.Sprintf(
		"SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND UPPER(COLUMN_NAME) IN (%s)",
		placeholders,
	))
	args := make([]interface{}, 0, len(cols)+2)
	args = append(args, imp.cfg.Database, imp.cfg.Table)
	lowered := make(map[string]string, len(cols))
	for _, c := range cols {
		c = strings.TrimSpace(c)
		lowered[strings.ToUpper(c)] = c
		args = append(args, strings.ToUpper(c))
	}
	rows, err := imp.db.QueryContext(ctx, q, args...)
	if err != nil {
		return errors.Wrap(err, "failed to verify target columns")
	}
	defer rows.Close()

	present := make(map[string]bool, len(cols))
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return errors.Wrap(err, "failed to scan column name")
		}
		present[strings.ToUpper(name)] = true
	}
	if err := rows.Err(); err != nil {
		return errors.Wrap(err, "column verification iteration error")
	}

	var missing []string
	for _, c := range cols {
		key := strings.ToUpper(strings.TrimSpace(c))
		if !present[key] {
			missing = append(missing, lowered[key])
		}
	}
	if len(missing) > 0 {
		return errors.Errorf("target table %q has no column(s): %s — the input header must match table column names; if the file was produced by a --query containing unaliased expressions (e.g. SELECT 1, COUNT(*)), alias them (SELECT 1 AS flag) or specify --columns/-c with the target column list",
			imp.cfg.Table, strings.Join(missing, ", "))
	}
	return nil
}

// columnKindsFor resolves the ColumnKind of each target column (by name) from
// information_schema, returning a slice aligned to cols order. Unknown or
// missing columns map to common.KindUnknown.
func (imp *Importer) columnKindsFor(ctx context.Context, cols []string) ([]common.ColumnKind, error) {
	if len(cols) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(cols))
	placeholders = placeholders[:len(placeholders)-1]
	q := common.TagSQL(fmt.Sprintf(
		"SELECT COLUMN_NAME, DATA_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME IN (%s)",
		placeholders,
	))
	args := make([]interface{}, 0, len(cols)+2)
	args = append(args, imp.cfg.Database, imp.cfg.Table)
	for _, c := range cols {
		args = append(args, c)
	}
	rows, err := imp.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, errors.Wrap(err, "failed to query column types")
	}
	defer rows.Close()

	byName := make(map[string]string, len(cols))
	for rows.Next() {
		var name, dataType string
		if err := rows.Scan(&name, &dataType); err != nil {
			return nil, errors.Wrap(err, "failed to scan column type")
		}
		byName[strings.ToLower(name)] = dataType
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Wrap(err, "column type iteration error")
	}

	kinds := make([]common.ColumnKind, len(cols))
	for i, c := range cols {
		if dt, ok := byName[strings.ToLower(strings.TrimSpace(c))]; ok {
			kinds[i] = common.ClassifyColumnKind(dt)
		} else {
			kinds[i] = common.KindUnknown
		}
	}
	return kinds, nil
}

// addBatchResult records insert/update counters for one successfully applied
// batch, derived from MySQL's RowsAffected semantics:
//   - plain multi-row INSERT (insert / replace / create modes): 1 per row.
//   - INSERT ... ON DUPLICATE KEY UPDATE (insert_update mode): 1 per new row,
//     2 per changed existing row, 0 per existing row set to identical values.
//
// The "set to identical" case is indistinguishable from a failed count, so we
// assume it never happens: updated = affected - rows (clamped), inserted =
// rows - updated. Unchanged re-imports therefore surface as updated=0,
// which slightly undercounts true updates when identical rows exist.
func (imp *Importer) addBatchResult(rows int64, res sql.Result) {
	affected, err := res.RowsAffected()
	if err != nil || affected < 0 {
		affected = rows
	}
	if imp.cfg.Mode == config.ImportModeInsertUpdate {
		updated := affected - rows
		if updated < 0 {
			updated = 0
		}
		inserted := rows - updated
		if inserted < 0 {
			inserted = 0
		}
		imp.stats.AddInserted(inserted)
		imp.stats.AddUpdated(updated)
		return
	}
	imp.stats.AddInserted(affected)
}

// importSingle performs single-threaded import with pipeline optimization
func (imp *Importer) importSingle(ctx context.Context, reader Reader, columns []string, startTime time.Time) error {
	// Resolve the batching ceilings *before* the transaction is open. The byte
	// budget comes from a SHOW VARIABLES query on the pool, and holding a
	// transaction while waiting for a second connection is a deadlock risk if
	// the pool is ever limited to one connection.
	batchSize, maxBatchBytes := imp.resolveBatchSize(len(columns))

	// Start transaction
	tx, err := imp.db.BeginTx(ctx, nil)
	if err != nil {
		if imp.msg != nil {
			imp.msg.WriteMessage(fmt.Sprintf("SQL3104N  Failed to begin transaction: %v", err))
		}
		return errors.Wrap(err, "failed to begin transaction")
	}
	// Roll back whatever transaction is active when the function returns.
	// Committed transactions return a benign ErrTxDone, which we ignore.
	defer func() { _ = tx.Rollback() }()

	// Pipeline configuration
	rowCount := int64(0)
	skipCount := int64(imp.cfg.SkipCount)
	rowLimit := int64(imp.cfg.RowCount)
	commitCount := int64(imp.cfg.CommitCount)

	// Pipeline: Reader goroutine → Channel → Writer (main goroutine)
	type rowBatch struct {
		data [][]interface{}
		err  error
	}

	rowCh := make(chan rowBatch, imp.cfg.PipelineBuffer) // buffer pipeline-buffer batches
	errCh := make(chan error, 1)

	// Stage 1: Reader goroutine - 从文件读取数据
	go func() {
		defer close(rowCh)

		batch := make([][]interface{}, 0, batchSize)
		var batchBytes int64
		readCount := int64(0)

		for {
			values, err := reader.ReadRow()
			if err != nil {
				if err.Error() == "EOF" {
					// 发送剩余数据
					if len(batch) > 0 {
						select {
						case rowCh <- rowBatch{data: batch}:
						case <-ctx.Done():
							errCh <- ctx.Err()
							return
						}
					}
					return
				}
				errCh <- errors.Wrap(err, "failed to read row")
				return
			}

			readCount++

			// Skip rows if needed
			if readCount <= skipCount {
				continue
			}

			// Check row limit
			if rowLimit > 0 && (readCount-skipCount-1) >= rowLimit {
				// 发送剩余数据
				if len(batch) > 0 {
					select {
					case rowCh <- rowBatch{data: batch}:
					case <-ctx.Done():
						errCh <- ctx.Err()
						return
					}
				}
				return
			}

			imp.stats.AddProcessed(1)
			batch = append(batch, values)
			batchBytes += estimateRowBytes(values)

			// 批量发送：行数达到 --batch-size，或累计字节达到 max_allowed_packet
			// 的安全预算，任一条件满足就刷新。
			if shouldFlush(len(batch), batchSize, batchBytes, maxBatchBytes) {
				select {
				case rowCh <- rowBatch{data: batch}:
					batch = make([][]interface{}, 0, batchSize)
					batchBytes = 0
				case <-ctx.Done():
					errCh <- ctx.Err()
					return
				}
			}
		}
	}()

	// Stage 2: Writer (main goroutine) - 执行批量 INSERT
	//
	// The batch INSERT placeholder count must match the actual number of rows
	// in each batch (the final batch is usually smaller than batchSize), so the
	// statement is built per batch rather than prepared once for a fixed size.
	for batch := range rowCh {
		if len(batch.data) == 0 {
			continue
		}

		// 构建批量插入的参数
		batchValues := make([]interface{}, 0, len(batch.data)*len(columns))
		for _, row := range batch.data {
			batchValues = append(batchValues, row...)
		}

		insertSQL := imp.buildBatchInsertSQL(columns, len(batch.data))

		// 执行批量插入
		res, err := tx.ExecContext(ctx, insertSQL, batchValues...)
		if err != nil {
			preview := common.FailingSQL(insertSQL, batchValues)
			imp.stats.AddFailed(int64(len(batch.data)))
			common.Logger.Error("Insert batch failed", "error", err, "sql", preview)
			if imp.msg != nil {
				imp.msg.WriteMessage(fmt.Sprintf("SQL3104N  Failed to insert batch at row %d: %v; SQL: %s", rowCount+1, err, preview))
			}
			return errors.Wrapf(err, "failed to insert batch (SQL: %s)", preview)
		}
		imp.addBatchResult(int64(len(batch.data)), res)

		rowCount += int64(len(batch.data))

		// Commit transaction periodically
		if rowCount%commitCount == 0 {
			if err := tx.Commit(); err != nil {
				if imp.msg != nil {
					imp.msg.WriteMessage(fmt.Sprintf("SQL3104N  Failed to commit transaction: %v", err))
				}
				return errors.Wrap(err, "failed to commit transaction")
			}
			tx, err = imp.db.BeginTx(ctx, nil)
			if err != nil {
				if imp.msg != nil {
					imp.msg.WriteMessage(fmt.Sprintf("SQL3104N  Failed to begin new transaction: %v", err))
				}
				return errors.Wrap(err, "failed to begin new transaction")
			}
		}
	}

	// 检查 reader goroutine 的错误
	select {
	case err := <-errCh:
		return err
	default:
	}

	// Final commit
	if err := tx.Commit(); err != nil {
		if imp.msg != nil {
			imp.msg.WriteMessage(fmt.Sprintf("SQL3104N  Failed to commit final transaction: %v", err))
		}
		return errors.Wrap(err, "failed to commit final transaction")
	}

	duration := time.Since(startTime)

	total := imp.stats.Snapshot()
	common.Logger.Info("Import completed",
		"read", total.Processed,
		"inserted", total.Inserted,
		"updated", total.Updated,
		"failed", total.Failed,
		"bytes", common.FileSize(imp.cfg.Input),
		"input", imp.cfg.Input,
		"duration", duration.Truncate(time.Millisecond),
		"rate", fmt.Sprintf("%.2f rows/sec", float64(total.Processed)/duration.Seconds()),
	)

	if imp.msg != nil {
		imp.msg.WriteFooter(rowCount)
	}

	return nil
}

// importParallel performs parallel import using multiple threads.
//
// It streams rows through a bounded channel (backpressure keeps memory usage
// proportional to the channel buffer, not the input size) and every worker
// writes multi-row batched INSERTs on its own connection. This mirrors the
// single-threaded pipeline's efficiency while spreading work across threads.
// The previous implementation read the whole file into memory and inserted one
// row per statement, which was slower and used ~10x the RSS on large inputs.
func (imp *Importer) importParallel(ctx context.Context, reader Reader, columns []string, startTime time.Time) error {
	numThreads := imp.cfg.ImportThreads

	// Cancellable child context so the first failure stops every goroutine.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	batchSize, maxBatchBytes := imp.resolveBatchSize(len(columns))
	skipCount := int64(imp.cfg.SkipCount)
	rowLimit := int64(imp.cfg.RowCount)
	commitCount := int64(imp.cfg.CommitCount)

	type rowBatch struct {
		data [][]interface{}
	}

	// Bounded hand-off channel: readers block once workers fall behind, which
	// caps in-memory rows at roughly (cap(batchCh) + numThreads) * batchSize.
	batchCh := make(chan rowBatch, imp.cfg.PipelineBuffer)
	errCh := make(chan error, numThreads+1)

	// Stage 1: single reader goroutine (the Reader is not safe for concurrent
	// use) reads rows, applies skip/limit, and emits fixed-size batches.
	go func() {
		defer close(batchCh)

		batch := make([][]interface{}, 0, batchSize)
		var batchBytes int64
		readCount := int64(0)

		for {
			values, err := reader.ReadRow()
			if err != nil {
				if err.Error() == "EOF" {
					if len(batch) > 0 {
						select {
						case batchCh <- rowBatch{data: batch}:
						case <-ctx.Done():
							errCh <- ctx.Err()
						}
					}
					return
				}
				errCh <- errors.Wrap(err, "failed to read row")
				cancel()
				return
			}

			readCount++
			if readCount <= skipCount {
				continue
			}
			if rowLimit > 0 && (readCount-skipCount-1) >= rowLimit {
				if len(batch) > 0 {
					select {
					case batchCh <- rowBatch{data: batch}:
					case <-ctx.Done():
						errCh <- ctx.Err()
					}
				}
				return
			}

			imp.stats.AddProcessed(1)
			batch = append(batch, values)
			batchBytes += estimateRowBytes(values)
			// 行数或字节预算任一达到上限就刷新，语义与单线程管线一致。
			if shouldFlush(len(batch), batchSize, batchBytes, maxBatchBytes) {
				select {
				case batchCh <- rowBatch{data: batch}:
					batch = make([][]interface{}, 0, batchSize)
					batchBytes = 0
				case <-ctx.Done():
					errCh <- ctx.Err()
					return
				}
			}
		}
	}()

	// Stage 2: worker goroutines, each with its own connection/transaction.
	var wg sync.WaitGroup

	// Build the DSN once for all workers (BuildDSN embeds charset/session vars).
	dsn := common.BuildDSN(&imp.cfg.Config)

	for i := 0; i < numThreads; i++ {
		wg.Add(1)
		go func(threadID int) {
			defer wg.Done()

			db, err := sql.Open("mysql", dsn)
			if err != nil {
				errCh <- errors.Wrapf(err, "thread %d: failed to connect", threadID)
				cancel()
				return
			}
			// One connection per worker: the pipeline hands each worker a full
			// batch to write at a time, so a single pooled conn is enough and
			// keeps the server-side connection count equal to numThreads.
			db.SetMaxOpenConns(1)
			defer db.Close()

			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				errCh <- errors.Wrapf(err, "thread %d: failed to begin transaction", threadID)
				cancel()
				return
			}
			// Committed txs return a benign ErrTxDone from Rollback.
			defer func() { _ = tx.Rollback() }()

			rowsInTx := int64(0)

			for batch := range batchCh {
				if len(batch.data) == 0 {
					continue
				}

				flat := make([]interface{}, 0, len(batch.data)*len(columns))
				for _, row := range batch.data {
					flat = append(flat, row...)
				}

				insertSQL := imp.buildBatchInsertSQL(columns, len(batch.data))
				res, err := tx.ExecContext(ctx, insertSQL, flat...)
				if err != nil {
					preview := common.FailingSQL(insertSQL, flat)
					imp.stats.AddFailed(int64(len(batch.data)))
					common.Logger.Error("Insert batch failed", "thread", threadID, "error", err, "sql", preview)
					errCh <- errors.Wrapf(err, "thread %d: failed to insert batch (SQL: %s)", threadID, preview)
					cancel()
					return
				}
				imp.addBatchResult(int64(len(batch.data)), res)

				rowsInTx += int64(len(batch.data))

				if rowsInTx >= commitCount {
					if err := tx.Commit(); err != nil {
						errCh <- errors.Wrapf(err, "thread %d: failed to commit transaction", threadID)
						cancel()
						return
					}
					tx, err = db.BeginTx(ctx, nil)
					if err != nil {
						errCh <- errors.Wrapf(err, "thread %d: failed to begin new transaction", threadID)
						cancel()
						return
					}
					rowsInTx = 0
				}

				// Honor cancellation promptly without busy-waiting.
				select {
				case <-ctx.Done():
					_ = tx.Commit()
					return
				default:
				}
			}

			// Final commit for this worker's last partial transaction.
			if err := tx.Commit(); err != nil {
				errCh <- errors.Wrapf(err, "thread %d: failed to commit final transaction", threadID)
				cancel()
				return
			}

			common.Logger.Info("Thread completed", "thread", threadID)
		}(i)
	}

	// Wait for workers, then surface the first error if any.
	wg.Wait()
	close(errCh)
	if err, ok := <-errCh; ok {
		return err
	}

	duration := time.Since(startTime)

	total := imp.stats.Snapshot()
	common.Logger.Info("Parallel import completed",
		"read", total.Processed,
		"inserted", total.Inserted,
		"updated", total.Updated,
		"failed", total.Failed,
		"bytes", common.FileSize(imp.cfg.Input),
		"input", imp.cfg.Input,
		"threads", numThreads,
		"duration", duration.Truncate(time.Millisecond),
		"rate", fmt.Sprintf("%.2f rows/sec", float64(total.Processed)/duration.Seconds()),
	)

	if imp.msg != nil {
		imp.msg.WriteFooter(total.Processed)
	}

	return nil
}

func (imp *Importer) deleteTable(ctx context.Context) error {
	query := common.TagSQL(fmt.Sprintf("DELETE FROM `%s`", imp.cfg.Table))
	_, err := imp.db.ExecContext(ctx, query)
	return err
}

// ColumnMapping represents the mapping from file columns to target table columns
type ColumnMapping struct {
	TargetColumns []string // Target column names in the order corresponding to file columns
}

// applyColumnMapping parses and validates column mapping
// Format: "file_col1=table_col1,file_col2=table_col2,..."
// Returns ColumnMapping for building INSERT statement
func (imp *Importer) applyColumnMapping(fileColumns []string) (*ColumnMapping, error) {
	// Parse mapping: file_col -> table_col
	mapping := make(map[string]string)
	pairs := strings.Split(imp.cfg.Columns, ",")
	for _, pair := range pairs {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			return nil, errors.Errorf("invalid column mapping format: %s (expected file_col=table_col)", pair)
		}
		fileCol := strings.TrimSpace(parts[0])
		tableCol := strings.TrimSpace(parts[1])
		mapping[fileCol] = tableCol
	}

	// Validate: all file columns must be mapped
	if len(mapping) != len(fileColumns) {
		return nil, errors.Errorf("column mapping count mismatch: file has %d columns, but mapping specifies %d columns",
			len(fileColumns), len(mapping))
	}

	// Build target column list in file column order
	targetColumns := make([]string, len(fileColumns))
	for i, fileCol := range fileColumns {
		tableCol, ok := mapping[fileCol]
		if !ok {
			return nil, errors.Errorf("file column '%s' is not mapped", fileCol)
		}
		targetColumns[i] = tableCol
	}

	common.Logger.Info("Column mapping applied",
		"file_columns", fileColumns,
		"target_columns", targetColumns,
		"mapping", imp.cfg.Columns)

	return &ColumnMapping{
		TargetColumns: targetColumns,
	}, nil
}

func (imp *Importer) buildBatchInsertSQL(columns []string, rowCount int) string {
	var sb strings.Builder

	// Trim whitespace from column names
	targetColumns := make([]string, len(columns))
	for i, col := range columns {
		targetColumns[i] = strings.TrimSpace(col)
	}

	sb.WriteString("INSERT INTO `")
	sb.WriteString(imp.cfg.Table)
	sb.WriteString("` (")

	for i, col := range targetColumns {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("`")
		sb.WriteString(col)
		sb.WriteString("`")
	}

	sb.WriteString(") VALUES ")

	// Build placeholder groups: (?,?,?),(?,?,?),(?,?,?) — one per row
	for i := 0; i < rowCount; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("(")
		for j := range targetColumns {
			if j > 0 {
				sb.WriteString(",")
			}
			sb.WriteString("?")
		}
		sb.WriteString(")")
	}

	// For insert_update mode, append ON DUPLICATE KEY UPDATE
	if imp.cfg.Mode == config.ImportModeInsertUpdate {
		sb.WriteString(" ON DUPLICATE KEY UPDATE ")
		for i, col := range targetColumns {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("`")
			sb.WriteString(col)
			sb.WriteString("` = VALUES(`")
			sb.WriteString(col)
			sb.WriteString("`)")
		}
	}

	return common.TagSQL(sb.String())
}

// createTableNameRe matches the table-name token that follows
// "CREATE [TEMPORARY] TABLE [IF NOT EXISTS]" (backtick-qualified or bare).
var createTableNameRe = regexp.MustCompile(
	"(?is)^(\\s*CREATE\\s+(?:TEMPORARY\\s+)?TABLE\\s+(?:IF\\s+NOT\\s+EXISTS\\s+)?)(\\x60[^\\x60]+\\x60|[\\w$]+)(.*)$",
)

// rewriteCreateTableTarget replaces the table name in a CREATE TABLE statement
// with target so the subsequent INSERT (built from imp.cfg.Table) lands in the
// same table this statement creates.
func rewriteCreateTableTarget(stmt, target string) string {
	if target == "" {
		return stmt
	}
	m := createTableNameRe.FindStringSubmatch(stmt)
	if m == nil {
		return stmt
	}
	name := m[2]
	if idx := strings.LastIndexByte(name, '.'); idx >= 0 {
		name = name[:idx+1] + "`" + target + "`"
	} else {
		name = "`" + target + "`"
	}
	return m[1] + name + m[3]
}

// extractCreateTableName returns the bare table name from a CREATE TABLE
// statement (schema qualification stripped, backticks removed), or "" when
// the statement doesn't match.
func extractCreateTableName(stmt string) string {
	m := createTableNameRe.FindStringSubmatch(stmt)
	if m == nil {
		return ""
	}
	name := strings.Trim(m[2], "`")
	if idx := strings.LastIndexByte(name, '.'); idx >= 0 {
		name = name[idx+1:]
	}
	return name
}
