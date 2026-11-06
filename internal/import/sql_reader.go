package _import

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"io"
	"strconv"
	"strings"

	"github.com/pkg/errors"
)

// SQLReader reads data in SQL format (CREATE TABLE + INSERT statements).
//
// A statement carries either one VALUES tuple (imex's own default layout)
// or many (--extended-insert and every dump tool's style), and it may be split
// across several physical lines. The reader reassembles each statement,
// tokenizes its tuples into rows and queues them for ReadRow.
//
// The statement level stays in []byte end to end. nextStatement hands out a
// window into the line buffer for a single-line statement and into its own
// scratch buffer only when a statement really spans lines; the parsers slice
// that window instead of re-rendering the statement as a string. Nothing copies
// a statement twice, there is no whole-statement upper-case copy, and the
// quoting state is carried across lines instead of re-scanning the text built
// so far. Only the values the caller keeps are copied, and only for the chunk of
// up to rowChunkRows rows being parsed, so the cost per row is proportional to
// its own data and the rows in flight are bounded by the chunk, not by the size
// of the largest statement in the file.
type SQLReader struct {
	rd      *recordReader
	stmt    []byte // scratch for a statement spanning several lines
	columns []string
	cur     insertCursor    // parse position inside the INSERT being read
	pending [][]interface{} // rows parsed from the current INSERT, not yet returned
	// unrecognized records a statement ReadHeader could not classify as an
	// INSERT or dump preamble (SET/LOCK/...). A file made only of preamble and
	// a CREATE TABLE line (a data-only dump of an empty table, or mysqldump's
	// DDL for it) is an empty dump, but one carrying other text is not: the
	// import has to say so instead of finishing with 0 rows.
	unrecognized bool
}

// NewSQLReader creates a new SQL format reader.
func NewSQLReader(r *bufio.Reader) *SQLReader {
	return &SQLReader{
		rd: newRecordReader(r, scanLines),
	}
}

// ReadHeader advances to the first INSERT statement, extracting the column
// list and buffering its rows. A CREATE TABLE line a foreign dump carries is
// skipped: imex replays no DDL, so the reader only looks for the INSERTs.
//
// A file carrying preamble and CREATE but no INSERT (a data-only dump of an
// empty table) is not an error: it returns io.EOF with only-preamble set, and
// the importer reports it as an empty file rather than a failure.
func (s *SQLReader) ReadHeader() ([]string, error) {
	for {
		stmt, ok := s.nextStatement()
		if !ok {
			if err := s.rd.Err(); err != nil {
				return nil, errors.Wrap(err, "failed to read SQL file")
			}
			return nil, io.EOF
		}

		head := bytes.TrimLeft(stmt, " \t")
		switch {
		case hasPrefixFold(head, "CREATE TABLE"), hasPrefixFold(head, "CREATE TEMPORARY"):
			// DDL from a foreign dump: recognized and skipped, never replayed, so
			// it does not count as unrecognized text either.
			continue
		case hasPrefixFold(head, "INSERT "):
			cols := s.extractColumns(stmt)
			s.columns = cols
			// The statement is parsed chunk by chunk from here on, so the header
			// only points the cursor at it: a one-million-row INSERT must not be
			// materialised before the importer has taken its first batch.
			s.cur.reset(stmt, len(cols))
			return cols, nil
		default:
			// Skip SET/LOCK/UNLOCK/other preamble statements. Anything else is
			// remembered so an empty-file success cannot swallow a file whose
			// text the reader does not understand.
			if !isPreambleStatement(head) {
				s.unrecognized = true
			}
			continue
		}
	}
}

// OnlyPreamble reports whether everything ReadHeader skipped was dump preamble
// (SET/LOCK/UNLOCK/USE/START/COMMIT/...) or a CREATE TABLE line. Together with a
// nil header that means the file is an empty dump — the shape a data-only export
// of an empty table has, `/*!40101 SET NAMES utf8mb4 */;` and nothing else — so
// importing it is a no-op rather than an error. Text the reader cannot classify
// makes this false, so a mistyped or truncated file still fails loudly.
func (s *SQLReader) OnlyPreamble() bool { return !s.unrecognized }

// nextStatement assembles one complete SQL statement (terminated by a top-level
// ';'), handling quoted strings and line comments. It returns the statement
// bytes and reports whether one was available; (nil, false) means end of input.
//
// The result is a window, valid until the next call: a single-line statement
// stays inside the recordReader's buffer and is never copied, and only a
// statement that continues onto another line is assembled in s.stmt.
func (s *SQLReader) nextStatement() ([]byte, bool) {
	s.stmt = s.stmt[:0]
	started := false
	var st stmtState

	for s.rd.Scan() {
		line := s.rd.Bytes()

		if !started {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 || hasCommentPrefix(trimmed) {
				continue
			}
			if _, complete := st.scan(trimmed); complete {
				return trimTrailingSemicolon(trimmed), true
			}
			// The statement continues on a following line, so the line window
			// is no longer safe to hand back: it is reused by the next Scan.
			started = true
			s.stmt = append(s.stmt, line...)
			continue
		}

		s.stmt = append(s.stmt, '\n')
		s.stmt = append(s.stmt, line...)
		if _, complete := st.scan(line); complete {
			return trimTrailingSemicolon(s.stmt), true
		}
	}

	if started {
		// File ended without a terminating ';' on the last statement.
		stmt := trimTrailingSemicolon(s.stmt)
		if len(bytes.TrimSpace(stmt)) > 0 {
			return stmt, true
		}
	}
	return nil, false
}

// stmtState is the quoting state machine behind nextStatement and insertCursor:
// whether the scan is inside a quoted string, which quote opened it, and whether
// the next byte is escaped. Only the quote characters, the backslash and ';'
// move it, so whitespace and newlines are inert. That lets a statement split
// across physical lines be scanned incrementally — one pass over each line —
// instead of re-walking the text assembled so far after every line, which was
// quadratic in statement length.
type stmtState struct {
	inString bool
	quote    byte
	escaped  bool
}

// scan advances the state over b and reports the offset of the first ';' that
// is outside a quoted string, if any.
func (st *stmtState) scan(b []byte) (int, bool) {
	for i := 0; i < len(b); i++ {
		ch := b[i]
		if st.escaped {
			st.escaped = false
			continue
		}
		if st.inString {
			switch ch {
			case '\\':
				st.escaped = true
			case st.quote:
				st.inString = false
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			st.inString = true
			st.quote = ch
		} else if ch == ';' {
			return i, true
		}
	}
	return 0, false
}

// trimTrailingSemicolon drops the statement terminator and any whitespace around
// it. A ';' that is not the last byte belongs to the statement body and stays.
func trimTrailingSemicolon(s []byte) []byte {
	s = bytes.TrimSpace(s)
	if len(s) > 0 && s[len(s)-1] == ';' {
		return bytes.TrimSpace(s[:len(s)-1])
	}
	return s
}

// hasCommentPrefix reports whether a line starts a SQL line comment.
func hasCommentPrefix(b []byte) bool {
	return bytes.HasPrefix(b, []byte("--")) || bytes.HasPrefix(b, []byte("/*"))
}

// hasPrefixFold compares b against an ASCII keyword that is written in upper
// case in the source. It folds only the ASCII range, so multi-byte characters in
// b keep their byte width and every offset the caller derives stays valid;
// strings.ToUpper offered no such guarantee because some code points change
// length when upper-cased.
func hasPrefixFold(b []byte, keyword string) bool {
	if len(b) < len(keyword) {
		return false
	}
	for i := 0; i < len(keyword); i++ {
		c := b[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c != keyword[i] {
			return false
		}
	}
	return true
}

// preambleKeywords are the statements a dump uses for session setup and
// bookkeeping rather than for data or structure. They are the only statements
// ReadHeader may skip without making the file "unrecognized".
var preambleKeywords = []string{
	"SET", "LOCK", "UNLOCK", "USE", "START", "BEGIN", "COMMIT", "ROLLBACK", "DELIMITER",
}

// isPreambleStatement reports whether head starts with one of those keywords as
// a whole word, so `SETX ...` is not mistaken for `SET ...`.
func isPreambleStatement(head []byte) bool {
	for _, kw := range preambleKeywords {
		if hasPrefixFold(head, kw) {
			if len(head) == len(kw) {
				return true
			}
			switch head[len(kw)] {
			case ' ', '\t', '\n', '\r', ';', '(':
				return true
			}
		}
	}
	return false
}

// indexFold returns the byte offset of the first ASCII case-insensitive
// occurrence of an upper-case keyword in b, or -1.
func indexFold(b []byte, keyword string) int {
	if len(b) < len(keyword) {
		return -1
	}
	for i := 0; i+len(keyword) <= len(b); i++ {
		if hasPrefixFold(b[i:], keyword) {
			return i
		}
	}
	return -1
}

// ReadRow returns the next buffered VALUES tuple, advancing through statements
// and parsing new INSERTs as needed.
func (s *SQLReader) ReadRow() ([]interface{}, error) {
	for len(s.pending) == 0 {
		// Finish the statement being read before opening the next one: the cursor
		// hands out its rows in bounded chunks, and the statement window it parses
		// from is only reused once it is exhausted.
		if rows := s.cur.nextChunk(); len(rows) > 0 {
			s.pending = rows
			break
		}
		stmt, ok := s.nextStatement()
		if !ok {
			if err := s.rd.Err(); err != nil {
				return nil, errors.Wrap(err, "failed to read SQL file")
			}
			return nil, io.EOF
		}
		head := bytes.TrimLeft(stmt, " \t")
		switch {
		case hasPrefixFold(head, "INSERT "):
			s.cur.reset(stmt, len(s.columns))
		default:
			// A CREATE TABLE or other statement between INSERTs is skipped; imex
			// replays no DDL.
			continue
		}
	}

	row := s.pending[0]
	s.pending = s.pending[1:]
	return row, nil
}

// extractColumns extracts column names from an INSERT statement's column list.
// The names are copies: the statement window they come from is reused by the
// next statement, while ReadHeader's caller keeps them for the whole run.
func (s *SQLReader) extractColumns(stmt []byte) []string {
	valuesIdx := indexFold(stmt, "VALUES")
	if valuesIdx == -1 {
		return nil
	}

	beforeValues := stmt[:valuesIdx]
	openParen := bytes.LastIndexByte(beforeValues, '(')
	closeParen := bytes.LastIndexByte(beforeValues, ')')
	if openParen == -1 || closeParen == -1 || closeParen <= openParen {
		return nil
	}

	parts := bytes.Split(beforeValues[openParen+1:closeParen], []byte{','})
	cols := make([]string, len(parts))
	for i, col := range parts {
		cols[i] = string(bytes.Trim(bytes.TrimSpace(col), "`\"'"))
	}
	return cols
}

// rowChunkRows bounds how many rows of one INSERT statement are materialised before
// the importer gets a piece of them.
//
// The import queue is measured in rows (--batch-size x --pipeline-buffer, 10,000 by
// default), but a dump that packs a million tuples into one INSERT used to be
// parsed end to end first: 24 MB of row headers and a 48 MB value arena stayed
// reachable for the whole run even though the importer could only be working on
// 10,000 rows at a time. Everything the collector had to mark sat in the reader, and
// peak RSS tracked the largest statement in the file rather than the queue. A chunk
// of 4,096 rows costs a few hundred KB, so the live set follows the queue and the
// statement window can stay the only large buffer in the run.
const rowChunkRows = 4096

// maxValuePrealloc bounds the value arena one chunk reserves up front. A chunk with
// more columns than that grows the ordinary way.
const maxValuePrealloc = 1 << 20 // 1,048,576 value slots (16 MB)

// tupleSpan is one VALUES tuple's body, as offsets into the statement's VALUES
// region: body is the first byte after '(' and end is the closing ')' itself.
type tupleSpan struct {
	body, end int
}

// insertCursor is the parse position inside one INSERT statement's VALUES region.
// The region is a window into the reader's statement buffer, which stays valid
// because the reader assembles the next statement only once the cursor is done.
//
// One cursor serves every statement in the run: reset clears the position but keeps
// the boundary scratch, so a dump of three million single-row INSERTs does not
// allocate a cursor per statement.
type insertCursor struct {
	after []byte      // the VALUES region of the current statement
	cols  int         // column count the header announced, for sizing the arena
	pos   int         // first unread byte of after
	done  bool        // after is exhausted
	spans []tupleSpan // this chunk's tuple boundaries; capacity rowChunkRows
}

// reset points the cursor at stmt's VALUES region and forgets any previous
// statement. It reports false when the statement carries no VALUES at all.
func (c *insertCursor) reset(stmt []byte, cols int) bool {
	valuesIdx := indexFold(stmt, "VALUES")
	if valuesIdx == -1 {
		return false
	}
	c.after = stmt[valuesIdx+len("VALUES"):]
	c.cols = cols
	c.pos = 0
	c.done = false
	return true
}

// nextChunk parses the next run of tuples, at most rowChunkRows of them, and returns
// the rows. Every plain value is a substring of the chunk's own copy of its bytes,
// so the rows outlive the statement window they were read from; an empty result
// means the statement is exhausted.
func (c *insertCursor) nextChunk() [][]interface{} {
	if c.done || c.pos >= len(c.after) {
		c.done = true
		return nil
	}
	start := c.pos
	if cap(c.spans) == 0 {
		c.spans = make([]tupleSpan, 0, rowChunkRows)
	}
	spans := c.spans[:0]

	// First pass: walk tuples and record their boundaries. The walk is the same
	// quoting state machine the value parser uses; because a chunk always stops at
	// a tuple boundary, the state restarts clean at the next chunk.
	var st stmtState
	i := start
	for i < len(c.after) && len(spans) < rowChunkRows {
		for i < len(c.after) && c.after[i] != '(' {
			i++
		}
		if i >= len(c.after) {
			break
		}
		i++ // skip '('
		body := i
		end := len(c.after)
		for i < len(c.after) {
			ch := c.after[i]
			if st.escaped {
				st.escaped = false
				i++
				continue
			}
			if st.inString {
				switch ch {
				case '\\':
					st.escaped = true
				case st.quote:
					st.inString = false
				}
				i++
				continue
			}
			if ch == '\'' || ch == '"' {
				st.inString = true
				st.quote = ch
				i++
				continue
			}
			if ch == ')' {
				end = i
				i++
				break
			}
			i++
		}
		spans = append(spans, tupleSpan{body: body, end: end})
		if end == len(c.after) && i >= len(c.after) && st.inString {
			// Truncated statement: the last tuple runs to the end of the region.
			break
		}
	}
	c.pos = i
	c.spans = spans
	if len(spans) == 0 {
		c.done = true
		return nil
	}
	if i >= len(c.after) {
		c.done = true
	}

	// The chunk's bytes are copied once; the statement window is not read again
	// after this, so the rows only keep this bounded copy alive.
	text := string(c.after[start:i])

	// Both containers are sized from the chunk's tuple count rather than grown by
	// append.
	rows := make([][]interface{}, 0, len(spans))
	slots := 0
	if c.cols > 0 {
		if len(spans) > maxValuePrealloc/c.cols {
			slots = maxValuePrealloc
		} else {
			slots = len(spans) * c.cols
		}
	}
	values := make([]interface{}, 0, slots)

	st = stmtState{}
	for _, sp := range spans {
		var row []interface{}
		values, row = parseValueList(values, c.after[sp.body:sp.end], text, sp.body-start, c.cols)
		rows = append(rows, row)
	}
	return rows
}

// isSpaceByte reports whether b is inter-token whitespace, the same set the
// token scanner skips.
func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// equalFoldNull compares a bare token against NULL without allocating: SQL
// keywords are ASCII, so folding a byte at a time is complete.
func equalFoldNull(b []byte) bool {
	const kw = "NULL"
	if len(b) != len(kw) {
		return false
	}
	for i := 0; i < len(kw); i++ {
		c := b[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c != kw[i] {
			return false
		}
	}
	return true
}

// parseValueList splits a single VALUES tuple body into typed values, appending
// them to dst and returning both the grown arena and the row window inside it.
// It walks the body token by token, treating single/double-quoted regions as
// string literals (delimiter-free, inner whitespace preserved) and everything
// else as a bare token (numbers, NULL, 0x hex literals, keywords). Surrounding
// inter-token whitespace is ignored; only content *inside* quotes is kept
// verbatim.
//
// The body is a window into text (textBase is its offset there): the chunk was
// copied once, so a plain literal or plain token becomes a substring of that
// copy with no allocation of its own. Only values that need rewriting — a
// collapsed doubled quote, a backslash escape, or a 0x literal — allocate.
func parseValueList(dst []interface{}, body []byte, text string, textBase, expected int) ([]interface{}, []interface{}) {
	start := len(dst)
	if expected > 0 && cap(dst)-start < expected {
		// Growth is rare: nextChunk sizes dst from the chunk's tuple count and
		// the header's column count. This only covers a row wider than its header.
		grown := make([]interface{}, len(dst), len(dst)+expected)
		copy(grown, dst)
		dst = grown
	}
	n := len(body)
	i := 0
	for {
		// Skip whitespace between tokens.
		for i < n && isSpaceByte(body[i]) {
			i++
		}
		if i >= n {
			break
		}

		var val interface{}
		// PostgreSQL E'' literal: a leading E/e before the opening quote marks a
		// string whose backslash escapes are decoded regardless of the server's
		// standard_conforming_strings. A MySQL dump has no bare leading E before a
		// quote, so this branch cannot be taken by MySQL text.
		if (body[i] == 'E' || body[i] == 'e') && i+1 < n && (body[i+1] == '\'' || body[i+1] == '"') {
			q := body[i+1]
			contentEnd, next, collapsed, _ := literalScan(body, i+2, q)
			inner := string(body[i+2 : contentEnd])
			if collapsed {
				inner = collapseDoubledQuotes(body[i+2:contentEnd], q)
			}
			val = pgUnescape(inner)
			i = next
		} else if q := body[i]; q == '\'' || q == '"' {
			// Quoted string literal: capture content without the delimiters.
			contentEnd, next, collapsed, escaped := literalScan(body, i+1, q)
			raw := body[i+1 : contentEnd]
			if isByteaCast(body[next:]) {
				// A closing quote followed by ::bytea is a PostgreSQL binary
				// literal, not a text value: decode the hex payload.
				val = decodeByteaLiteral(raw)
				next += len("::bytea")
				if next > n {
					next = n
				}
			} else if collapsed {
				val = unescapeSQLString(collapseDoubledQuotes(raw, q))
			} else if escaped {
				val = unescapeSQLString(string(raw))
			} else {
				val = text[textBase+i+1 : textBase+contentEnd]
			}
			i = next
		} else {
			// Bare token: read until the next top-level comma.
			end := i
			for end < n && body[end] != ',' {
				end++
			}
			token := bytes.TrimSpace(body[i:end])
			trimmed := strings.TrimSpace(text[textBase+i : textBase+end])
			i = end
			switch {
			case len(token) == 0 || equalFoldNull(token):
				val = nil
			case isHexLiteralBytes(token):
				val = decodeHexLiteralBytes(token)
			default:
				val = trimmed
			}
		}
		dst = append(dst, val)

		// Skip trailing whitespace, then consume the separating comma if present.
		for i < n && isSpaceByte(body[i]) {
			i++
		}
		if i < n && body[i] == ',' {
			i++
			continue
		}
		break
	}
	return dst, dst[start:]
}

// literalScan walks the quoted literal whose opening quote sits at body[start-1]
// and returns the end offset of its content, the offset to continue parsing at,
// and two flags describing how the value's text differs from the bytes it spans.
//
//   - collapsed: the content holds a doubled quote, which becomes one quote
//     character, so the text has to be rebuilt.
//   - escaped: the content holds a backslash escape. The escape pairs are part of
//     the window verbatim, so no rebuild is needed, but the value must still go
//     through unescapeSQLString. A backslash at the very end of an unterminated
//     literal is excluded from the window, matching the per-byte path it replaced.
//
// With neither flag the value is exactly the window, which is the common case and
// costs one copy.
func literalScan(body []byte, start int, q byte) (contentEnd, next int, collapsed, escaped bool) {
	i := start
	for i < len(body) {
		ch := body[i]
		if ch == '\\' {
			if i+1 >= len(body) {
				return i, i + 1, collapsed, true
			}
			escaped = true
			i += 2
			continue
		}
		if ch == q {
			if i+1 < len(body) && body[i+1] == q {
				i += 2
				collapsed = true
				continue
			}
			return i, i + 1, collapsed, escaped
		}
		i++
	}
	return len(body), len(body), collapsed, escaped
}

// collapseDoubledQuotes renders a literal's content with every doubled quote
// reduced to one quote character. Backslash pairs are copied verbatim, so the
// result still needs unescapeSQLString, exactly like the per-byte path it
// replaced.
func collapseDoubledQuotes(raw []byte, q byte) string {
	var sb strings.Builder
	sb.Grow(len(raw))
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		if ch == '\\' && i+1 < len(raw) {
			i++
			sb.WriteByte(ch)
			sb.WriteByte(raw[i])
			continue
		}
		if ch == q && i+1 < len(raw) && raw[i+1] == q {
			i++
			sb.WriteByte(q)
			continue
		}
		sb.WriteByte(ch)
	}
	return sb.String()
}

// isHexLiteralBytes reports whether token is a MySQL hex literal (0x... or 0X...).
func isHexLiteralBytes(token []byte) bool {
	return len(token) >= 2 && token[0] == '0' && (token[1] == 'x' || token[1] == 'X')
}

// decodeHexLiteralBytes decodes a MySQL 0x hex literal to bytes. An isolated "0x"
// (zero-length hex body) returns []byte{} rather than nil, consistent with the
// DEL reader's hex decoding.
func decodeHexLiteralBytes(token []byte) []byte {
	body := token[2:] // strip "0x" or "0X"
	if len(body) == 0 {
		return []byte{}
	}
	if len(body)%2 != 0 {
		// Malformed hex: pass the literal through as raw bytes so the caller can
		// still bind it (the server will reject it anyway).
		return append([]byte(nil), token...)
	}
	dst := make([]byte, len(body)/2)
	if _, err := hex.Decode(dst, body); err != nil {
		return append([]byte(nil), token...)
	}
	return dst
}

// decodeHexField decodes a 0x hex literal in a context where the token could
// also be ordinary text, and reports whether it really was a hex literal. It is
// stricter than decodeHexLiteralBytes: odd-length or non-hex bodies are rejected
// instead of being passed through, so the caller can leave such a field as text.
// An isolated "0x" decodes to an empty (non-nil) byte slice, matching the SQL
// reader and the DEL exporter, which write an empty blob as 0x.
func decodeHexField(token string) ([]byte, bool) {
	if len(token) < 2 || token[0] != '0' || (token[1] != 'x' && token[1] != 'X') {
		return nil, false
	}
	body := token[2:]
	if body == "" {
		return []byte{}, true
	}
	if len(body)%2 != 0 {
		return nil, false
	}
	b, err := hex.DecodeString(body)
	if err != nil {
		return nil, false
	}
	return b, true
}

func unescapeSQLString(s string) string {
	if !strings.ContainsAny(s, "\\") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case '0':
				b.WriteByte(0)
			case 'Z':
				b.WriteByte(0x1a)
			case '\\':
				b.WriteByte('\\')
			case '\'':
				b.WriteByte('\'')
			case '"':
				b.WriteByte('"')
			default:
				b.WriteByte(s[i+1])
			}
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// isByteaCast reports whether b begins with the PostgreSQL type annotation
// "::bytea" (case-insensitive) that the writer appends to a hex binary literal.
func isByteaCast(b []byte) bool {
	const cast = "::bytea"
	if len(b) < len(cast) {
		return false
	}
	for i := 0; i < len(cast); i++ {
		c := b[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != cast[i] {
			return false
		}
	}
	return true
}

// decodeByteaLiteral turns the text between the quotes of a '\x41'::bytea
// literal into bytes. The \x prefix is optional for tolerance; an empty body is
// an empty (non-nil) slice, matching the SQL writer, and malformed hex is
// handed back raw so the caller can still try to bind it.
func decodeByteaLiteral(raw []byte) []byte {
	if len(raw) >= 2 && raw[0] == '\\' && (raw[1] == 'x' || raw[1] == 'X') {
		raw = raw[2:]
	}
	if len(raw) == 0 {
		return []byte{}
	}
	if len(raw)%2 != 0 {
		return append([]byte(nil), raw...)
	}
	dst := make([]byte, len(raw)/2)
	if _, err := hex.Decode(dst, raw); err != nil {
		return append([]byte(nil), raw...)
	}
	return dst
}

// pgUnescape decodes the body of a PostgreSQL E” literal: the C-style escapes
// \n \r \t \b \f \v, a doubled backslash, an escaped quote, \xNN hexadecimal and
// \NNN octal, and any other backslash sequence as its literal character (the
// rule PostgreSQL applies by dropping the backslash). It is reached only from the
// E” branch, so MySQL text is unaffected.
func pgUnescape(s string) string {
	if !strings.ContainsAny(s, "\\") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		nxt := s[i+1]
		switch {
		case nxt == 'n':
			b.WriteByte('\n')
			i++
		case nxt == 'r':
			b.WriteByte('\r')
			i++
		case nxt == 't':
			b.WriteByte('\t')
			i++
		case nxt == 'b':
			b.WriteByte('\b')
			i++
		case nxt == 'f':
			b.WriteByte('\f')
			i++
		case nxt == 'v':
			b.WriteByte('\v')
			i++
		case nxt == '\\':
			b.WriteByte('\\')
			i++
		case nxt == '\'':
			b.WriteByte('\'')
			i++
		case nxt == '"':
			b.WriteByte('"')
			i++
		case nxt == 'x' || nxt == 'X':
			if i+3 < len(s) && isHexDigit(s[i+2]) && isHexDigit(s[i+3]) {
				if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
					b.WriteByte(byte(v))
					i += 3
					continue
				}
			}
			b.WriteByte('x')
			i++
		case nxt >= '0' && nxt <= '7':
			j := i + 1
			v := 0
			for k := 0; j < len(s) && s[j] >= '0' && s[j] <= '7' && k < 3; j++ {
				v = v*8 + int(s[j]-'0')
				k++
			}
			b.WriteByte(byte(v))
			i = j - 1
		default:
			b.WriteByte(nxt)
			i++
		}
	}
	return b.String()
}

// isHexDigit reports whether c is 0-9, a-f or A-F.
func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
