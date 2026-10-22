package _import

import (
	"bufio"
	"encoding/hex"
	"io"
	"strings"

	"github.com/pkg/errors"
)

// SQLReader reads data in SQL format (CREATE TABLE + INSERT statements).
//
// The exporter emits one INSERT statement per batch (multiple VALUES tuples),
// so a single INSERT line can represent many rows. This reader tokenizes each
// VALUES tuple into its own row and queues them for ReadRow.
type SQLReader struct {
	rd          *recordReader
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

		upper := strings.ToUpper(strings.TrimLeft(stmt, " \t"))
		switch {
		case strings.HasPrefix(upper, "CREATE TABLE"), strings.HasPrefix(upper, "CREATE TEMPORARY"):
			s.createStmts = append(s.createStmts, stmt)
		case strings.HasPrefix(upper, "INSERT "):
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
// ';'), handling quoted strings and line comments. Returns (stmt, true) or
// ("", false) at EOF with no pending data.
func (s *SQLReader) nextStatement() (string, bool) {
	var sb strings.Builder
	started := false

	for s.rd.Scan() {
		line := s.rd.Text()

		if !started {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "--") || strings.HasPrefix(trimmed, "/*") {
				continue
			}
			started = true
		}

		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(line)

		if statementComplete(sb.String()) {
			stmt := sb.String()
			return strings.TrimSpace(trimTrailingSemicolon(stmt)), true
		}
	}

	if started {
		stmt := strings.TrimSpace(sb.String())
		if stmt != "" {
			return strings.TrimSpace(trimTrailingSemicolon(stmt)), true
		}
	}
	return "", false
}

// statementComplete reports whether the accumulated buffer ends with a ';' that
// is not inside a quoted string.
func statementComplete(buf string) bool {
	inString := false
	var quote byte
	escaped := false
	for i := 0; i < len(buf); i++ {
		ch := buf[i]
		if escaped {
			escaped = false
			continue
		}
		if inString {
			switch ch {
			case '\\':
				escaped = true
			case quote:
				inString = false
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			inString = true
			quote = ch
		} else if ch == ';' {
			return true
		}
	}
	return false
}

func trimTrailingSemicolon(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, ";") {
		return strings.TrimSpace(s[:len(s)-1])
	}
	return s
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
		upper := strings.ToUpper(strings.TrimLeft(stmt, " \t"))
		switch {
		case strings.HasPrefix(upper, "INSERT "):
			s.pending = s.extractRows(stmt)
		case strings.HasPrefix(upper, "CREATE TABLE"), strings.HasPrefix(upper, "CREATE TEMPORARY"):
			s.createStmts = append(s.createStmts, stmt)
		default:
			continue
		}
	}

	row := s.pending[0]
	s.pending = s.pending[1:]
	return row, nil
}

// extractColumns extracts column names from an INSERT statement's column list.
func (s *SQLReader) extractColumns(line string) []string {
	upper := strings.ToUpper(line)
	valuesIdx := strings.Index(upper, "VALUES")
	if valuesIdx == -1 {
		return nil
	}

	beforeValues := line[:valuesIdx]
	openParen := strings.LastIndex(beforeValues, "(")
	closeParen := strings.LastIndex(beforeValues, ")")
	if openParen == -1 || closeParen == -1 || closeParen <= openParen {
		return nil
	}

	colStr := beforeValues[openParen+1 : closeParen]
	cols := strings.Split(colStr, ",")
	for i, col := range cols {
		col = strings.TrimSpace(col)
		col = strings.Trim(col, "`\"'")
		cols[i] = col
	}
	return cols
}

// extractRows parses every VALUES tuple in an INSERT statement into rows.
func (s *SQLReader) extractRows(line string) [][]interface{} {
	upper := strings.ToUpper(line)
	valuesIdx := strings.Index(upper, "VALUES")
	if valuesIdx == -1 {
		return nil
	}

	after := line[valuesIdx+len("VALUES"):]

	var rows [][]interface{}
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

		var content strings.Builder
		inString := false
		var quote byte
		escaped := false
		for i < len(after) {
			ch := after[i]
			if escaped {
				content.WriteByte(ch)
				escaped = false
				i++
				continue
			}
			if inString {
				switch ch {
				case '\\':
					content.WriteByte(ch)
					escaped = true
				case quote:
					inString = false
					content.WriteByte(ch)
				default:
					content.WriteByte(ch)
				}
				i++
				continue
			}
			if ch == '\'' || ch == '"' {
				inString = true
				quote = ch
				content.WriteByte(ch)
				i++
				continue
			}
			if ch == ')' {
				i++ // skip ')'
				break
			}
			content.WriteByte(ch)
			i++
		}

		rows = append(rows, s.parseValueList(content.String()))
	}

	return rows
}

// parseValueList splits a single VALUES tuple body into typed values. It walks
// the body token by token, treating single/double-quoted regions as string
// literals (delimiter-free, inner whitespace preserved) and everything else as
// a bare token (numbers, NULL, 0x hex literals, keywords). Surrounding
// inter-token whitespace is ignored; only content *inside* quotes is kept
// verbatim.
func (s *SQLReader) parseValueList(body string) []interface{} {
	var values []interface{}
	n := len(body)
	i := 0
	for {
		// Skip whitespace between tokens.
		for i < n && (body[i] == ' ' || body[i] == '\t' || body[i] == '\n' || body[i] == '\r') {
			i++
		}
		if i >= n {
			break
		}

		var val interface{}
		if q := body[i]; q == '\'' || q == '"' {
			// Quoted string literal: capture content without the delimiters.
			i++
			var sb strings.Builder
			escaped := false
			for i < n {
				ch := body[i]
				if escaped {
					sb.WriteByte('\\')
					sb.WriteByte(ch)
					escaped = false
					i++
					continue
				}
				if ch == '\\' {
					escaped = true
					i++
					continue
				}
				if ch == q {
					// Doubled quote -> a literal quote inside the string.
					if i+1 < n && body[i+1] == q {
						sb.WriteByte(q)
						i += 2
						continue
					}
					i++ // consume closing quote; token ends here
					break
				}
				sb.WriteByte(ch)
				i++
			}
			val = unescapeSQLString(sb.String())
		} else {
			// Bare token: read until the next top-level comma.
			var sb strings.Builder
			for i < n && body[i] != ',' {
				sb.WriteByte(body[i])
				i++
			}
			token := strings.TrimSpace(sb.String())
			if strings.EqualFold(token, "NULL") || token == "" {
				val = nil
			} else if isHexLiteral(token) {
				// 0x hex literal: decode to []byte. An isolated "0x" (zero-length
				// hex) decodes to []byte{} — matches DEL reader behaviour.
				val = decodeHexLiteral(token)
			} else {
				val = token
			}
		}
		values = append(values, val)

		// Skip trailing whitespace, then consume the separating comma if present.
		for i < n && (body[i] == ' ' || body[i] == '\t' || body[i] == '\n' || body[i] == '\r') {
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

// isHexLiteral reports whether token is a MySQL hex literal (0x... or 0X...).
func isHexLiteral(token string) bool {
	return len(token) >= 2 && token[0] == '0' && (token[1] == 'x' || token[1] == 'X')
}

// decodeHexLiteral decodes a MySQL 0x hex literal to bytes. An isolated "0x"
// (zero-length hex body) returns []byte{} rather than nil, consistent with the
// DEL reader's hex decoding.
func decodeHexLiteral(token string) []byte {
	hexStr := token[2:] // strip "0x" or "0X"
	if len(hexStr) == 0 {
		return []byte{}
	}
	b, err := hex.DecodeString(hexStr)
	if err != nil {
		// Malformed hex: return as raw bytes of the literal itself so the
		// caller can still bind it (the server will reject it anyway).
		return []byte(token)
	}
	return b
}

// decodeHexField decodes a 0x hex literal in a context where the token could
// also be ordinary text, and reports whether it really was a hex literal. It is
// stricter than decodeHexLiteral: odd-length or non-hex bodies are rejected
// instead of being passed through, so the caller can leave such a field as text.
// An isolated "0x" decodes to an empty (non-nil) byte slice, matching the SQL
// reader and the DEL exporter, which write an empty blob as 0x.
func decodeHexField(token string) ([]byte, bool) {
	if !isHexLiteral(token) {
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
