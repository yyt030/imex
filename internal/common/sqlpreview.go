package common

import (
	"fmt"
	"strings"
)

// MaxSQLErrorPreview caps how much of a failing statement we echo into error
// messages, so a huge multi-row INSERT cannot flood the terminal or log.
const MaxSQLErrorPreview = 2048

// MaxSQLInteractionPreview caps the statement text a default-level
// `db interaction` record carries. It is deliberately smaller than the error
// preview: an error echoes one statement per run and the reader needs all of it,
// while the trace echoes one record per statement and a parallel export embeds
// the caller's entire query in every shard SELECT. Measured on a 3,522-byte
// `SELECT ... WHERE id IN (1..899)` with --threads 32 (64 shard ranges) against
// a 36,550-byte product, the uncapped trace wrote 165,731 bytes of log, its 200
// interaction records alone being 153,585 bytes with the longest at 2,138 bytes.
// Under this cap the same command writes 67,497–67,777 bytes across repeat runs
// (the shard boundary literals shift the length): the same 200 records are
// 55,348–55,539 bytes and the longest is 603–604 bytes (512 of statement plus
// the truncation marker and the args tag comment), so no single record can
// carry away the log; the remainder is pre-existing fixed lines such as the
// 3,991-byte `export config` snapshot.
const MaxSQLInteractionPreview = 512

// EchoSQLValues, when true, makes failing-SQL diagnostics inline the actual
// row values instead of redacted type/length tags. It is off by default
// because real data (passwords, personal or business content) would then be
// written to stderr, logs, and --messages files. Enabled via --echo-sql-values
// or IMEX_ECHO_SQL_VALUES=1.
var EchoSQLValues bool

// FailingSQL renders a failing statement for an error message, respecting the
// EchoSQLValues setting. Default output is redacted (see SQLPreview).
func FailingSQL(query string, args []interface{}) string {
	if EchoSQLValues {
		return SQLPreviewWithValues(query, args)
	}
	return SQLPreview(query, args)
}

// sqlPreviewArgCap limits how many args the redacted preview summarizes to
// bound the appended comment size for wide batches.
const sqlPreviewArgCap = 32

// SQLPreview renders a failing statement for diagnostics WITHOUT exposing
// row values. The statement text (table/column identifiers and the `?`
// placeholders) is echoed verbatim; each placeholder is annotated in a
// trailing `/* args: ... */` comment with only its Go-derived type tag and,
// for strings/bytes, byte length. This is enough to spot "column 3 came
// back NULL / string too long / unexpected bytes" without leaking
// passwords, personal data, or business content into logs, stderr, and
// message files.
//
// Use SQLPreviewWithValues to get an inlined value view — that path leaks
// real data and must remain opt-in.
func SQLPreview(query string, args []interface{}) string {
	return sqlPreview(query, args, MaxSQLErrorPreview)
}

// SQLInteractionPreview renders a statement for the `db interaction` trace: same
// redaction rule as SQLPreview (identifiers verbatim, bound values as type/length
// tags), same trailing `/* args: ... */` comment, but clipped to
// MaxSQLInteractionPreview instead of the error cap.
func SQLInteractionPreview(query string, args []interface{}) string {
	return sqlPreview(query, args, MaxSQLInteractionPreview)
}

// sqlPreview is the single renderer behind both preview sizes, so the redaction
// rule cannot drift between the trace and the failing-SQL echo.
func sqlPreview(query string, args []interface{}, limit int) string {
	truncated := truncateSQLTo(query, limit)
	if len(args) == 0 {
		return truncated
	}
	parts := make([]string, 0, sqlPreviewArgCap+1)
	limitArgs := len(args)
	if limitArgs > sqlPreviewArgCap {
		limitArgs = sqlPreviewArgCap
	}
	for i := 0; i < limitArgs; i++ {
		parts = append(parts, argTypeTag(args[i]))
	}
	comment := " /* args: " + strings.Join(parts, ", ")
	if len(args) > limitArgs {
		comment += fmt.Sprintf(", +%d more", len(args)-limitArgs)
	}
	comment += " */"
	return truncateSQLTo(truncated+comment, limit)
}

// SQLPreviewWithValues substitutes as many `?` placeholders as it can with
// rendered literals. The result is meant for human debugging ONLY — never
// execute it, and never write it to a persistent sink unless the user
// explicitly opted in (--echo-sql-values / IMEX_ECHO_SQL_VALUES=1).
func SQLPreviewWithValues(query string, args []interface{}) string {
	var sb strings.Builder
	sb.Grow(len(query) + 16*len(args))

	argIdx := 0
	for i := 0; i < len(query); i++ {
		c := query[i]
		if c == '?' && argIdx < len(args) {
			sb.WriteString(renderArg(args[argIdx]))
			argIdx++
			continue
		}
		sb.WriteByte(c)
	}
	return truncateSQL(sb.String())
}

// argTypeTag classifies an arg WITHOUT exposing its content. String and
// byte values reveal length only, since "value too long for column" is a
// common failure and byte-length hints resolve it without disclosure.
func argTypeTag(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case string:
		return fmt.Sprintf("str(%d)", len(x))
	case []byte:
		return fmt.Sprintf("bytes(%d)", len(x))
	case bool:
		return "bool"
	case int, int64, int32, int16, int8, uint, uint64, uint32, uint16, uint8:
		return "num"
	case float32, float64:
		return "float"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// truncateSQL clips a preview string to MaxSQLErrorPreview at a whitespace
// boundary while preserving single-quote parity, so a truncated preview
// never leaves a dangling half-open literal that would confuse a reader
// parsing the SQL.
func truncateSQL(s string) string {
	return truncateSQLTo(s, MaxSQLErrorPreview)
}

// truncateSQLTo applies the same clipping rule at a caller-chosen byte limit.
// Both preview sizes go through here so they cut on the same boundary and append
// the same marker.
func truncateSQLTo(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := s[:limit]
	if last := strings.LastIndexByte(cut, ' '); last > 0 {
		cut = cut[:last]
	}
	for strings.Count(cut, "'")%2 == 1 {
		last := strings.LastIndexByte(cut, '\'')
		if last < 0 {
			break
		}
		cut = cut[:last]
	}
	cut = strings.TrimRight(cut, " \t,")
	return cut + " /* ... truncated */"
}

func renderArg(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	case []byte:
		return fmt.Sprintf("0x%X", x)
	case bool:
		if x {
			return "1"
		}
		return "0"
	case int:
		return fmt.Sprintf("%d", x)
	case int64:
		return fmt.Sprintf("%d", x)
	case float64:
		return fmt.Sprintf("%v", x)
	default:
		return fmt.Sprintf("%v", x)
	}
}
