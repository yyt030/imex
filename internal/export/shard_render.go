package export

import (
	"strings"

	"github.com/pkg/errors"

	"github.com/imex/internal/backend"
)

// The shard query is not sent to the server as written: --query is parsed with
// the MySQL parser (vitess), the shard condition is injected into the AST, and
// the statement is serialized back to text. That text is MySQL-flavoured in two
// ways the other engines reject as syntax errors:
//
//   - an identifier that is a MySQL keyword comes back in backticks, which
//     PostgreSQL and Dameng do not use as quoting at all;
//   - a string literal keeps MySQL's backslash escapes ('O\'Brien'), and neither
//     engine reads a backslash as an escape marker.
//
// Both are reachable on a real server, and both were measured before this file
// existed (DM8, 2026-11-14): a sharded export of `SELECT ID, NAME FROM t` died on
// every shard with -2007 "nearby [`] has error", and a VARCHAR shard key holding
// O'Brien boundaries died with -2007 "nearby [Brien_7302] has error" — the quote
// closed the literal early. The single-threaded path never sees either form,
// because it sends the user's text untouched, so a run only fails once --threads
// goes above 1.
//
// translateShardQuery rewrites the rendered text into the grammar the target
// reads, with one rule: say what the user's query said. Identifiers are emitted
// bare (so the server's own case folding applies, exactly as it does for the same
// query at --threads 1) unless the target refuses that word bare or the name
// cannot be written bare, in which case the engine's quoting is used. Literals
// are decoded out of MySQL's escape grammar and re-emitted with the doubled
// quote every SQL server accepts.
//
// The MySQL family is deliberately not translated: what BuildShardedQuery emits
// is what a MySQL-targeted parallel export has always sent.

// shardReservedIdents answers "does this engine reject this word as a bare
// identifier?". Only words the MySQL parser can hand back quoted are listed,
// because those are the only ones this code ever sees quoted. Both sets were
// measured against a live server, in both positions an identifier is rendered in
// (select list and WHERE), by sending `SELECT <word> FROM <probe table>` /
// `... WHERE <word> > 1` and reading the error back: a name the grammar accepts
// is reported as an unknown column, anything else means the word is reserved (or
// resolves to something other than the column, like Dameng's LEVEL pseudo-column
// and PostgreSQL's USER function — quoting is what makes them a column again).
//
// pgShardReservedIdents: PostgreSQL 14, 77+23 reserved words in pg_get_keywords()
// of which these 15 also sit in the MySQL keyword table.
var pgShardReservedIdents = map[string]bool{
	"any": true, "array": true, "collation": true, "do": true, "end": true,
	"fetch": true, "full": true, "grant": true, "offset": true, "only": true,
	"references": true, "returning": true, "some": true, "tablesample": true,
	"user": true,
}

// dmShardReservedIdents: Dameng DM8 (Oracle mode), 53 of the MySQL keywords.
var dmShardReservedIdents = map[string]bool{
	"any": true, "array": true, "auto_increment": true, "begin": true,
	"byte": true, "char": true, "coalesce": true, "collation": true,
	"comment": true, "commit": true, "current": true, "cursor": true,
	"decimal": true, "double": true, "end": true, "exchange": true,
	"execute": true, "fetch": true, "first": true, "float": true,
	"function": true, "grant": true, "grouping": true, "int": true,
	"json_array": true, "json_arrayagg": true, "json_object": true,
	"last_insert_id": true, "less": true, "level": true, "list": true,
	"optimize": true, "privileges": true, "procedure": true, "references": true,
	"repeat": true, "return": true, "returning": true, "revoke": true,
	"rollback": true, "rollup": true, "some": true, "subpartition": true,
	"than": true, "timestampadd": true, "timestampdiff": true, "trigger": true,
	"trim": true, "truncate": true, "unicode": true, "user": true, "view": true,
	"without": true,
}

// shardIdentQuoter returns the reserved table and the quoting function a target
// needs, or nil when the target is the MySQL family and nothing is translated.
func shardIdentQuoter(be backend.Backend) (map[string]bool, func(string) string) {
	if be == nil {
		return nil, nil
	}
	switch be.Engine() {
	case backend.EnginePG, backend.EngineGauss:
		return pgShardReservedIdents, be.QuoteIdent
	case backend.EngineDM:
		return dmShardReservedIdents, be.QuoteIdent
	default:
		return nil, nil
	}
}

// translateShardQuery rewrites MySQL-quoted identifiers and MySQL-escaped string
// literals for the engine that has to run the statement. reserved == nil means
// the target speaks MySQL and the text is returned unchanged.
func translateShardQuery(text string, reserved map[string]bool, quoteIdent func(string) string) (string, error) {
	if reserved == nil {
		return text, nil
	}

	out := make([]byte, 0, len(text)+16)
	for i := 0; i < len(text); {
		switch text[i] {
		case '`':
			name, next, err := readBacktickedIdent(text, i)
			if err != nil {
				return "", err
			}
			if !isBareIdent(name) || reserved[strings.ToLower(name)] {
				out = append(out, quoteIdent(name)...)
			} else {
				out = append(out, name...)
			}
			i = next
		case '\'':
			value, next, err := readMySQLStringLiteral(text, i)
			if err != nil {
				return "", err
			}
			lit, err := renderStandardLiteral(value)
			if err != nil {
				return "", err
			}
			out = append(out, lit...)
			i = next
		default:
			out = append(out, text[i])
			i++
		}
	}
	return string(out), nil
}

// readBacktickedIdent consumes one backtick-quoted identifier, where a doubled
// backtick inside is an escaped backtick, and returns the bare name.
func readBacktickedIdent(text string, start int) (string, int, error) {
	var name []byte
	for i := start + 1; i < len(text); {
		if text[i] == '`' {
			if i+1 < len(text) && text[i+1] == '`' {
				name = append(name, '`')
				i += 2
				continue
			}
			return string(name), i + 1, nil
		}
		name = append(name, text[i])
		i++
	}
	return "", 0, errors.Errorf("unterminated quoted identifier in the shard query at offset %d", start)
}

// readMySQLStringLiteral decodes one single-quoted MySQL literal into the bytes
// it stands for: backslash escapes and doubled quotes both mean one character.
func readMySQLStringLiteral(text string, start int) (string, int, error) {
	var value []byte
	for i := start + 1; i < len(text); {
		switch text[i] {
		case '\\':
			if i+1 >= len(text) {
				return "", 0, errors.Errorf("shard query literal ends on a backslash at offset %d", start)
			}
			i++
			switch c := text[i]; c {
			case '0':
				value = append(value, 0x00)
			case 'b':
				value = append(value, 0x08)
			case 'n':
				value = append(value, '\n')
			case 'r':
				value = append(value, '\r')
			case 't':
				value = append(value, '\t')
			case 'Z':
				value = append(value, 0x1A)
			case '\\', '\'', '"':
				value = append(value, c)
			default:
				// MySQL ignores the backslash in front of any other character,
				// including the LIKE escapes \% and \_ — those two have to keep
				// it, or a LIKE boundary stops meaning what it said.
				if c != '%' && c != '_' {
					value = append(value, c)
				} else {
					value = append(value, '\\', c)
				}
			}
			i++
		case '\'':
			if i+1 < len(text) && text[i+1] == '\'' {
				value = append(value, '\'')
				i += 2
				continue
			}
			return string(value), i + 1, nil
		default:
			value = append(value, text[i])
			i++
		}
	}
	return "", 0, errors.Errorf("unterminated string literal in the shard query at offset %d", start)
}

// renderStandardLiteral writes decoded bytes back as a plain SQL literal: the
// only escape a standard-conforming server needs is a doubled quote.
func renderStandardLiteral(value string) (string, error) {
	if strings.IndexByte(value, 0x00) != -1 {
		return "", errors.Errorf("the shard query carries a NUL byte inside a string literal, which this engine cannot receive: put the value in a WHERE clause the export runs single-threaded")
	}
	return "'" + strings.ReplaceAll(value, "'", "''") + "'", nil
}

// isBareIdent reports whether the name can reach the server unquoted: a leading
// letter, then letters, digits, _ and $. Anything else (a space, a dot, a leading
// digit, a non-ASCII letter the parser could only accept quoted) has to carry the
// engine's quoting, which also preserves its case.
func isBareIdent(name string) bool {
	if name == "" {
		return false
	}
	c := name[0]
	if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if c == '_' || c == '$' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			continue
		}
		return false
	}
	return true
}
