package export

import (
	"bufio"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/gomydump/internal/common"
)

// SQLWriter writes data in SQL format (CREATE TABLE + INSERT statements)
type SQLWriter struct {
	bw            *bufio.Writer
	tableName     string
	columns       []string
	statementSize int
	writeCreate   bool
	writeData     bool
	serverType    common.ServerType
	// createStmt is the pre-fetched CREATE TABLE statement. The exporter must
	// obtain it before opening the streaming cursor, because the single
	// export connection is held by the open result set.
	createStmt string
	batchBuf   strings.Builder
	valueBuf   strings.Builder // Reuse for each row to avoid allocation
	batchCount int
	colList    string // cached column list string
	charset    string // connection charset for SET NAMES header
	// binaryFlags[i] marks column i as a binary column (BLOB/BINARY/etc).
	// When hexBlob is true, binary columns are written as 0x... hex literals.
	binaryFlags []bool
	hexBlob     bool
	// singleRow emits one INSERT per row instead of accumulating VALUES tuples
	// up to statementSize.
	singleRow      bool
	oversizeWarned bool
	// skipTableLocks suppresses LOCK TABLES/UNLOCK TABLES. Parallel export
	// writes shard fragments that are concatenated later, and a lock statement
	// per fragment would release the table lock in the middle of the dump.
	skipTableLocks bool
}

// SetSkipTableLocks suppresses the LOCK TABLES/UNLOCK TABLES pair. Used by the
// shard writers of a parallel export, whose output is merged under a single
// lock written by the merge step.
func (s *SQLWriter) SetSkipTableLocks(on bool) {
	s.skipTableLocks = on
}

// NewSQLWriter creates a new SQL format writer.
// createStmt must be provided by the caller (via SHOW CREATE TABLE) before the
// result-set cursor is opened; it is used only when writeCreate is true.
// charset is the connection charset used for the SET NAMES header line.
func NewSQLWriter(w io.Writer, tableName string, columns []string, statementSize int, writeCreate, writeData bool, serverType common.ServerType, createStmt string, charset string) *SQLWriter {
	return &SQLWriter{
		bw:            bufio.NewWriterSize(w, 256*1024),
		tableName:     tableName,
		columns:       columns,
		statementSize: statementSize,
		writeCreate:   writeCreate,
		writeData:     writeData,
		serverType:    serverType,
		createStmt:    createStmt,
		charset:       charset,
	}
}

// SetBinaryFormat configures binary column encoding. binaryFlags[i] marks
// column i as binary; when hexBlob is true those columns are written as 0x...
// hex literals (charset-safe, self-describing). Empty binary values are
// written as the zero-length literal 0x (not ” or NULL).
func (s *SQLWriter) SetBinaryFormat(binaryFlags []bool, hexBlob bool) {
	s.binaryFlags = binaryFlags
	s.hexBlob = hexBlob
}

// WriteHeader writes the SET NAMES charset line, then CREATE TABLE if enabled.
func (s *SQLWriter) WriteHeader(columns []string) error {
	// Write the self-describing charset header whenever we're going to emit
	// any content (CREATE TABLE or INSERT data). This mirrors mysqldump's
	// --set-charset behavior; the standard mysql client uses it to set the
	// connection charset, and gomydump's own importer skips this line.
	if s.writeCreate || s.writeData {
		if _, err := fmt.Fprintf(s.bw, "/*!40101 SET NAMES %s */;\n", s.charset); err != nil {
			return err
		}
	}

	// Write LOCK TABLES statement for MySQL (skip for OceanBase)
	// Lock table when we're going to write data to ensure consistency
	if s.serverType == common.ServerMySQL && s.writeData && !s.skipTableLocks {
		if _, err := fmt.Fprintf(s.bw, "LOCK TABLES `%s` WRITE;\n", s.tableName); err != nil {
			return err
		}
	}

	if !s.writeCreate {
		return nil
	}

	if s.createStmt == "" {
		return errors.New("CREATE TABLE statement was not provided")
	}
	createStmt := s.createStmt

	// Write header comment
	if _, err := fmt.Fprintf(s.bw, "-- Table structure for %s\n", s.tableName); err != nil {
		return err
	}

	// Write CREATE TABLE statement
	if _, err := fmt.Fprintln(s.bw, createStmt); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(s.bw); err != nil {
		return err
	}

	if s.writeData {
		if _, err := fmt.Fprintf(s.bw, "-- Data for %s\n", s.tableName); err != nil {
			return err
		}
	}

	return nil
}

// SetSingleRowInsert selects the layout the exporter uses by default: one INSERT
// statement per row (the mysqldump --skip-extended-insert layout). Batching by
// --statement-size stops, so the size option degrades into a warning threshold:
// a row whose own statement is larger than it is still written whole, because a
// row cannot be split. The exporter turns this off only when --extended-insert
// is given, since the import side re-batches rows whatever the file looks like.
func (s *SQLWriter) SetSingleRowInsert(on bool) {
	s.singleRow = on
}

// WriteRow writes INSERT statement for a row
func (s *SQLWriter) WriteRow(values []interface{}) error {
	if !s.writeData {
		return nil
	}

	// Build column list if not cached
	if s.colList == "" {
		var sb strings.Builder
		sb.WriteString("INSERT INTO `")
		sb.WriteString(s.tableName)
		sb.WriteString("` (")
		for i, col := range s.columns {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("`")
			sb.WriteString(col)
			sb.WriteString("`")
		}
		sb.WriteString(") VALUES ")
		s.colList = sb.String()
	}

	// Build VALUES tuple (reuse valueBuf to avoid allocation)
	s.valueBuf.Reset()
	s.valueBuf.WriteString("(")
	for i, val := range values {
		if i > 0 {
			s.valueBuf.WriteString(", ")
		}
		s.valueBuf.WriteString(s.formatSQLValue(val, i))
	}
	s.valueBuf.WriteString(")")

	// Add to batch
	return s.appendValueTuple()
}

// WriteRawRow writes INSERT statement for a row using sql.RawBytes to avoid allocation
func (s *SQLWriter) WriteRawRow(values []sql.RawBytes) error {
	if !s.writeData {
		return nil
	}

	// Build column list if not cached
	if s.colList == "" {
		var sb strings.Builder
		sb.WriteString("INSERT INTO `")
		sb.WriteString(s.tableName)
		sb.WriteString("` (")
		for i, col := range s.columns {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("`")
			sb.WriteString(col)
			sb.WriteString("`")
		}
		sb.WriteString(") VALUES ")
		s.colList = sb.String()
	}

	// Build VALUES tuple (reuse valueBuf to avoid allocation)
	s.valueBuf.Reset()
	s.valueBuf.WriteString("(")
	for i, val := range values {
		if i > 0 {
			s.valueBuf.WriteString(", ")
		}
		s.valueBuf.WriteString(s.formatRawSQLValue(val, i))
	}
	s.valueBuf.WriteString(")")

	// Add to batch
	return s.appendValueTuple()
}

// appendValueTuple hands the tuple built in valueBuf to the output: in batched
// mode it joins the pending VALUES list, flushing the statement once
// --statement-size is reached; in single-row mode it writes the row as its own
// INSERT immediately.
func (s *SQLWriter) appendValueTuple() error {
	if s.singleRow {
		size := len(s.colList) + s.valueBuf.Len() + 2 // ";\n"
		if size > s.statementSize {
			s.warnOversizedStatement(size)
		}
		return s.writeStatement(s.valueBuf.String())
	}

	if s.batchBuf.Len()+s.valueBuf.Len()+len(s.colList) > s.statementSize && s.batchCount > 0 {
		if err := s.flushBatch(); err != nil {
			return err
		}
	}

	if s.batchCount > 0 {
		s.batchBuf.WriteString(", ")
	}
	s.batchBuf.WriteString(s.valueBuf.String())
	s.batchCount++
	return nil
}

// writeStatement emits one complete INSERT on exactly one physical line.
func (s *SQLWriter) writeStatement(tuple string) error {
	if _, err := s.bw.WriteString(s.colList); err != nil {
		return err
	}
	if _, err := s.bw.WriteString(tuple); err != nil {
		return err
	}
	_, err := s.bw.WriteString(";\n")
	return err
}

// warnOversizedStatement reports once per file that a single-row statement is
// larger than --statement-size. Nothing can be done about it — a row is the
// atomic unit of an INSERT — but the dump is then no longer splittable below one
// row, which is what the size limit is usually there for.
func (s *SQLWriter) warnOversizedStatement(size int) {
	if s.oversizeWarned {
		return
	}
	s.oversizeWarned = true
	common.Logger.Warn("single-row INSERT exceeds --statement-size and is written whole",
		"statement_bytes", size, "statement_size", s.statementSize)
}

// flushBatch writes the current batch INSERT statement
func (s *SQLWriter) flushBatch() error {
	if s.batchCount == 0 {
		return nil
	}

	if err := s.writeStatement(s.batchBuf.String()); err != nil {
		return err
	}

	// Reset batch state
	s.batchBuf.Reset()
	s.batchCount = 0
	return nil
}

// Flush writes buffered bytes to the underlying writer without emitting a
// footer. The parallel export merge step needs it: the preamble must land on
// disk before the shard files are appended to the same handle.
func (s *SQLWriter) Flush() error {
	return s.bw.Flush()
}

// WriteFooter completes the SQL file with UNLOCK TABLES
func (s *SQLWriter) WriteFooter() error {
	// Flush any remaining batched INSERT statements
	if s.writeData {
		if err := s.flushBatch(); err != nil {
			return err
		}
	}

	// Write UNLOCK TABLES statement for MySQL (skip for OceanBase)
	if s.serverType == common.ServerMySQL && s.writeData && !s.skipTableLocks {
		if _, err := fmt.Fprintln(s.bw, "UNLOCK TABLES;"); err != nil {
			return err
		}
	}

	return s.bw.Flush()
}

func (s *SQLWriter) formatSQLValue(val interface{}, colIdx int) string {
	if val == nil {
		return "NULL"
	}

	// Binary column with hex encoding: write as 0x... literal.
	// Empty binary values write as 0x (zero-length hex literal), not '' or NULL.
	if s.hexBlob && colIdx < len(s.binaryFlags) && s.binaryFlags[colIdx] {
		switch v := val.(type) {
		case []byte:
			return "0x" + hex.EncodeToString(v)
		case string:
			return "0x" + hex.EncodeToString([]byte(v))
		}
	}

	switch v := val.(type) {
	case []byte:
		return "'" + escapeSQLString(string(v)) + "'"
	case string:
		return "'" + escapeSQLString(v) + "'"
	case time.Time:
		return "'" + v.Format("2006-01-02 15:04:05") + "'"
	case int:
		return strconv.FormatInt(int64(v), 10)
	case int8:
		return strconv.FormatInt(int64(v), 10)
	case int16:
		return strconv.FormatInt(int64(v), 10)
	case int32:
		return strconv.FormatInt(int64(v), 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case uint:
		return strconv.FormatUint(uint64(v), 10)
	case uint8:
		return strconv.FormatUint(uint64(v), 10)
	case uint16:
		return strconv.FormatUint(uint64(v), 10)
	case uint32:
		return strconv.FormatUint(uint64(v), 10)
	case uint64:
		return strconv.FormatUint(v, 10)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		if v {
			return "1"
		}
		return "0"
	default:
		return "'" + escapeSQLString(fmt.Sprintf("%v", v)) + "'"
	}
}

// Global SQL escape replacer - reuse to avoid allocation overhead
// Single quotes use SQL-standard double-write (”), not backslash escape.
// Newline, carriage return, tab and backslash use backslash escapes so every
// INSERT fits on a single physical line (file is line-splittable).
var sqlEscapeReplacer = strings.NewReplacer(
	"\\", "\\\\",
	"'", "''",
	"\n", "\\n",
	"\r", "\\r",
	"\t", "\\t",
	"\x00", "\\0",
	"\x1a", "\\Z",
)

// needEscapeChars 包含所有需要转义的字符
const needEscapeChars = "'\n\r\t\\\x00\x1a"

func escapeSQLString(s string) string {
	// 快速路径：检查是否需要转义
	if !strings.ContainsAny(s, needEscapeChars) {
		return s
	}
	return sqlEscapeReplacer.Replace(s)
}

// formatRawSQLValue formats sql.RawBytes directly to avoid allocation
func (s *SQLWriter) formatRawSQLValue(val sql.RawBytes, colIdx int) string {
	if val == nil {
		return "NULL"
	}
	// Binary column with hex encoding: write as 0x... literal.
	// Empty binary values write as 0x (zero-length hex literal), not '' or NULL.
	if s.hexBlob && colIdx < len(s.binaryFlags) && s.binaryFlags[colIdx] {
		return "0x" + hex.EncodeToString(val)
	}
	// 零拷贝转换：[]byte -> string，避免内存分配
	return "'" + escapeSQLString(bytesToString(val)) + "'"
}
