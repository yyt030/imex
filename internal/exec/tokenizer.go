package exec

import (
	"strings"

	"github.com/imex/internal/backend"
)

// The script splitter lives here because "split a SQL file into statements" is
// not a portable operation the way the rest of this command is: which quoting
// and comment syntaxes hide a terminator is engine vocabulary. A backslash is an
// escape inside a MySQL string literal and a literal backslash inside a Dameng
// one; a leading '#' is a MySQL comment and an ordinary character in Oracle-mode
// DM; a dollar-quoted string exists only in PostgreSQL. Splitting on every byte
// ';' regardless would break 'a;b' inside a column COMMENT and $$ ... ; ... $$
// inside a PostgreSQL function body, and a wrong split does not report an error
// — it silently runs half a statement.
//
// The delimiter-aware top-level split is what lets `imex exec` replay a saved
// script one statement at a time (see the exec change record in
// docs/requirements.md). It is the client-side half of what the mysql and psql
// command-line clients do; it is not a general SQL parser and understands only
// statement boundaries, never statement grammar.

// tokenizerFlags are the engine-specific lexical features a ';' can hide behind.
type tokenizerFlags struct {
	// backslashEscape: inside a quoted literal a backslash escapes the next byte,
	// so \' and \" do not close the literal (MySQL).
	backslashEscape bool
	// hashComment: a '#' starts a comment running to end of line (MySQL).
	hashComment bool
	// backtick: a '`' quotes an identifier, so a ';' inside it is not a boundary
	// (MySQL).
	backtick bool
	// dollarQuote: a $tag$ ... $tag$ literal has no escape rules and must be
	// skipped to its closing tag (PostgreSQL).
	dollarQuote bool
}

func tokenizerFlagsFor(e backend.Engine) tokenizerFlags {
	switch e {
	case backend.EngineMySQL:
		return tokenizerFlags{backslashEscape: true, hashComment: true, backtick: true}
	case backend.EnginePG:
		return tokenizerFlags{dollarQuote: true}
	default:
		// Dameng Oracle-mode and any future engine follow the SQL-standard lexical
		// rules: doubled single quotes escape, no backslash escape, no '#' comment,
		// no backticks, no dollar quotes. Erring toward the standard reading is what
		// keeps an unrecognised engine from splitting inside a literal.
		return tokenizerFlags{}
	}
}

// SplitStatements divides one SQL script into individual statements at each
// top-level occurrence of the active terminator. The terminator starts as ';'
// and can be redefined by a client-side DELIMITER directive. An unterminated
// trailing statement is still returned, because a script that does not end in a
// terminator is valid and dropping its last statement silently would lose a
// table. Regions that are empty or contain only whitespace and comments are
// skipped, so comment text attached to a real statement travels with it (an
// optimizer hint at the start survives) while a stand-alone comment is not sent.
func SplitStatements(script string, e backend.Engine) []string {
	return splitStatements(script, tokenizerFlagsFor(e))
}

func splitStatements(script string, fl tokenizerFlags) []string {
	b := []byte(script)
	n := len(b)
	delim := []byte(";")
	var out []string

	start := 0           // first byte of the statement region in progress
	sawAnything := false // the region has seen a non-whitespace byte
	i := 0

	// emit takes the region [start, codeEnd) — up to, but not including, the
	// terminator — trims it, records it when it holds something to send, and
	// resumes the next region at nextStart (just past the terminator).
	emit := func(codeEnd, nextStart int) {
		s := strings.TrimSpace(string(b[start:codeEnd]))
		if s != "" && !onlyComments(s, fl) {
			out = append(out, s)
		}
		start = nextStart
		sawAnything = false
	}

	for i < n {
		// At the start of a region (nothing but whitespace consumed so far) a
		// DELIMITER directive may appear on its own line. It redefines the
		// terminator the client splits on and is never sent to the server — the
		// only way a routine body containing ';' can be split correctly is for the
		// client to honour the switch the dump makes to '//' or '$'.
		if !sawAnything {
			switch c := b[i]; {
			case c == ' ' || c == '\t' || c == '\r' || c == '\n':
				i++
				continue
			case matchDirective(b[i:]):
				nd, relNext := parseDelimiter(b[i:])
				delim = nd
				i, start = i+relNext, i+relNext
				continue
			}
			sawAnything = true // the region now has a first real byte; no directive below it
		}

		switch c := b[i]; {
		// Line comments: '--' for every engine, '#' for MySQL. Either ends at the
		// newline, which stays inside the region.
		case c == '-' && i+1 < n && b[i+1] == '-':
			i = endOfLine(b, i)
		case c == '#' && fl.hashComment:
			i = endOfLine(b, i)

		// Block comment /* ... */, closed by the first '*/' or by end of input.
		case c == '/' && i+1 < n && b[i+1] == '*':
			i = endBlockComment(b, i)

		// Single-quoted string.
		case c == '\'':
			i = skipQuoted(b, i+1, '\'', fl)

		// Double-quoted: a string in MySQL's default mode, an identifier in ANSI
		// and Oracle mode. Both hide a ';' and both double the closing quote to
		// escape it, so the same scan is right whichever reading the server gives.
		case c == '"':
			i = skipQuoted(b, i+1, '"', fl)

		// Backtick identifier (MySQL only).
		case c == '`' && fl.backtick:
			i = skipQuoted(b, i+1, '`', tokenizerFlags{})

		// Dollar-quoted string (PostgreSQL only). A '$' that opens no tag falls
		// through to the terminator test so an ordinary '$' is not swallowed.
		case c == '$' && fl.dollarQuote:
			if end, ok := skipDollarQuoted(b, i); ok {
				i = end
				break
			}
			if matchesDelimiter(b, i, delim) {
				emit(i, i+len(delim))
				i += len(delim)
				continue
			}
			i++

		default:
			if matchesDelimiter(b, i, delim) {
				emit(i, i+len(delim))
				i += len(delim)
				continue
			}
			i++
		}
	}

	// Whatever remains after the last terminator is the final statement, valid
	// even without a trailing ';'.
	if start < n {
		emit(n, n)
	}
	return out
}

// onlyComments reports whether a trimmed statement is nothing but whitespace and
// comments, i.e. it has no executable token. A statement that carries any byte
// outside a comment — including a bare string literal — is not comment-only.
func onlyComments(s string, fl tokenizerFlags) bool {
	b := []byte(s)
	i, n := 0, len(b)
	for i < n {
		switch c := b[i]; {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case c == '-' && i+1 < n && b[i+1] == '-':
			i = endOfLine(b, i)
		case c == '#' && fl.hashComment:
			i = endOfLine(b, i)
		case c == '/' && i+1 < n && b[i+1] == '*':
			i = endBlockComment(b, i)
		default:
			return false
		}
	}
	return true
}

// matchesDelimiter reports whether the terminator begins at i.
func matchesDelimiter(b []byte, i int, delim []byte) bool {
	if i+len(delim) > len(b) {
		return false
	}
	return string(b[i:i+len(delim)]) == string(delim)
}

// endOfLine returns the index of the newline at or after i (the newline stays in
// the text), or len(b) when there is none.
func endOfLine(b []byte, i int) int {
	for i < len(b) && b[i] != '\n' {
		i++
	}
	return i
}

// endBlockComment returns the index just past the '*/' closing the comment that
// opens at i, or len(b) when the comment never closes.
func endBlockComment(b []byte, i int) int {
	i += 2 // past '/*'
	for i+1 < len(b) {
		if b[i] == '*' && b[i+1] == '/' {
			return i + 2
		}
		i++
	}
	return len(b)
}

// skipQuoted returns the index just past the closing quote of a quoted region
// whose opening quote has already been consumed (i is one past it). Inside, a
// doubled close quote escapes itself, and — when fl.backslashEscape — a backslash
// escapes the next byte. An unclosed literal runs to end of input, the safe
// reading: the trailing bytes stay part of the current statement rather than
// being split as a new one.
func skipQuoted(b []byte, i int, quote byte, fl tokenizerFlags) int {
	for i < len(b) {
		c := b[i]
		if fl.backslashEscape && c == '\\' {
			i += 2
			continue
		}
		if c == quote {
			if i+1 < len(b) && b[i+1] == quote {
				i += 2
				continue
			}
			return i + 1
		}
		i++
	}
	return i
}

// skipDollarQuoted recognises a PostgreSQL dollar-quoted string starting at the
// '$' at i and returns the index just past its closing tag with ok=true, or
// ok=false when i opens no valid tag. A tag is '$' [A-Za-z0-9_]* '$'; the body
// runs to the next identical tag.
func skipDollarQuoted(b []byte, i int) (int, bool) {
	j := i + 1
	for j < len(b) {
		if b[j] == '$' {
			break
		}
		if !isTagChar(b[j]) {
			return 0, false // a plain '$', not a tag opener
		}
		j++
	}
	if j >= len(b) {
		return 0, false // no closing '$' of the tag: not a dollar quote
	}
	tag := string(b[i : j+1]) // both '$' and the identifier between them
	end := strings.Index(string(b[j+1:]), tag)
	if end < 0 {
		return len(b), true // unterminated: consume the rest as one literal
	}
	return j + 1 + end + len(tag), true
}

func isTagChar(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// matchDirective reports whether the slice begins with the DELIMITER keyword
// (case-insensitive) followed by a space or tab. Requiring the separator avoids
// matching an identifier or statement that merely begins with those letters.
func matchDirective(b []byte) bool {
	const kw = "delimiter"
	if len(b) < len(kw)+1 {
		return false
	}
	for k := 0; k < len(kw); k++ {
		c := b[k]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != kw[k] {
			return false
		}
	}
	return b[len(kw)] == ' ' || b[len(kw)] == '\t'
}

// parseDelimiter reads the new terminator from a DELIMITER directive line and
// returns it with the index of the first byte of the following line. The
// terminator is the rest of the line trimmed; an empty value resets it to ';',
// matching the client. Its bytes are taken literally, which is the point of the
// command: it names a terminator that needs no escaping.
func parseDelimiter(b []byte) (delim []byte, next int) {
	const kwLen = len("delimiter")
	lineEnd := endOfLine(b, 0) // index of '\n', or len(b)
	content := strings.TrimSpace(string(b[kwLen:lineEnd]))
	if content == "" {
		content = ";"
	}
	next = lineEnd
	if next < len(b) && b[next] == '\n' {
		next++ // drop the newline so the directive line is not rescanned
	}
	return []byte(content), next
}
