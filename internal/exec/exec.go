// Package exec runs arbitrary SQL statements against a target engine, one
// statement at a time, and reports what each did. It is the execution counterpart
// to `show`: where `show` prints the CREATE statement the server holds, `exec`
// takes that text back and replays it, so a schema captured with `show` (or any
// script following the terminator convention) can be rebuilt on another server
// without an import data path getting in the way.
//
// The split into statements is this package's tokenizer (tokenizer.go), and it is
// deliberately the whole of its SQL knowledge: exec does not parse statements, it
// hands each one verbatim to the server and reports the server's own answer. That
// keeps contract face ⑤ (the SQL the server sees) exactly the bytes the operator
// wrote, apart from the leading attribution comment the rest of the tool adds.
package exec

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"

	"github.com/pkg/errors"

	"github.com/imex/internal/backend"
	"github.com/imex/internal/common"
	"github.com/imex/internal/list"
)

// Stats summarises one exec run. Statements counts the statements actually sent
// (comment-only regions are not counted because they are never sent). Rows counts
// rows the result sets carried; Affected sums the rows the writing statements
// reported. A DDL statement contributes 0 to both, which is what the server says.
type Stats struct {
	Statements int
	Rows       int64
	Affected   int64
}

// Run splits script into statements and executes them in order against db on the
// engine e, writing each statement's result to out and returning the aggregate.
// The first failing statement stops the run: a script is an ordered set of
// instructions, and continuing past one the server rejected would build the rest
// on a schema the operator did not ask for. header selects whether a result set
// prints its column-name row.
func Run(ctx context.Context, db *sql.DB, e backend.Engine, script string, header bool, out io.Writer) (Stats, error) {
	stmts := SplitStatements(script, e)
	fl := tokenizerFlagsFor(e)

	var st Stats
	for i, stmt := range stmts {
		common.LogInteractionSQL(stmt)
		if returnsRows(stmt, fl, e) {
			rows, err := runQuery(ctx, db, stmt, header, out)
			if err != nil {
				return st, statementErr(i, stmt, err)
			}
			st.Rows += rows
		} else {
			affected, lastID, err := runExec(ctx, db, stmt)
			if err != nil {
				return st, statementErr(i, stmt, err)
			}
			st.Affected += affected
			if err := writeExecResult(out, affected, lastID); err != nil {
				return st, errors.Wrap(err, "failed to write exec result")
			}
		}
		st.Statements++
	}
	return st, nil
}

// runQuery executes a row-returning statement and renders its result set through
// the same aligned table `imex list` writes, so the two commands agree on layout.
// It returns the number of data rows rendered.
func runQuery(ctx context.Context, db *sql.DB, stmt string, header bool, out io.Writer) (int64, error) {
	rows, err := db.QueryContext(ctx, common.TagSQL(stmt))
	if err != nil {
		return 0, errors.Wrap(err, "query failed")
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return 0, errors.Wrap(err, "failed to read result columns")
	}

	rendered := make([][]*string, 0, 64)
	count := int64(0)
	for rows.Next() {
		values := make([]sql.NullString, len(cols))
		dest := make([]any, len(cols))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return count, errors.Wrap(err, "failed to read result row")
		}
		row := make([]*string, len(cols))
		for i := range values {
			if values[i].Valid {
				v := values[i].String
				row[i] = &v
			}
		}
		rendered = append(rendered, row)
		count++
	}
	if err := rows.Err(); err != nil {
		return count, errors.Wrap(err, "query failed")
	}
	if err := list.Render(out, cols, header, rendered); err != nil {
		return count, errors.Wrap(err, "failed to write result")
	}
	return count, nil
}

// runExec applies a statement that returns no rows (DDL and plain DML) and reads
// its affected-row count and insert id. Both are best-effort: engines whose
// DDL answers no row count simply report 0, which is the truth.
func runExec(ctx context.Context, db *sql.DB, stmt string) (affected int64, lastID int64, err error) {
	res, err := db.ExecContext(ctx, common.TagSQL(stmt))
	if err != nil {
		return 0, 0, errors.Wrap(err, "statement failed")
	}
	if n, err := res.RowsAffected(); err == nil {
		affected = n
	}
	if id, err := res.LastInsertId(); err == nil {
		lastID = id
	}
	return affected, lastID, nil
}

// writeExecResult records one non-row statement's outcome. A plain DDL statement
// (no rows, no id) reads as "OK"; a write names its affected count, and an insert
// adds the generated id when the engine supplied one.
func writeExecResult(out io.Writer, affected, lastID int64) error {
	line := "OK"
	if affected > 0 {
		line = fmt.Sprintf("%d rows affected", affected)
		if lastID > 0 {
			line += fmt.Sprintf(", last_insert_id %d", lastID)
		}
	}
	_, err := fmt.Fprintf(out, "%s\n", line)
	return err
}

// statementErr names which statement failed and quotes a short, single-line
// preview of it, so the operator can find the boundary without the tool echoing a
// whole file. The server's own error is kept underneath.
func statementErr(i int, stmt string, err error) error {
	preview := firstLine(stmt)
	if len(preview) > 200 {
		preview = preview[:200] + "…"
	}
	return errors.Wrapf(err, "statement %d failed: %s", i+1, preview)
}

// firstLine flattens a statement's leading whitespace/newlines to one line for
// the error preview.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		return strings.TrimSpace(s[:nl])
	}
	return s
}

// returnsRows decides whether a statement is expected to carry a result set, by
// its first keyword plus a RETURNING clause on the engines that spell one. The
// heuristic is not a parser: a statement whose first word is one of these is read
// with QueryContext, everything else with ExecContext. It is the same first-word
// rule the interactive clients use to choose how to print, and a wrong guess only
// changes which report line the operator sees, never what the server executes.
func returnsRows(stmt string, fl tokenizerFlags, e backend.Engine) bool {
	word := firstWord(stmt, fl)
	switch word {
	case "select", "with", "show", "explain", "describe", "desc", "values", "table", "call", "pragma", "query":
		return true
	case "insert", "update", "delete", "merge":
		// PostgreSQL and Dameng can answer a DML with RETURNING, which is a result
		// set; MySQL has no such clause, so only the row-returning engines count.
		if e == backend.EnginePG || e == backend.EngineDM {
			return hasWord(stmt, fl, "returning")
		}
	}
	return false
}

// firstWord returns the leading alphanumeric word of a statement with its comments
// and whitespace skipped, lower-cased.
func firstWord(stmt string, fl tokenizerFlags) string {
	i, n := 0, len(stmt)
	for i < n {
		c := stmt[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			i++
			continue
		}
		if c == '-' && i+1 < n && stmt[i+1] == '-' {
			i = endOfLineStr(stmt, i)
			continue
		}
		if c == '#' && fl.hashComment {
			i = endOfLineStr(stmt, i)
			continue
		}
		if c == '/' && i+1 < n && stmt[i+1] == '*' {
			i = endBlockCommentStr(stmt, i)
			continue
		}
		// A leading quote/identifier is not a keyword word; return empty so the
		// caller treats it as non-row-returning.
		if !(isWordByte(c)) {
			return ""
		}
		j := i
		for j < n && isWordByte(stmt[j]) {
			j++
		}
		return strings.ToLower(stmt[i:j])
	}
	return ""
}

// hasWord reports whether word appears as a whole alphanumeric word outside
// comments and quoted literals of the statement.
func hasWord(stmt string, fl tokenizerFlags, word string) bool {
	i, n := 0, len(stmt)
	for i < n {
		switch c := stmt[i]; {
		case c == '-' && i+1 < n && stmt[i+1] == '-':
			i = endOfLineStr(stmt, i)
		case c == '#' && fl.hashComment:
			i = endOfLineStr(stmt, i)
		case c == '/' && i+1 < n && stmt[i+1] == '*':
			i = endBlockCommentStr(stmt, i)
		case c == '\'' || c == '"' || (c == '`' && fl.backtick):
			i = skipQuoted([]byte(stmt), i+1, c, fl)
		case c == '$' && fl.dollarQuote:
			if end, ok := skipDollarQuoted([]byte(stmt), i); ok {
				i = end
				continue
			}
			i++
		default:
			if !isWordByte(c) {
				i++
				continue
			}
			j := i
			for j < n && isWordByte(stmt[j]) {
				j++
			}
			if strings.EqualFold(stmt[i:j], word) {
				return true
			}
			i = j
		}
	}
	return false
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func endOfLineStr(s string, i int) int {
	for i < len(s) && s[i] != '\n' {
		i++
	}
	return i
}

func endBlockCommentStr(s string, i int) int {
	i += 2
	for i+1 < len(s) {
		if s[i] == '*' && s[i+1] == '/' {
			return i + 2
		}
		i++
	}
	return len(s)
}
