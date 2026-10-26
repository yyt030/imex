package common

import (
	"fmt"
	"strings"
)

// MaxSQLErrorPreview caps how much of a failing statement we echo into error
// messages, so a huge multi-row INSERT cannot flood the terminal or log.
const MaxSQLErrorPreview = 2048

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
	truncated := truncateSQL(query)
	if len(args) == 0 {
		return truncated
	}
	parts := make([]string, 0, sqlPreviewArgCap+1)
	limit := len(args)
	if limit > sqlPreviewArgCap {
		limit = sqlPreviewArgCap
	}
	for i := 0; i < limit; i++ {
		parts = append(parts, argTypeTag(args[i]))
	}
	comment := " /* args: " + strings.Join(parts, ", ")
	if len(args) > limit {
		comment += fmt.Sprintf(", +%d more", len(args)-limit)
	}
	comment += " */"
	return truncateSQL(truncated + comment)
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
	if len(s) <= MaxSQLErrorPreview {
		return s
	}
	cut := s[:MaxSQLErrorPreview]
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
