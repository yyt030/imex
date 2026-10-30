package _import

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"

	"github.com/pkg/errors"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

// Importer handles data import operations
type Importer struct {
	cfg   *config.ImportConfig
	db    *sql.DB
	stats *common.Stats

	// packetOnce resolves max_allowed_packet lazily on the first batched write,
	// so an import that never reaches the INSERT stage costs no extra round-trip.
	packetOnce    sync.Once
	maxBatchBytes int64

	// queueDepth is the configured --pipeline-buffer, kept exactly as the operator
	// wrote it; maxQueuedBatchBytes only decides when the run reports what that depth
	// costs in bytes (see resolveQueueDepth). It is resolved with the byte budget,
	// before either import path builds its channel.
	queueDepth int

	// stopMonitor ends the periodic progress record. It is held on the struct
	// because the summaries are logged by the import methods, which must stop the
	// monitor first: a progress record after "import completed" reads as a run
	// that is still going.
	stopMonitor func()

	// setupDur and headerDur split the time before the data path starts: setup
	// is the connection, metadata and mode preparation, header is opening the
	// input file and parsing its header (a parquet reader builds its schema
	// there). Both are set once, before either import path begins, and read
	// once by that path's summary.
	setupDur  time.Duration
	headerDur time.Duration

	// connectDur is the dial + ping cost measured before Import starts, reported
	// separately because the run's duration starts after it.
	connectDur time.Duration

	// projection feeds the INSERT column list from the parsed rows:
	// projection[i] is the row position whose value goes to INSERT column i. It
	// is built by the name-based --columns mapping (a subset in any order) and
	// by excludeGeneratedColumns (computed columns stay out of the INSERT). It
	// is nil whenever row order equals INSERT order — the common run, whose
	// writers then flatten rows with a single append.
	projection []int
}

// NewImporter creates a new Importer instance
func NewImporter(cfg *config.ImportConfig) (*Importer, error) {
	// The parameters go out before the connection is dialled: this is the record
	// an operator reads when the run cannot reach the server at all, so it must
	// not depend on the dial succeeding.
	logImportConfig(cfg)

	// Build DSN using mysql.Config to properly escape special characters
	dsn := common.BuildDSN(&cfg.Config)

	dialStart := time.Now()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, errors.Wrap(err, "failed to connect to database")
	}
	common.ApplyPoolHardening(db)

	// Test the connection. Only a failure a reconnect can heal is retried; a
	// server rejection (bad credentials, unknown database, unknown session
	// variable) returns on the first try — see common.PingWithRetry.
	if pingErr := common.PingWithRetry(db); pingErr != nil {
		db.Close()
		return nil, pingErr
	}

	return &Importer{
		cfg:        cfg,
		db:         db,
		connectDur: time.Since(dialStart),
	}, nil
}

// Close closes the importer and releases resources
func (imp *Importer) Close() error {
	if imp.db != nil {
		return imp.db.Close()
	}
	return nil
}

// Import performs the import operation
func (imp *Importer) Import(ctx context.Context) error {
	startTime := time.Now()
	imp.stats = common.NewStats()
	imp.stopMonitor = common.StartMonitor(ctx, imp.cfg.EffectiveProgressInterval(), imp.stats, func(prev, curr common.StatsSnapshot) {
		// --quiet suppresses the periodic progress record. --messages receives the
		// command log itself rather than a report of its own, so a suppressed
		// record is absent from the file as well.
		if !common.Quiet {
			common.Logger.Info("import progress",
				"read", curr.Processed,
				"inserted", curr.Inserted,
				"updated", curr.Updated,
				"unchanged", curr.Unchanged,
				"failed", curr.Failed,
				"rate", fmt.Sprintf("%.2f rows/sec", curr.RatePerSec()),
				"interval_rate", fmt.Sprintf("%.2f rows/sec", curr.IntervalRate(prev)),
				"elapsed", common.Seconds(curr.Elapsed),
			)
		}
	})
	defer imp.stopMonitor()

	// Open input file. From here to the end of ReadHeader is the header stage:
	// on parquet it reads the footer and builds the schema, on DEL/SQL it reads
	// the first record, and on a very large or remote file it can be the
	// slowest part of the run before any row moves.
	headerStart := time.Now()
	file, err := os.Open(imp.cfg.Input)
	if err != nil {
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
		if errors.Is(err, io.EOF) {
			// A data-only SQL dump of an empty table carries only the SET header
			// (31 bytes for `/*!40101 SET NAMES utf8mb4 */;`): no INSERT to parse and
			// no column list to build an INSERT from. There is nothing to do, and
			// "nothing to do" is the honest success the mysql client also reports
			// when replaying such a file — refusing it made an empty table's own
			// export impossible to import back. Only a file whose skipped statements
			// were all dump preamble qualifies: anything else keeps failing below.
			if r, ok := reader.(*SQLReader); ok && r.OnlyPreamble() {
				return imp.finishNoData(startTime, "empty SQL file: no INSERT statement and no CREATE TABLE, nothing to import")
			}
			// Reached end of file without finding any header row or INSERT:
			// a plain EOF is meaningless to the user here.
			err = errors.New("the input file contains no importable data: no header row (DEL), no INSERT statement (SQL) and no column schema (Parquet) were found")
		}
		return errors.Wrap(err, "failed to read header")
	}
	imp.headerDur = time.Since(headerStart)

	// Determine target columns: --columns > file columns
	// --columns supports two formats:
	// 1. Position-based: --columns "name,age,email" (file columns map to table columns by position)
	// 2. Name-based mapping: --columns "file_col1=table_col1,file_col2=table_col2" (explicit mapping; a subset is fine)
	var targetColumns []string
	// projection maps each INSERT value to its position in a parsed row; nil
	// means row order equals INSERT order (see the field's comment).
	var projection []int
	// readerColumns stays aligned to the input positions, one entry per file
	// column: the readers hand back one field per input column whatever the
	// INSERT names, and the DEL reader resolves its per-column kinds and binary
	// flags position by position.
	var readerColumns []string
	if imp.cfg.Columns != "" {
		if strings.Contains(imp.cfg.Columns, "=") {
			// Name-based mapping mode
			var columnMapping *ColumnMapping
			columnMapping, err = imp.applyColumnMapping(fileColumns)
			if err != nil {
				return errors.Wrap(err, "failed to apply column mapping")
			}
			targetColumns = columnMapping.Columns
			projection = columnMapping.Projection
			readerColumns = columnMapping.ReaderColumns
		} else {
			// Position-based mode
			targetColumns = strings.Split(imp.cfg.Columns, ",")
			for i, col := range targetColumns {
				targetColumns[i] = strings.TrimSpace(col)
			}
			if err := validatePositionalColumns(targetColumns, fileColumns); err != nil {
				return err
			}
			readerColumns = targetColumns
			if imp.cfg.Format == "del" && !imp.cfg.DelModifier.NoColNames && len(fileColumns) > 0 {
				// Positional --columns consumes the first record as a header. That is
				// right for a normal dump and silently drops a row for a
				// --no-col-names one, and the file alone cannot tell the two apart.
				// The one decisive signal is the target table: if none of the consumed
				// fields is one of its columns, the "header" is probably data, so the
				// user has to be told which record was dropped and how to keep it.
				if ambiguous, err := imp.headerLooksLikeData(ctx, fileColumns); err != nil {
					common.Logger.Warn("header check failed", "error", err)
				} else if ambiguous {
					warn := fmt.Sprintf("the first record of the input file was treated as the column header and not imported (fields: %s); "+
						"none of them is a column of table %q, so if the file was exported with --no-col-names add --no-col-names to the import "+
						"to load that record as data", strings.Join(fileColumns, ", "), imp.cfg.Table)
					common.Logger.Warn(warn, "columns", imp.cfg.Columns)
				}
			}
		}
	} else {
		targetColumns = fileColumns
		readerColumns = fileColumns
	}

	// A structure-only SQL file (--no-data export) carries CREATE statements
	// but no rows: create mode applies them and finishes with 0 rows instead
	// of demanding a column list it cannot have.
	structureOnly := imp.cfg.Format == "sql" && imp.cfg.Mode == config.ImportModeCreate &&
		len(targetColumns) == 0 && isStructureOnlySQL(reader)

	if len(targetColumns) == 0 && !structureOnly {
		return errors.New("no target columns available: the input file has no header row, specify --columns/-c with the target column list")
	}

	// Fail fast on target columns that do not exist on the table. DEL/Parquet
	// headers carry the original result-set column names, so an unaliased
	// expression column exported via --query (e.g. "SELECT 1, name ...")
	// would otherwise surface as a cryptic server error 1054 during INSERT —
	// and in replace mode only after the table had already been emptied.
	// SQL format is only exempt in create mode, which legitimately runs before
	// the table exists; for every other mode the table must already be there,
	// so the same pre-flight check applies.
	if imp.cfg.Format != "sql" || imp.cfg.Mode != config.ImportModeCreate {
		if err := imp.validateTargetColumns(ctx, targetColumns); err != nil {
			return err
		}
	}

	// insert_update relies on a UNIQUE key to decide insert-vs-update. On a
	// table without one every ON DUPLICATE KEY clause degenerates to plain
	// INSERT and re-imports silently duplicate rows, so make that visible
	// before any data is written.
	if imp.cfg.Mode == config.ImportModeInsertUpdate {
		if n, err := imp.countUniqueKeys(ctx); err != nil {
			common.Logger.Warn("failed to verify unique keys for insert_update mode", "error", err)
		} else if n == 0 {
			msg := fmt.Sprintf("table %q has no PRIMARY or UNIQUE key: insert_update mode degenerates to plain INSERT and repeated imports will duplicate rows", imp.cfg.Table)
			common.Logger.Warn(msg)
		}
	}

	// Handle import modes
	switch imp.cfg.Mode {
	case config.ImportModeReplace:
		// Disable foreign key checks for replace mode
		common.LogInteractionSQL("SET FOREIGN_KEY_CHECKS=0")
		if _, err := imp.db.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS=0"); err != nil {
			common.Logger.Warn("failed to disable foreign key checks", "error", err)
		}
		defer func() {
			common.LogInteractionSQL("SET FOREIGN_KEY_CHECKS=1")
			if _, err := imp.db.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS=1"); err != nil {
				common.Logger.Warn("failed to re-enable foreign key checks", "error", err)
			}
		}()

		if err := imp.deleteTable(ctx); err != nil {
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
		var createStmts []string
		if sqlReader, ok := reader.(*SQLReader); ok {
			createStmts = sqlReader.GetCreateStatements()
		}
		if imp.cfg.Table == "" {
			if len(createStmts) == 0 {
				return errors.New("create mode requires --table when the SQL file contains no CREATE TABLE statement")
			}
			name := extractCreateTableName(createStmts[0])
			if name == "" {
				return errors.New("failed to derive table name from CREATE statement; specify --table")
			}
			imp.cfg.Table = name
			common.Logger.Info("table name derived from SQL file", "table", name)
		}
		// A data-only SQL file carries no DDL, so with -t the run can only work
		// against a table that already exists. Without this check the first batch
		// was read and INSERTed, and the server answered with the raw 1146 after
		// the data had moved; fail before reading instead, and name both ways out.
		if len(createStmts) == 0 {
			exists, err := imp.tableExists(ctx)
			if err != nil {
				return err
			}
			if !exists {
				return errors.Errorf("create mode found no CREATE TABLE statement in the SQL file and table %q does not exist in database %q: re-export with --create, or import with --mode insert into an existing table", imp.cfg.Table, imp.cfg.Database)
			}
		}
		for _, stmt := range createStmts {
			stmt = rewriteCreateTableTarget(stmt, imp.cfg.Table)
			common.LogInteractionSQL(stmt)
			taggedStmt := common.TagSQL(stmt)
			common.Logger.Info("create table executing")
			if _, err := imp.db.ExecContext(ctx, taggedStmt); err != nil {
				preview := common.SQLPreview(taggedStmt, nil)
				common.Logger.Error("create table failed", "error", err, "sql", preview)
				return errors.Wrapf(err, "failed to execute CREATE TABLE (SQL: %s)", preview)
			}
		}
	}

	if structureOnly {
		return imp.finishNoData(startTime, "structure-only SQL file: table created, no data rows")
	}

	// Generated columns are computed by the server and must stay out of the INSERT
	// column list: writing a value for one is rejected under strict mode (Error
	// 1906) and silently discarded otherwise. Dropping them here keeps the parsed
	// rows and the INSERT list in step (projection carries the positions), and the
	// server recomputes exactly the values the export carried.
	//
	// readerColumns stays the pre-filter list because the reader still hands back
	// one field per input column: its per-position column kinds and binary flags
	// must therefore be resolved for the untouched positions.
	targetColumns, projection, err = imp.excludeGeneratedColumns(ctx, targetColumns, projection)
	if err != nil {
		return err
	}
	imp.projection = projection

	// For DEL import, teach the reader the target column kinds so the text
	// modifiers (--dec-pt/--date-fmt/--timestamp-fmt) reverse-transform only
	// numeric/temporal fields; string fields are never rewritten. The binary
	// flags let it decode the 0x hex literals the exporter writes for
	// binary columns.
	if imp.cfg.Format == "del" {
		if delReader, ok := reader.(*DelReader); ok {
			kinds, binaries, err := imp.columnInfoFor(ctx, readerColumns)
			if err != nil {
				common.Logger.Warn("failed to resolve target column types; DEL text modifiers disabled", "error", err)
			} else {
				delReader.SetColumnKinds(kinds)
				delReader.SetBinaryColumns(binaries)
			}
		}
	}

	// Everything above — connecting, resolving the target table, mode
	// preparation, opening and parsing the input — is setup. The data path
	// below reports its own segments, so the four add up to the total.
	imp.setupDur = time.Since(startTime) - imp.headerDur

	// Choose parallel or single-threaded import
	var importErr error
	if imp.cfg.ImportThreads > 1 {
		importErr = imp.importParallel(ctx, reader, targetColumns, startTime)
	} else {
		importErr = imp.importSingle(ctx, reader, targetColumns, startTime)
	}
	if importErr != nil {
		// Aborted run: still surface what was accomplished before failing. The
		// progress ticker is stopped first, so the abort summary is the last
		// record of the run.
		imp.stopMonitor()
		total := imp.stats.Snapshot()
		common.Logger.Info("import aborted",
			"read", total.Processed,
			"inserted", total.Inserted,
			"updated", total.Updated,
			"unchanged", total.Unchanged,
			"failed", total.Failed,
			"elapsed", common.Seconds(total.Elapsed),
		)
	}
	return importErr
}

// isStructureOnlySQL reports whether reader is a SQL reader that finished
// ReadHeader with captured CREATE statements but no INSERT rows.
func isStructureOnlySQL(reader Reader) bool {
	s, ok := reader.(*SQLReader)
	return ok && !s.HasData() && len(s.GetCreateStatements()) > 0
}

// finishNoData ends a run that has legitimately nothing to write and reports it
// in the same shape as a normal completion: a zero-row summary with the reason
// in `note`. Both callers (a structure-only SQL file and a data-only SQL file
// with no INSERT) reach it before any batch is built, so the counters are
// untouched and the record is the last line of the run.
func (imp *Importer) finishNoData(startTime time.Time, note string) error {
	imp.stopMonitor()
	total := imp.stats.Snapshot()
	common.Logger.Info("import completed",
		"read", total.Processed,
		"inserted", total.Inserted,
		"updated", total.Updated,
		"unchanged", total.Unchanged,
		"failed", total.Failed,
		"bytes", common.FileSize(imp.cfg.Input),
		"input", imp.cfg.Input,
		"duration", common.Seconds(time.Since(startTime)),
		"note", note,
	)
	return nil
}

// validatePositionalColumns checks the positional --columns form ("a,b,c"):
// mapping happens by position, so the list must have one non-empty name per
// file column. With --no-col-names the file header is unknown, so only the
// names themselves are checked.
func validatePositionalColumns(targetColumns, fileColumns []string) error {
	seen := make(map[string]int, len(targetColumns))
	for i, col := range targetColumns {
		if col == "" {
			return errors.Errorf("--columns entry %d is empty: positional mapping needs one target column name per file column", i+1)
		}
		key := strings.ToLower(col)
		if prev, dup := seen[key]; dup {
			return errors.Errorf("--columns names target column %q twice (entries %d and %d): the server rejects a repeated column in the INSERT list (Error 1110 Column '%s' specified twice)", col, prev+1, i+1, col)
		}
		seen[key] = i
	}
	if len(fileColumns) > 0 && len(targetColumns) != len(fileColumns) {
		return errors.Errorf("--columns lists %d target columns but the input file has %d columns: positional mapping pairs them by index, so the counts must match (use --columns \"file_col=table_col\" to import a subset)", len(targetColumns), len(fileColumns))
	}
	return nil
}

// tableExists reports whether the configured target table exists in the target
// database. It is the single existence probe shared by the column pre-flight and
// create mode's data-only-file check, so both send the same statement and report
// probe failures the same way.
func (imp *Importer) tableExists(ctx context.Context) (bool, error) {
	if imp.cfg.Table == "" {
		return false, nil
	}
	var found int
	existsSQL := "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?"
	common.LogInteractionSQL(existsSQL, imp.cfg.Database, imp.cfg.Table)
	if err := imp.db.QueryRowContext(ctx,
		common.TagSQL(existsSQL),
		imp.cfg.Database, imp.cfg.Table).Scan(&found); err != nil {
		return false, errors.Wrap(err, "failed to verify target table")
	}
	return found > 0, nil
}

// validateTargetColumns checks that every target column exists on the table,
// producing a clear error instead of the server's raw 1054. A missing table is
// reported as such. Comparison is case-insensitive, matching MySQL semantics.
func (imp *Importer) validateTargetColumns(ctx context.Context, cols []string) error {
	if imp.cfg.Table == "" {
		return nil
	}
	exists, err := imp.tableExists(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return errors.Errorf("target table %q does not exist in database %q", imp.cfg.Table, imp.cfg.Database)
	}

	placeholders := strings.Repeat("?,", len(cols))
	placeholders = placeholders[:len(placeholders)-1]
	colsSQL := fmt.Sprintf(
		"SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND UPPER(COLUMN_NAME) IN (%s)",
		placeholders,
	)
	q := common.TagSQL(colsSQL)
	args := make([]interface{}, 0, len(cols)+2)
	args = append(args, imp.cfg.Database, imp.cfg.Table)
	lowered := make(map[string]string, len(cols))
	for _, c := range cols {
		c = strings.TrimSpace(c)
		lowered[strings.ToUpper(c)] = c
		args = append(args, strings.ToUpper(c))
	}
	common.LogInteractionSQL(colsSQL, args...)
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

// countUniqueKeys returns the number of PRIMARY or UNIQUE indexes on the
// target table. A result of 0 means INSERT ... ON DUPLICATE KEY UPDATE can
// never match an existing row.
func (imp *Importer) countUniqueKeys(ctx context.Context) (int, error) {
	if imp.cfg.Table == "" {
		return 0, nil
	}
	var n int
	uniqSQL := `SELECT COUNT(DISTINCT INDEX_NAME)
FROM information_schema.STATISTICS
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND (NON_UNIQUE = 0 OR INDEX_NAME = 'PRIMARY')`
	common.LogInteractionSQL(uniqSQL, imp.cfg.Database, imp.cfg.Table)
	err := imp.db.QueryRowContext(ctx,
		common.TagSQL(uniqSQL),
		imp.cfg.Database, imp.cfg.Table).Scan(&n)
	if err != nil {
		return 0, errors.Wrap(err, "failed to inspect unique indexes")
	}
	return n, nil
}

// headerLooksLikeData reports whether the fields ReadHeader consumed as a
// column header are more likely to be a data row. A header written by the tool
// carries the table's own column names, so when none of the consumed fields is a
// column of the target table the record is probably data: a --no-col-names dump
// imported with positional --columns lands exactly there. An unknown table or an
// empty column list is not evidence of anything, so it reports false and leaves
// the import alone.
func (imp *Importer) headerLooksLikeData(ctx context.Context, fields []string) (bool, error) {
	if imp.cfg.Table == "" {
		return false, nil
	}
	listSQL := "SELECT UPPER(COLUMN_NAME) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?"
	common.LogInteractionSQL(listSQL, imp.cfg.Database, imp.cfg.Table)
	rows, err := imp.db.QueryContext(ctx,
		common.TagSQL(listSQL),
		imp.cfg.Database, imp.cfg.Table)
	if err != nil {
		return false, errors.Wrap(err, "failed to list target table columns")
	}
	defer rows.Close()

	columns := make(map[string]bool, len(fields))
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, errors.Wrap(err, "failed to scan target table column")
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return false, errors.Wrap(err, "target table column iteration error")
	}
	if len(columns) == 0 {
		return false, nil
	}
	for _, f := range fields {
		if columns[strings.ToUpper(strings.TrimSpace(f))] {
			return false, nil
		}
	}
	return true, nil
}

// columnInfoFor resolves the ColumnKind of each target column (by name) from
// information_schema, returning slices aligned to cols order, together with a
// "this column is raw binary" flag per column. Unknown or missing columns map to
// common.KindUnknown and false.
func (imp *Importer) columnInfoFor(ctx context.Context, cols []string) ([]common.ColumnKind, []bool, error) {
	if len(cols) == 0 {
		return nil, nil, nil
	}
	placeholders := strings.Repeat("?,", len(cols))
	placeholders = placeholders[:len(placeholders)-1]
	infoSQL := fmt.Sprintf(
		"SELECT COLUMN_NAME, DATA_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME IN (%s)",
		placeholders,
	)
	q := common.TagSQL(infoSQL)
	args := make([]interface{}, 0, len(cols)+2)
	args = append(args, imp.cfg.Database, imp.cfg.Table)
	for _, c := range cols {
		args = append(args, c)
	}
	common.LogInteractionSQL(infoSQL, args...)
	rows, err := imp.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to query column types")
	}
	defer rows.Close()

	byName := make(map[string]string, len(cols))
	for rows.Next() {
		var name, dataType string
		if err := rows.Scan(&name, &dataType); err != nil {
			return nil, nil, errors.Wrap(err, "failed to scan column type")
		}
		byName[strings.ToLower(name)] = dataType
	}
	if err := rows.Err(); err != nil {
		return nil, nil, errors.Wrap(err, "column type iteration error")
	}

	kinds := make([]common.ColumnKind, len(cols))
	binaries := make([]bool, len(cols))
	for i, c := range cols {
		if dt, ok := byName[strings.ToLower(strings.TrimSpace(c))]; ok {
			kinds[i] = common.ClassifyColumnKind(dt)
			binaries[i] = common.IsBinaryTypeName(dt)
		} else {
			kinds[i] = common.KindUnknown
		}
	}
	return kinds, binaries, nil
}

// addBatchResult splits one successfully applied batch into inserted / updated
// / unchanged counters using MySQL's affected-rows semantics. Measured against
// MariaDB with the go driver (no CLIENT_FOUND_ROWS), for a batch of `rows` rows:
//
//   - plain INSERT (insert / create modes) and REPLACE (this tool empties the
//     table first, so every row is a genuine insert): affected == rows.
//   - INSERT ... ON DUPLICATE KEY UPDATE (insert_update mode): 1 per new row,
//     2 per existing row whose stored values actually changed, 0 per existing
//     row set to identical values. So affected = inserted + 2*updated and
//     rows = inserted + updated + unchanged, which gives updated - unchanged
//     == affected - rows: one degree of freedom stays unresolved inside a
//     batch, so the split below takes the provable side of it.
//
// Attribution rule: a surplus (affected > rows) can only come from changed
// rows, and a deficit (affected < rows) can only come from identical rows, so
// each is charged entirely to its own counter and the remainder is the new
// insert count. The sum of the three counters always equals `rows`. The
// residual ambiguity is a batch that mixes changed and identical rows
// (affected lands exactly on `rows`); that is reported as inserts, matching
// the far more common "loading a fresh dump" case.
func (imp *Importer) addBatchResult(rows int64, res sql.Result) {
	affected, err := res.RowsAffected()
	if err != nil || affected < 0 {
		affected = rows
	}
	updated := affected - rows
	if updated < 0 {
		updated = 0
	}
	if updated > rows {
		// Unreachable under the semantics above; a server reporting more than 2
		// affected rows per row would otherwise inflate the counters past `rows`.
		updated = rows
	}
	unchanged := rows - affected
	if unchanged < 0 {
		unchanged = 0
	}
	inserted := rows - updated - unchanged
	if inserted < 0 {
		inserted = 0
	}
	imp.stats.AddInserted(inserted)
	imp.stats.AddUpdated(updated)
	imp.stats.AddUnchanged(unchanged)
}

// Stats exposes the run counters so integration tests can assert the
// inserted/updated/unchanged split against a real server.
func (imp *Importer) Stats() common.StatsSnapshot {
	if imp.stats == nil {
		return common.StatsSnapshot{}
	}
	return imp.stats.Snapshot()
}

// importSingle performs single-threaded import with pipeline optimization
func (imp *Importer) importSingle(ctx context.Context, reader Reader, columns []string, startTime time.Time) error {
	// Resolve the batching ceilings *before* the transaction is open. The byte
	// budget comes from a SHOW VARIABLES query on the pool, and holding a
	// transaction while waiting for a second connection is a deadlock risk if
	// the pool is ever limited to one connection.
	batchSize, maxBatchBytes := imp.resolveBatchSize(len(columns))

	// Open the writer session. It owns the transaction, the --commit-count
	// policy, idle commits and transient-error replay.
	session := newWriteSession(imp, imp.db, ctx, int64(imp.cfg.CommitCount), "single-threaded")
	if err := session.begin(); err != nil {
		return errors.Wrap(err, "failed to begin transaction")
	}
	// Roll back whatever transaction is active when the function returns.
	// Committed transactions return a benign ErrTxDone, which we ignore.
	defer session.rollback()

	// Pipeline configuration
	rowCount := int64(0)
	skipCount := int64(imp.cfg.SkipCount)
	rowLimit := int64(imp.cfg.RowCount)

	// Pipeline: Reader goroutine → Channel → Writer (main goroutine)
	type rowBatch struct {
		data [][]interface{}
		err  error
	}

	// Depth is --pipeline-buffer as configured. One batch can hold the whole byte
	// budget, so the depth is also the memory the queue may retain; past
	// maxQueuedBatchBytes the run says so on the log and imports anyway (see
	// resolveQueueDepth).
	rowCh := make(chan rowBatch, imp.queueDepth)
	errCh := make(chan error, 1)

	// readDur and waitFull are the reader side of the stage report: time spent
	// reading and parsing, and time spent waiting for room in the queue
	// (backpressure from a writer that cannot keep up). They are read after
	// rowCh is closed, so that close is the only synchronisation needed.
	var readDur, waitFull time.Duration

	// Stage 1: Reader goroutine - 从文件读取数据
	go func() {
		defer close(rowCh)

		batch := make([][]interface{}, 0, batchSize)
		var batchBytes int64
		readCount := int64(0)

		// batchStart brackets the read/parse work of one batch, so time spent
		// blocked on a full queue is charged to waitFull instead of making the
		// file look slow.
		batchStart := time.Now()
		send := func(b rowBatch) bool {
			readDur += time.Since(batchStart)
			blockedAt := time.Now()
			select {
			case rowCh <- b:
				waitFull += time.Since(blockedAt)
				batchStart = time.Now()
				return true
			case <-ctx.Done():
				errCh <- ctx.Err()
				return false
			}
		}

		for {
			values, err := reader.ReadRow()
			if err != nil {
				if err.Error() == "EOF" {
					// 发送剩余数据
					if len(batch) > 0 {
						send(rowBatch{data: batch})
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
					send(rowBatch{data: batch})
				}
				return
			}

			imp.stats.AddProcessed(1)
			rowBytes := estimateRowBytes(values)
			// 先判断再加：这一行若会把批次顶出字节预算，先把当前批次发出去。
			// 只在 append 之后判断时，两行各自没超预算、合计却超过
			// max_allowed_packet 的语句仍会被凑成一批（实测触 Error 1153）。
			if wouldExceedBatch(batchBytes, rowBytes, maxBatchBytes) {
				if !send(rowBatch{data: batch}) {
					return
				}
				batch = make([][]interface{}, 0, batchSize)
				batchBytes = 0
			}
			batch = append(batch, values)
			batchBytes += rowBytes

			// 批量发送：行数达到 --batch-size，或单行本身就超过字节预算，任一条件
			// 满足就刷新。
			if shouldFlush(len(batch), batchSize, batchBytes, maxBatchBytes) {
				if !send(rowBatch{data: batch}) {
					return
				}
				batch = make([][]interface{}, 0, batchSize)
				batchBytes = 0
			}
		}
	}()

	// Stage 2: Writer (main goroutine) - 执行批量 INSERT
	//
	// The batch INSERT placeholder count must match the actual number of rows
	// in each batch (the final batch is usually smaller than batchSize), so the
	// statement is built per batch rather than prepared once for a fixed size.
	//
	// The idle timer commits a partial transaction when the reader stalls:
	// leaving rows uncommitted across a long network pause is exactly the
	// window a cloud LB/NAT idle reaper closes, and every unnotified commit
	// shrinks the replay set after a reconnect.
	idleTimer := time.NewTimer(DefaultIdleCommitTimeout)
	defer idleTimer.Stop()

	var batch rowBatch
	var ok bool

	// The writer side of the stage report: time blocked waiting for a batch
	// (an idle reader, or a file slower than the server) and time spent inside
	// the batched INSERT itself.
	var sendDur, waitEmpty time.Duration
loop:
	for {
		recvAt := time.Now()
		select {
		case batch, ok = <-rowCh:
			waitEmpty += time.Since(recvAt)
			if !ok {
				break loop
			}
		case <-idleTimer.C:
			if err := session.idleCommit(); err != nil {
				return errors.Wrap(err, "failed to commit idle transaction")
			}
			idleTimer.Reset(DefaultIdleCommitTimeout)
			continue
		case <-ctx.Done():
			return ctx.Err()
		}

		if len(batch.data) == 0 {
			armIdleTimer(idleTimer, DefaultIdleCommitTimeout)
			continue
		}

		// 构建批量插入的参数
		batchValues := make([]interface{}, 0, len(batch.data)*len(columns))
		for _, row := range batch.data {
			batchValues = appendRowValues(batchValues, row, imp.projection)
		}

		insertSQL := imp.buildBatchInsertSQL(columns, len(batch.data))

		// 执行批量插入（瞬断时自动重连并回放本事务的未提交批次）
		writeAt := time.Now()
		if err := session.write(insertSQL, batchValues, len(batch.data)); err != nil {
			preview := common.FailingSQL(insertSQL, batchValues)
			imp.stats.AddFailed(int64(len(batch.data)))
			common.Logger.Error("insert batch failed", "error", err, "sql", preview)
			return errors.Wrapf(err, "failed to insert batch (SQL: %s)", preview)
		}
		batchDur := time.Since(writeAt)
		sendDur += batchDur
		common.WarnIfSlow(slowBatchThreshold, "slow insert batch", batchDur,
			"rows", len(batch.data), "bytes", batchWireBytes(insertSQL, batchValues))

		rowCount += int64(len(batch.data))

		// Commit transaction periodically, counting the rows actually held by
		// the open transaction (a batch size that does not divide the commit
		// count must not postpone the commit indefinitely).
		if err := session.maybeCommit(); err != nil {
			return errors.Wrap(err, "failed to commit transaction")
		}
		armIdleTimer(idleTimer, DefaultIdleCommitTimeout)
	}

	// 检查 reader goroutine 的错误
	select {
	case err := <-errCh:
		return err
	default:
	}

	// Final commit
	if err := session.commit(); err != nil {
		return errors.Wrap(err, "failed to commit final transaction")
	}

	duration := time.Since(startTime)

	// Stop the progress ticker before the summary and take its drained counters:
	// a run must not end on a "progress" record, which reads as a job that is
	// still going.
	imp.stopMonitor()

	total := imp.stats.Snapshot()
	common.Logger.Info("import completed",
		"read", total.Processed,
		"inserted", total.Inserted,
		"updated", total.Updated,
		"unchanged", total.Unchanged,
		"failed", total.Failed,
		"bytes", common.FileSize(imp.cfg.Input),
		"input", imp.cfg.Input,
		"duration", common.Seconds(duration),
		"rate", fmt.Sprintf("%.2f rows/sec", float64(total.Processed)/duration.Seconds()),
		// setup + header + read + send + the two queue waits account for the run:
		// read is the file (parse), send is the server, and the waits are the
		// pipeline handing off between them.
		"connect", common.Seconds(imp.connectDur), "setup", common.Seconds(imp.setupDur),
		"header", common.Seconds(imp.headerDur),
		"read", common.Seconds(readDur), "send", common.Seconds(sendDur),
		"wait_empty", common.Seconds(waitEmpty), "wait_full", common.Seconds(waitFull),
	)

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

	// The queue is the single reader's hand-off to all workers: one shared channel of
	// batches, so its depth is a memory question while --threads is a concurrency
	// question. A queued batch holds up to maxBatchBytes of parsed rows, and past
	// maxQueuedBatchBytes the depth is reported, not changed (see batchcap.go).
	batchCh := make(chan rowBatch, imp.queueDepth)
	errCh := make(chan error, numThreads+1)

	// wg covers *both* stages. errCh is closed after wg.Wait() below, and the
	// reader sends to errCh on its error and cancellation paths; leaving the
	// reader out of the group let close(errCh) race with those sends, which
	// panicked the process (exit code 2, partial import, no error record).
	var wg sync.WaitGroup

	// Stage 1: single reader goroutine (the Reader is not safe for concurrent
	// use) reads rows, applies skip/limit, and emits fixed-size batches.
	wg.Add(1)
	go func() {
		defer wg.Done()
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
			rowBytes := estimateRowBytes(values)
			// 先判断再加，理由与单线程管线相同：两行各自没超预算、合计超过
			// max_allowed_packet 时，append 之后再判断会把它们凑成一条语句。
			if wouldExceedBatch(batchBytes, rowBytes, maxBatchBytes) {
				select {
				case batchCh <- rowBatch{data: batch}:
					batch = make([][]interface{}, 0, batchSize)
					batchBytes = 0
				case <-ctx.Done():
					errCh <- ctx.Err()
					return
				}
			}
			batch = append(batch, values)
			batchBytes += rowBytes
			// 行数或单行超预算任一满足就刷新，语义与单线程管线一致。
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

	// Build the DSN once for all workers (BuildDSN embeds charset/session vars).
	dsn := common.BuildDSN(&imp.cfg.Config)
	warnIfThreadsExceedCapacity(ctx, imp.db, numThreads)
	warnIfReplayWindowScales(numThreads)

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
			common.ApplyPoolHardening(db)
			defer db.Close()

			workerStart := time.Now()
			workerRows := int64(0)

			session := newWriteSession(imp, db, ctx, commitCount, fmt.Sprintf("thread %d", threadID))
			if err := session.begin(); err != nil {
				errCh <- errors.Wrapf(err, "thread %d: failed to begin transaction", threadID)
				cancel()
				return
			}
			// Committed txs return a benign ErrTxDone from Rollback.
			defer session.rollback()

			// See importSingle: commit a partial transaction rather than let an
			// idle connection hold uncommitted rows across a network pause.
			idleTimer := time.NewTimer(DefaultIdleCommitTimeout)
			defer idleTimer.Stop()

			var batch rowBatch
			var ok bool
		workerLoop:
			for {
				select {
				case batch, ok = <-batchCh:
					if !ok {
						break workerLoop
					}
				case <-idleTimer.C:
					if err := session.idleCommit(); err != nil {
						errCh <- errors.Wrapf(err, "thread %d: failed to commit idle transaction", threadID)
						cancel()
						return
					}
					idleTimer.Reset(DefaultIdleCommitTimeout)
					continue
				case <-ctx.Done():
					// Preserve the progress made before cancellation.
					_ = session.commit()
					return
				}

				if len(batch.data) == 0 {
					armIdleTimer(idleTimer, DefaultIdleCommitTimeout)
					continue
				}

				flat := make([]interface{}, 0, len(batch.data)*len(columns))
				for _, row := range batch.data {
					flat = appendRowValues(flat, row, imp.projection)
				}

				insertSQL := imp.buildBatchInsertSQL(columns, len(batch.data))
				writeAt := time.Now()
				if err := session.write(insertSQL, flat, len(batch.data)); err != nil {
					preview := common.FailingSQL(insertSQL, flat)
					imp.stats.AddFailed(int64(len(batch.data)))
					common.Logger.Error("insert batch failed", "thread", threadID, "error", err, "sql", preview)
					errCh <- errors.Wrapf(err, "thread %d: failed to insert batch (SQL: %s)", threadID, preview)
					cancel()
					return
				}
				batchDur := time.Since(writeAt)
				workerRows += int64(len(batch.data))
				common.WarnIfSlow(slowBatchThreshold, "slow insert batch", batchDur,
					"thread", threadID, "rows", len(batch.data), "bytes", batchWireBytes(insertSQL, flat))

				if err := session.maybeCommit(); err != nil {
					errCh <- errors.Wrapf(err, "thread %d: failed to commit transaction", threadID)
					cancel()
					return
				}
				armIdleTimer(idleTimer, DefaultIdleCommitTimeout)
			}

			// Final commit for this worker's last partial transaction.
			if err := session.commit(); err != nil {
				errCh <- errors.Wrapf(err, "thread %d: failed to commit final transaction", threadID)
				cancel()
				return
			}

			workerDur := time.Since(workerStart)
			common.Logger.Info("worker completed", "thread", threadID, "rows", workerRows,
				"duration", common.Seconds(workerDur),
				"rate", fmt.Sprintf("%.2f rows/sec", rowsPerSecond(workerRows, workerDur)))
		}(i)
	}

	// Wait for workers, then surface the first error if any.
	wg.Wait()
	close(errCh)
	if err, ok := <-errCh; ok {
		return err
	}

	duration := time.Since(startTime)

	// Stop the progress ticker before the summary and take its drained counters:
	// a run must not end on a "progress" record, which reads as a job that is
	// still going.
	imp.stopMonitor()

	total := imp.stats.Snapshot()
	common.Logger.Info("parallel import completed",
		"read", total.Processed,
		"inserted", total.Inserted,
		"updated", total.Updated,
		"unchanged", total.Unchanged,
		"failed", total.Failed,
		"bytes", common.FileSize(imp.cfg.Input),
		"input", imp.cfg.Input,
		"threads", numThreads,
		"duration", common.Seconds(duration),
		"rate", fmt.Sprintf("%.2f rows/sec", float64(total.Processed)/duration.Seconds()),
		"connect", common.Seconds(imp.connectDur),
		"setup", common.Seconds(imp.setupDur), "header", common.Seconds(imp.headerDur),
	)

	return nil
}

func (imp *Importer) deleteTable(ctx context.Context) error {
	query := fmt.Sprintf("DELETE FROM `%s`", imp.cfg.Table)
	common.LogInteractionSQL(query)
	_, err := imp.db.ExecContext(ctx, common.TagSQL(query))
	return err
}

// ColumnMapping is one name-based --columns mapping resolved against the input
// header. A mapping names the columns an import writes, not every column the
// file carries: an unmapped file column is simply not inserted.
type ColumnMapping struct {
	// Columns are the target table columns in the order they go into the INSERT
	// column list. They follow the input header's order, so the order the user
	// wrote the pairs in does not matter.
	Columns []string

	// Projection feeds Columns: Projection[i] is the parsed-row position that
	// supplies Columns[i]. It is nil when the mapped columns are all of the
	// file's columns in file order, so the common mapping still appends whole
	// rows without indexing.
	Projection []int

	// ReaderColumns is aligned to the input positions — one entry per file
	// column — and carries the table column each position feeds, or "" where
	// the mapping leaves that file column out.
	ReaderColumns []string
}

// applyColumnMapping resolves the name-based --columns form against the input
// header. Only the written mappings must be valid: a mapping may name a subset
// of the file's columns, but every entry it does name is checked before a row
// is read — the file column must exist in the header, no file column may be
// mapped twice, and no target column may be fed twice (the server would reject
// the INSERT with Error 1110 after the table had already been written).
func (imp *Importer) applyColumnMapping(fileColumns []string) (*ColumnMapping, error) {
	type entry struct{ fileCol, tableCol string }
	entries := make([]entry, 0, 4)
	mapped := make(map[string]string, 4) // lowercased file column -> table column
	for _, pair := range strings.Split(imp.cfg.Columns, ",") {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			return nil, errors.Errorf("invalid column mapping entry %q: expected file_col=table_col", strings.TrimSpace(pair))
		}
		fileCol := strings.TrimSpace(parts[0])
		tableCol := strings.TrimSpace(parts[1])
		if fileCol == "" || tableCol == "" {
			return nil, errors.Errorf("invalid column mapping entry %q: both file_col and table_col are required", strings.TrimSpace(pair))
		}
		key := strings.ToLower(fileCol)
		if prev, dup := mapped[key]; dup {
			return nil, errors.Errorf("file column %q is mapped twice (to %q and %q): each file column can feed one target column", fileCol, prev, tableCol)
		}
		mapped[key] = tableCol
		entries = append(entries, entry{fileCol: fileCol, tableCol: tableCol})
	}

	targets := make(map[string]string, len(entries)) // lowercased table column -> file column
	for _, e := range entries {
		key := strings.ToLower(e.tableCol)
		if prev, dup := targets[key]; dup {
			return nil, errors.Errorf("target column %q is fed by two file columns (%q and %q): the server rejects a repeated column in the INSERT list (Error 1110 Column '%s' specified twice)", e.tableCol, prev, e.fileCol, e.tableCol)
		}
		targets[key] = e.fileCol
	}

	// The mapping is resolved by name, so a file without a header has no name to
	// match: that is a usage error, not an unknown column.
	if len(fileColumns) == 0 {
		return nil, errors.New("column mapping matches file columns by name, but the input file has no column header: use the positional form --columns \"col1,col2,...\" instead (one target column per file column, in file order)")
	}

	headerIndex := make(map[string]int, len(fileColumns))
	for i, name := range fileColumns {
		key := strings.ToLower(strings.TrimSpace(name))
		if _, ok := headerIndex[key]; !ok {
			headerIndex[key] = i
		}
	}
	for _, e := range entries {
		if _, ok := headerIndex[strings.ToLower(e.fileCol)]; !ok {
			return nil, errors.Errorf("file column %q is not in the input header (file columns: %s)", e.fileCol, strings.Join(fileColumns, ", "))
		}
	}

	cm := &ColumnMapping{
		Columns:       make([]string, 0, len(entries)),
		Projection:    make([]int, 0, len(entries)),
		ReaderColumns: make([]string, len(fileColumns)),
	}
	ignored := make([]string, 0, len(fileColumns)-len(entries))
	// Walk the header, not the mapping: that is what keeps Columns in file
	// order and the projection in step with the row the reader builds.
	for i, name := range fileColumns {
		tableCol, ok := mapped[strings.ToLower(strings.TrimSpace(name))]
		if !ok {
			ignored = append(ignored, name)
			continue
		}
		cm.Columns = append(cm.Columns, tableCol)
		cm.Projection = append(cm.Projection, i)
		cm.ReaderColumns[i] = tableCol
	}

	// Whole row in file order is the fast path: no per-value index at all.
	identity := len(cm.Projection) == len(fileColumns)
	for i := 0; identity && i < len(cm.Projection); i++ {
		identity = cm.Projection[i] == i
	}
	if identity {
		cm.Projection = nil
	}

	common.Logger.Info("column mapping applied",
		"file_columns", fileColumns,
		"target_columns", cm.Columns,
		"mapping", imp.cfg.Columns)
	if len(ignored) > 0 {
		common.Logger.Info("unmapped file columns are not imported",
			"columns", ignored,
			"note", "only the columns named in --columns are written; the remaining fields of each row are ignored")
	}

	return cm, nil
}

func (imp *Importer) buildBatchInsertSQL(columns []string, rowCount int) string {
	// Trim whitespace from column names
	targetColumns := make([]string, len(columns))
	for i, col := range columns {
		targetColumns[i] = strings.TrimSpace(col)
	}

	// One placeholder group per row: "(?,?,?)" for a three-column table. Building
	// it once lets the batch write rowCount copies of a single string instead of
	// rowCount*columns writes of one byte.
	group := "(" + strings.TrimSuffix(strings.Repeat("?,", len(targetColumns)), ",") + ")"
	separators := 0
	if rowCount > 0 {
		separators = rowCount - 1
	}

	// For insert_update mode, append ON DUPLICATE KEY UPDATE
	var onDup string
	if imp.cfg.Mode == config.ImportModeInsertUpdate {
		var od strings.Builder
		od.WriteString(" ON DUPLICATE KEY UPDATE ")
		for i, col := range targetColumns {
			if i > 0 {
				od.WriteString(", ")
			}
			od.WriteString("`")
			od.WriteString(col)
			od.WriteString("` = VALUES(`")
			od.WriteString(col)
			od.WriteString("`)")
		}
		onDup = od.String()
	}

	// The statement is assembled once, in a buffer sized up front: the
	// attribution comment goes in as the first bytes instead of being
	// concatenated onto the finished text (that copied the whole batch again),
	// and Grow stops the builder from doubling its way up to the batch size
	// (that copied it a third time, plus the discarded halves).
	tag := common.SQLTag()
	colListLen := 0
	for _, col := range targetColumns {
		colListLen += len(col) + 2 // the backticks around each name
	}
	if len(targetColumns) > 1 {
		colListLen += 2 * (len(targetColumns) - 1) // ", " between names
	}

	var sb strings.Builder
	sb.Grow(len(tag) + 1 + len("INSERT INTO `") + len(imp.cfg.Table) + len("` (") +
		colListLen + len(") VALUES ") + rowCount*len(group) + separators + len(onDup))
	if tag != "" {
		sb.WriteString(tag)
		sb.WriteByte(' ')
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

	for i := 0; i < rowCount; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(group)
	}
	sb.WriteString(onDup)

	return sb.String()
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
