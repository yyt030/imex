package export

import (
	"bufio"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

// SQLWriter writes data rows as INSERT statements. It never writes DDL: table
// definitions come from `imex show tab`, which is the only place this tool reads
// SHOW CREATE TABLE.
type SQLWriter struct {
	bw            *bufio.Writer
	tableName     string
	columns       []string
	statementSize int
	// batch holds the pending VALUES tuples (without the INSERT prefix);
	// row is the scratch tuple for the row being formatted. Both are byte
	// slices reused across rows: formatting appends in place instead of
	// building a string per value and copying it again into the batch.
	batch      []byte
	row        []byte
	batchCount int
	colList    []byte // cached INSERT prefix, including the column list
	// binaryFlags[i] marks column i as a binary column (BLOB/BINARY/etc).
	// When hexBlob is true, binary columns are written as 0x... hex literals.
	binaryFlags []bool
	hexBlob     bool
	// kinds[i] is the SQL literal kind of column i. Numeric columns are written
	// as bare literals (no quotes) so the output matches mysqldump; when kinds
	// is nil every value keeps the historical quoted representation.
	kinds []common.ColumnKind
	// singleRow emits one INSERT per row instead of accumulating VALUES tuples
	// up to statementSize.
	singleRow      bool
	oversizeWarned bool
	// mergedTuples counts the rows taken into the batched layout and everMerged
	// records whether one statement ever held more than one tuple. Together they
	// detect the layout silently collapsing to one INSERT per row.
	mergedTuples int
	everMerged   bool
	// dialect is the SQL grammar the writer emits. The zero value ("") and
	// SQLDialectMySQL both mean MySQL, so a writer configured by no caller
	// produces the historical bytes unchanged; only SQLDialectPG switches the
	// identifier quoting, the binary literal, the boolean spelling and the string
	// escaping.
	dialect config.SQLDialect
	// boolFlags[i] marks column i as a boolean column, supplied by
	// SetBooleanFlags from the source engine's BooleanFlags. The writer reads it
	// for whichever --sql-dialect is active, because the driver hands booleans
	// over as text (PostgreSQL "t"/"f" after CellFixups, Dameng "1"/"0") and a
	// bare token in a VALUES list is parsed as a column reference by every
	// engine. Each dialect therefore needs the value spelled in its own valid
	// literal form (see dialectBoolLiteral); the mark is what makes the dump
	// replayable on the target.
	boolFlags []bool
}

// isPG reports whether the writer is emitting the PostgreSQL grammar.
func (s *SQLWriter) isPG() bool { return s.dialect == config.SQLDialectPG }

// isDM reports whether the writer is emitting the Dameng (Oracle-compat)
// grammar. Like PostgreSQL it double-quotes identifiers, but it differs in the
// two places Oracle semantics force: string literals have no backslash escape
// (only the single quote is doubled) and binary goes out as HEXTORAW('..').
func (s *SQLWriter) isDM() bool { return s.dialect == config.SQLDialectDM }

// doubleQuotesIdent reports whether the current grammar quotes identifiers with
// double quotes rather than MySQL backticks. Both PostgreSQL and Dameng do.
func (s *SQLWriter) doubleQuotesIdent() bool { return s.isPG() || s.isDM() }

// SetBooleanFlags marks which columns the writer should spell in the active
// dialect's boolean literal form (true/false for PostgreSQL, 1/0 for MySQL and
// Dameng) instead of passing the source driver's raw boolean text through.
func (s *SQLWriter) SetBooleanFlags(flags []bool) { s.boolFlags = flags }

// NewSQLWriter creates a new SQL format writer. It writes the rows of one
// result set as INSERT statements and nothing else.
func NewSQLWriter(w io.Writer, tableName string, columns []string, statementSize int) *SQLWriter {
	return &SQLWriter{
		bw:            bufio.NewWriterSize(w, 256*1024),
		tableName:     tableName,
		columns:       columns,
		statementSize: statementSize,
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

// SetColumnKinds configures per-column SQL literal kinds. Numeric columns are
// written unquoted; other kinds keep the quoted string representation.
func (s *SQLWriter) SetColumnKinds(kinds []common.ColumnKind) {
	s.kinds = kinds
}

// SetDialect selects the SQL grammar the writer emits. Leaving it unset, or
// passing MySQL, keeps every byte the historical writer produced; PostgreSQL
// switches identifier quoting, the binary literal, the boolean spelling and the
// string escaping. It must be called before the first row so the cached column
// list is built in the chosen grammar.
func (s *SQLWriter) SetDialect(d config.SQLDialect) {
	s.dialect = d
}

func (s *SQLWriter) columnKind(idx int) common.ColumnKind {
	if idx >= 0 && idx < len(s.kinds) {
		return s.kinds[idx]
	}
	return common.KindUnknown
}

// WriteHeader is intentionally empty. The dump used to open with a MySQL
// version-conditional `/*!40101 SET NAMES <charset> */;` preamble. That line is
// inert for a reader that treats `/* ... */` as a comment but it is still MySQL
// session grammar, and a file that has to replay unchanged on MySQL, PostgreSQL
// and Dameng cannot carry it, so the product is now a plain sequence of INSERT
// statements and nothing else. The header stays a method because Writer is the
// interface the exporter drives for every format, and it still flushes nothing:
// there is simply no preamble byte to emit.
//
// The import path is unchanged and still skips the legacy preamble statements
// (SET NAMES / LOCK / UNLOCK), so files written by earlier versions keep
// importing; this only stops the exporter from producing them.
func (s *SQLWriter) WriteHeader(columns []string) error {
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
	s.ensureColList()

	// Build VALUES tuple in the scratch row buffer, appending in place. The
	// historical implementation built one string per value and copied the
	// tuple again into the batch, which cost three passes over every row.
	s.row = s.row[:0]
	s.row = append(s.row, '(')
	for i, val := range values {
		if i > 0 {
			s.row = append(s.row, ',')
		}
		s.row = s.appendSQLValue(s.row, val, i)
	}
	s.row = append(s.row, ')')

	return s.appendValueTuple()
}

// WriteRawRow writes INSERT statement for a row using sql.RawBytes to avoid allocation
func (s *SQLWriter) WriteRawRow(values []sql.RawBytes) error {
	s.ensureColList()

	s.row = s.row[:0]
	s.row = append(s.row, '(')
	for i, val := range values {
		if i > 0 {
			s.row = append(s.row, ',')
		}
		s.row = s.appendRawSQLValue(s.row, val, i)
	}
	s.row = append(s.row, ')')

	return s.appendValueTuple()
}

// ensureColList builds the cached "INSERT INTO `t` (`a`, `b`) VALUES " prefix.
func (s *SQLWriter) ensureColList() {
	if len(s.colList) > 0 {
		return
	}
	buf := make([]byte, 0, 64)
	buf = append(buf, "INSERT INTO "...)
	buf = s.appendIdent(buf, s.tableName)
	buf = append(buf, " ("...)
	for i, col := range s.columns {
		if i > 0 {
			buf = append(buf, ',', ' ')
		}
		buf = s.appendIdent(buf, col)
	}
	buf = append(buf, ") VALUES "...)
	s.colList = buf
}

// appendIdent writes one identifier in the dialect's quoting. MySQL backquotes
// the name verbatim, byte for byte as the writer has always done; PostgreSQL and
// Dameng double-quote it and double any embedded double quote so a name that
// carries a quote survives the round trip (DM8 disql confirms backticks are a
// syntax error there and double-quoted names are case-sensitive).
func (s *SQLWriter) appendIdent(dst []byte, name string) []byte {
	if !s.doubleQuotesIdent() {
		dst = append(dst, '`')
		dst = append(dst, name...)
		return append(dst, '`')
	}
	dst = append(dst, '"')
	for i := 0; i < len(name); i++ {
		if name[i] == '"' {
			dst = append(dst, '"', '"')
			continue
		}
		dst = append(dst, name[i])
	}
	return append(dst, '"')
}

// appendValueTuple hands the tuple built in row to the output: in batched
// mode it joins the pending VALUES list, flushing the statement once
// --statement-size is reached; in single-row mode it writes the row as its own
// INSERT immediately.
func (s *SQLWriter) appendValueTuple() error {
	if s.singleRow {
		size := len(s.colList) + len(s.row) + 2 // ";\n"
		if size > s.statementSize {
			s.warnOversizedStatement(size)
		}
		return s.writeStatement(s.row)
	}

	if len(s.batch)+len(s.row)+len(s.colList) > s.statementSize && s.batchCount > 0 {
		if err := s.flushBatch(); err != nil {
			return err
		}
	}

	if s.batchCount > 0 {
		s.batch = append(s.batch, ',')
		s.everMerged = true
	}
	s.batch = s.growBatch(len(s.row) + 1)
	s.batch = append(s.batch, s.row...)
	s.batchCount++
	s.mergedTuples++
	return nil
}

// batchPrealloc caps how much the pending VALUES buffer reserves in one step.
// --statement-size may legally go up to 256 MiB, and reserving that per writer
// would cost a small export — and every parallel shard writer — far more memory
// than the rows it holds.
const batchPrealloc = 8 << 20

// batchFastLane is the size the pending buffer must reach before growth stops
// stepping and jumps straight to the statement budget. Below it a run that
// never fills a statement keeps allocating in small steps, so a five-row export
// pays 4 KiB instead of 8 MiB.
const batchFastLane = 1 << 20

// growBatch returns the pending buffer with room for n more bytes.
//
// The buffer used to grow by append's default doubling all the way to
// --statement-size, which threw away about the final size in copied garbage: a
// 4-thread, 16-range export of 3,000,000 rows spent 319 MB re-copying the batch,
// because every shard range builds its own writer and grows its own buffer from
// zero. Growth is ×4 up to batchFastLane and then jumps to the budget, so the
// same run reaches its 8 MiB statement size in five allocations (~1.3 MB of
// garbage) instead of twenty-three.
func (s *SQLWriter) growBatch(n int) []byte {
	if cap(s.batch)-len(s.batch) >= n {
		return s.batch
	}
	want := len(s.batch) + n
	size := cap(s.batch) * 4
	if size < 4096 {
		size = 4096
	}
	if cap(s.batch) >= batchFastLane {
		limit := s.statementSize
		if limit > batchPrealloc {
			limit = batchPrealloc
		}
		if size < limit {
			size = limit
		}
	}
	if size < want {
		size = want
	}
	b := make([]byte, len(s.batch), size)
	copy(b, s.batch)
	return b
}

// writeStatement emits one complete INSERT on exactly one physical line.
func (s *SQLWriter) writeStatement(tuple []byte) error {
	if _, err := s.bw.Write(s.colList); err != nil {
		return err
	}
	if _, err := s.bw.Write(tuple); err != nil {
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

// warnNeverMerged reports once per output file that the merged layout requested
// by --extended-insert never put two rows in the same statement. The flush rule
// in appendValueTuple fires before a second tuple can join, because every row is
// already wider than --statement-size on its own, so the dump comes out byte for
// byte identical to the default single-row layout. The user asked for merging and
// silently did not get it, which is the same situation warnOversizedStatement
// exists for on the other side of the layout switch. A file with a single row is
// excluded: one row cannot merge with anything whatever the size limit is, so
// nothing there contradicts the flag.
func (s *SQLWriter) warnNeverMerged() {
	common.Logger.Warn("--extended-insert never merged rows: every row alone exceeds --statement-size, so the dump is one INSERT per row",
		"statement_size", s.statementSize, "rows", s.mergedTuples)
}

// flushBatch writes the current batch INSERT statement
func (s *SQLWriter) flushBatch() error {
	if s.batchCount == 0 {
		return nil
	}

	if err := s.writeStatement(s.batch); err != nil {
		return err
	}

	// Reset batch state, keeping the buffer capacity for the next statement.
	s.batch = s.batch[:0]
	s.batchCount = 0
	return nil
}

// Flush writes buffered bytes to the underlying writer without emitting a
// footer. The parallel export merge step needs it: the preamble must land on
// disk before the shard files are appended to the same handle.
func (s *SQLWriter) Flush() error {
	return s.bw.Flush()
}

// WriteFooter flushes the last batched INSERT and the buffered writer.
func (s *SQLWriter) WriteFooter() error {
	// Flush any remaining batched INSERT statements
	if !s.singleRow && s.mergedTuples >= 2 && !s.everMerged {
		s.warnNeverMerged()
	}
	if err := s.flushBatch(); err != nil {
		return err
	}

	return s.bw.Flush()
}

// appendSQLValue appends the SQL literal for one value to dst. Numeric columns
// are emitted as bare literals; strings are quoted and escaped in place, with
// no intermediate string allocation on the common path.
func (s *SQLWriter) appendSQLValue(dst []byte, val interface{}, colIdx int) []byte {
	if val == nil {
		return append(dst, "NULL"...)
	}

	// Binary column with hex encoding: MySQL writes 0x..., PostgreSQL writes
	// '\x..'::bytea. Empty binary values keep a zero-length literal in each
	// dialect (0x and '\x'::bytea), not '' or NULL.
	if s.hexBlob && colIdx < len(s.binaryFlags) && s.binaryFlags[colIdx] {
		switch v := val.(type) {
		case []byte:
			return s.appendBinaryLiteral(dst, v)
		case string:
			return s.appendBinaryLiteral(dst, []byte(v))
		}
	}

	// Boolean column: spell the value in the grammar the writer is emitting. The
	// driver hands booleans over as text ("t"/"f" from PostgreSQL, "1"/"0" from
	// Dameng) or a Go bool; a bare token dropped into a VALUES list reads back
	// as a column reference on every engine, so the mark drives not just the
	// PostgreSQL dialect but whichever dialect is active.
	if colIdx < len(s.boolFlags) && s.boolFlags[colIdx] {
		return append(dst, s.dialectBoolLiteral(val)...)
	}

	// Numeric columns are emitted as bare SQL literals. The driver returns
	// numeric wire text as []byte, so without this branch every number would
	// be quoted like a string and the dump would diverge from mysqldump.
	if s.columnKind(colIdx) == common.KindNumber {
		switch v := val.(type) {
		case []byte:
			return append(dst, v...)
		case sql.RawBytes:
			return append(dst, v...)
		case string:
			return append(dst, v...)
		}
	}

	switch v := val.(type) {
	case []byte:
		return s.appendStringLiteral(dst, bytesToString(v))
	case string:
		return s.appendStringLiteral(dst, v)
	case time.Time:
		dst = append(dst, '\'')
		dst = append(dst, v.Format("2006-01-02 15:04:05")...)
		return append(dst, '\'')
	case int:
		return strconv.AppendInt(dst, int64(v), 10)
	case int8:
		return strconv.AppendInt(dst, int64(v), 10)
	case int16:
		return strconv.AppendInt(dst, int64(v), 10)
	case int32:
		return strconv.AppendInt(dst, int64(v), 10)
	case int64:
		return strconv.AppendInt(dst, v, 10)
	case uint:
		return strconv.AppendUint(dst, uint64(v), 10)
	case uint8:
		return strconv.AppendUint(dst, uint64(v), 10)
	case uint16:
		return strconv.AppendUint(dst, uint64(v), 10)
	case uint32:
		return strconv.AppendUint(dst, uint64(v), 10)
	case uint64:
		return strconv.AppendUint(dst, v, 10)
	case float32:
		return strconv.AppendFloat(dst, float64(v), 'f', -1, 32)
	case float64:
		return strconv.AppendFloat(dst, v, 'f', -1, 64)
	case bool:
		if s.isPG() {
			if v {
				return append(dst, "true"...)
			}
			return append(dst, "false"...)
		}
		if v {
			return append(dst, '1')
		}
		return append(dst, '0')
	default:
		return s.appendStringLiteral(dst, fmt.Sprintf("%v", v))
	}
}

// appendBinaryLiteral writes a binary column value in the dialect's hex form:
// MySQL 0x..., PostgreSQL '\x..'::bytea, Dameng HEXTORAW('..'). An empty value
// becomes 0x, '\x'::bytea and HEXTORAW(”) respectively, never ” or NULL. The
// live DM8 probe showed HEXTORAW is the one form accepted by both BLOB and
// VARBINARY columns — X'..' is rejected on BLOB and silently widens to a 4-byte
// integer, and 0x.. is VARBINARY-only — so it is the only safe choice here.
func (s *SQLWriter) appendBinaryLiteral(dst, v []byte) []byte {
	if s.isDM() {
		dst = append(dst, "HEXTORAW('"...)
		dst = hex.AppendEncode(dst, v)
		return append(dst, '\'', ')')
	}
	if s.isPG() {
		dst = append(dst, '\'', '\\', 'x')
		dst = hex.AppendEncode(dst, v)
		dst = append(dst, '\'', ':', ':')
		return append(dst, "bytea"...)
	}
	dst = append(dst, '0', 'x')
	return hex.AppendEncode(dst, v)
}

// appendStringLiteral dispatches a text value to the dialect's escaper.
func (s *SQLWriter) appendStringLiteral(dst []byte, str string) []byte {
	if s.isDM() {
		return appendQuotedSQLDM(dst, str)
	}
	if s.isPG() {
		return appendQuotedSQLPG(dst, str)
	}
	return appendQuotedSQL(dst, str)
}

// appendQuotedSQLDM appends a Dameng/Oracle-standard string literal: single
// quotes on the outside and a lone single quote doubled inside. Oracle-mode DM
// does NOT interpret a backslash as an escape (the live probe stored 'a\\b' as
// two literal backslashes), so unlike the MySQL and PG writers a backslash, a
// newline, a tab or any other byte is emitted verbatim inside the quotes. NUL
// cannot appear in a DM text value at all — the engine refuses a NUL byte in a
// text column and forces binary out as HEXTORAW — so there is nothing here to
// escape it into.
func appendQuotedSQLDM(dst []byte, s string) []byte {
	dst = append(dst, '\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			dst = append(dst, '\'', '\'')
			continue
		}
		dst = append(dst, s[i])
	}
	return append(dst, '\'')
}

// pgBoolLiteral reduces a boolean column's value to PostgreSQL's true/false
// spelling. The export path hands over the driver's "t"/"f" text (after
// CellFixups) or a real Go bool; anything that is not one of the explicit false
// words is treated as true, matching how PostgreSQL casts its own boolean text.
func pgBoolLiteral(val interface{}) []byte {
	var s string
	switch v := val.(type) {
	case []byte:
		s = string(v)
	case sql.RawBytes:
		s = string(v)
	case string:
		s = v
	case bool:
		if v {
			return []byte("true")
		}
		return []byte("false")
	default:
		return []byte("true")
	}
	switch s {
	case "", "f", "F", "false", "FALSE", "False", "0":
		return []byte("false")
	}
	return []byte("true")
}

// dialectBoolLiteral renders a boolean column's value in the grammar the writer
// is emitting. It reuses pgBoolLiteral only as a driver-text-to-truth reducer
// ("t"/"f", "1"/"0", "true"/"false", or a Go bool) and then spells that truth in
// the active dialect's accepted form: PostgreSQL true/false, MySQL and Dameng a
// bare 1/0 — both accept an integer literal for a boolean/BIT column, and the
// live DM8 probe confirmed BOOLEAN is permissive about it. Emitting the raw
// "t"/"f" text into a non-PG VALUES list would be read back as a column
// reference, which is the cross-dialect defect this closes.
func (s *SQLWriter) dialectBoolLiteral(val interface{}) []byte {
	truth := pgBoolLiteral(val)
	if s.isPG() {
		return truth
	}
	if truth[0] == 't' {
		return []byte{'1'}
	}
	return []byte{'0'}
}

// escapeBytes marks the bytes appendQuotedSQL rewrites, one lookup per byte
// instead of a seven-way switch. The switch version measured 8.07 ms over a
// 300,000-string corpus against 5.93 ms here (~26% of this scan, which is about
// 10% of a single-threaded SQL export's CPU time), and a word-at-a-time SWAR
// variant measured *slower* than both because values are short.
var escapeBytes = [256]bool{
	0: true, '\t': true, '\n': true, '\r': true, 0x1a: true, '\'': true, '\\': true,
}

// hasNeedEscape reports whether s contains a byte appendQuotedSQL rewrites.
//
// This is a byte loop rather than strings.ContainsAny: ContainsAny builds an
// ASCII set on every call for a non-constant pattern, which cost ~140 ms on a
// 3,000,000-row export. The set here is fixed and one pass over the bytes is
// what the append below would do anyway.
func hasNeedEscape(s string) bool {
	for i := 0; i < len(s); i++ {
		if escapeBytes[s[i]] {
			return true
		}
	}
	return false
}

// appendQuotedSQL appends a single-quoted, escaped SQL string literal.
// Single quotes use SQL-standard double-write (”), not backslash escape.
// Newline, carriage return, tab and backslash use backslash escapes so every
// INSERT fits on a single physical line (file is line-splittable).
func appendQuotedSQL(dst []byte, s string) []byte {
	dst = append(dst, '\'')
	if !hasNeedEscape(s) {
		dst = append(dst, s...)
		return append(dst, '\'')
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\'':
			dst = append(dst, '\'', '\'')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		case 0:
			dst = append(dst, '\\', '0')
		case 0x1a:
			dst = append(dst, '\\', 'Z')
		default:
			dst = append(dst, c)
		}
	}
	return append(dst, '\'')
}

// hasPGEscapes reports whether s carries a byte appendQuotedSQLPG has to rewrite.
// It reuses the MySQL escape set, which already marks NUL, and a value that trips
// it is written as an E” literal so PostgreSQL, whose default
// standard_conforming_strings=on would otherwise read a backslash as a literal,
// decodes the same bytes back.
func hasPGEscapes(s string) bool { return hasNeedEscape(s) }

// appendQuotedSQLPG appends a PostgreSQL string literal. A value with nothing to
// escape is a plain '...' with the single quote doubled internally. A value
// carrying a control byte or a backslash is written as E'...' with C-style
// escapes: \n, \r, \t for those controls, \\ for a backslash, ” or \' for a
// quote, and \xNN for the two bytes MySQL spells as \0 and \Z (NUL and 0x1A) so
// PostgreSQL sees the byte it means rather than a digit or letter.
func appendQuotedSQLPG(dst []byte, s string) []byte {
	if !hasPGEscapes(s) {
		dst = append(dst, '\'')
		dst = append(dst, s...)
		return append(dst, '\'')
	}
	dst = append(dst, 'E', '\'')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\'':
			dst = append(dst, '\'', '\'')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		case 0:
			dst = append(dst, '\\', 'x', '0', '0')
		case 0x1a:
			dst = append(dst, '\\', 'x', '1', 'a')
		default:
			dst = append(dst, c)
		}
	}
	return append(dst, '\'')
}

// appendRawSQLValue is appendSQLValue for the sql.RawBytes fast path.
func (s *SQLWriter) appendRawSQLValue(dst []byte, val sql.RawBytes, colIdx int) []byte {
	if val == nil {
		return append(dst, "NULL"...)
	}
	// Binary column with hex encoding: 0x... (MySQL) or '\x..'::bytea (PG).
	if s.hexBlob && colIdx < len(s.binaryFlags) && s.binaryFlags[colIdx] {
		return s.appendBinaryLiteral(dst, []byte(val))
	}
	if colIdx < len(s.boolFlags) && s.boolFlags[colIdx] {
		return append(dst, s.dialectBoolLiteral(val)...)
	}
	if s.columnKind(colIdx) == common.KindNumber {
		return append(dst, val...)
	}
	return s.appendStringLiteral(dst, bytesToString(val))
}
