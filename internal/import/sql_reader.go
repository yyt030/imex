package _import

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"io"
	"strings"

	"github.com/pkg/errors"
)

// SQLReader reads data in SQL format (CREATE TABLE + INSERT statements).
//
// A statement carries either one VALUES tuple (gomydump's own default layout)
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
// so far. Only the values the caller keeps are copied, so the cost per row is
// proportional to its own data, not to the size of the statement it sits in.
type SQLReader struct {
	rd          *recordReader
	stmt        []byte // scratch for a statement spanning several lines
	columns     []string
	createStmts []string        // captured CREATE TABLE statements for create mode
	pending     [][]interface{} // rows parsed from the current INSERT, not yet returned
	hasData     bool            // ReadHeader found at least one INSERT statement
}

// NewSQLReader creates a new SQL format reader.
func NewSQLReader(r *bufio.Reader) *SQLReader {
	return &SQLReader{
		rd: newRecordReader(r, scanLines),
	}
}

// ReadHeader advances to the first INSERT statement, extracting the column
// list and buffering its rows. CREATE TABLE statements encountered along the
// way are captured for create mode.
//
// A file with CREATE statements but no INSERTs (a --no-data export) is not an
// error: it returns no columns and HasData() reports false, so create mode can
// still apply the captured structure.
func (s *SQLReader) ReadHeader() ([]string, error) {
	for {
		stmt, ok := s.nextStatement()
		if !ok {
			if err := s.rd.Err(); err != nil {
				return nil, errors.Wrap(err, "failed to read SQL file")
			}
			if len(s.createStmts) > 0 {
				return nil, nil
			}
			return nil, io.EOF
		}

		head := bytes.TrimLeft(stmt, " \t")
		switch {
		case hasPrefixFold(head, "CREATE TABLE"), hasPrefixFold(head, "CREATE TEMPORARY"):
			// CREATE statements are kept for create mode, so they outlive the
			// statement window and must be copied. A file carries a handful.
			s.createStmts = append(s.createStmts, string(stmt))
		case hasPrefixFold(head, "INSERT "):
			cols := s.extractColumns(stmt)
			s.columns = cols
			s.hasData = true
			rows := s.extractRows(stmt)
			s.pending = rows
			return cols, nil
		default:
			// Skip SET/LOCK/UNLOCK/other preamble statements.
			continue
		}
	}
}

// HasData reports whether ReadHeader found an INSERT statement. It is false for
// a structure-only SQL file.
func (s *SQLReader) HasData() bool { return s.hasData }

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

// stmtState is the quoting state machine behind nextStatement and extractRows:
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

// GetCreateStatements returns captured CREATE TABLE statements
func (s *SQLReader) GetCreateStatements() []string {
	return s.createStmts
}

// ReadRow returns the next buffered VALUES tuple, advancing through statements
// and parsing new INSERTs as needed.
func (s *SQLReader) ReadRow() ([]interface{}, error) {
	for len(s.pending) == 0 {
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
			s.pending = s.extractRows(stmt)
		case hasPrefixFold(head, "CREATE TABLE"), hasPrefixFold(head, "CREATE TEMPORARY"):
			s.createStmts = append(s.createStmts, string(stmt))
		default:
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

// extractRows parses every VALUES tuple in an INSERT statement into rows. Each
// tuple is handed to parseValueList as a window into the statement instead of a
// freshly copied buffer, which is what keeps the row cost independent of how
// many tuples share the statement.
func (s *SQLReader) extractRows(stmt []byte) [][]interface{} {
	valuesIdx := indexFold(stmt, "VALUES")
	if valuesIdx == -1 {
		return nil
	}

	after := stmt[valuesIdx+len("VALUES"):]

	var rows [][]interface{}
	var st stmtState
	i := 0
	for i < len(after) {
		// Find next tuple opening '('
		for i < len(after) && after[i] != '(' {
			i++
		}
		if i >= len(after) {
			break
		}
		i++ // skip '('
		body := i

		// Walk to the tuple's closing ')' at top level: inside a quoted value
		// a ')' is data, and a backslash consumes the next byte.
		closed := false
		for i < len(after) {
			ch := after[i]
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
				i++
				closed = true
				break
			}
			i++
		}
		end := len(after)
		if closed {
			end = i - 1 // offset of the closing ')'
		}
		rows = append(rows, parseValueList(after[body:end]))
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

// parseValueList splits a single VALUES tuple body into typed values. It walks
// the body token by token, treating single/double-quoted regions as string
// literals (delimiter-free, inner whitespace preserved) and everything else as
// a bare token (numbers, NULL, 0x hex literals, keywords). Surrounding
// inter-token whitespace is ignored; only content *inside* quotes is kept
// verbatim.
//
// The body is a window, so every value that is returned as a string or byte
// slice is copied out of it. The fast path for a plain literal or a plain token
// is that copy and nothing else.
func parseValueList(body []byte) []interface{} {
	var values []interface{}
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
		if q := body[i]; q == '\'' || q == '"' {
			// Quoted string literal: capture content without the delimiters.
			contentEnd, next, collapsed, escaped := literalScan(body, i+1, q)
			raw := body[i+1 : contentEnd]
			switch {
			case collapsed:
				val = unescapeSQLString(collapseDoubledQuotes(raw, q))
			case escaped:
				val = unescapeSQLString(string(raw))
			default:
				val = string(raw)
			}
			i = next
		} else {
			// Bare token: read until the next top-level comma.
			end := i
			for end < n && body[end] != ',' {
				end++
			}
			token := bytes.TrimSpace(body[i:end])
			i = end
			switch {
			case len(token) == 0 || equalFoldNull(token):
				val = nil
			case isHexLiteralBytes(token):
				val = decodeHexLiteralBytes(token)
			default:
				val = string(token)
			}
		}
		values = append(values, val)

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
	return values
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
