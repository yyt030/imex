// Package show prints the definition text the server holds for ONE schema
// object: SHOW CREATE TABLE, VIEW, TRIGGER, PROCEDURE, FUNCTION or EVENT.
//
// The scope rule is the one the user set: `show` looks at a single object,
// `imex list` produces the batches. Enumeration therefore stays in `list`, and
// so does everything a batch needs — ordering, dependency handling, idempotency
// are the replay side's job, not this command's.
//
// The text is copied verbatim. information_schema is deliberately not used even
// though it looks cheaper: TRIGGERS.ACTION_STATEMENT holds only the body without
// the trigger header, ROUTINES.ROUTINE_DEFINITION loses the parameter list and
// RETURNS clause, and VIEWS.VIEW_DEFINITION is the parsed normalisation rather
// than what was typed. The only bytes this package adds are one terminating
// semicolon and one newline, folded to exactly one from whatever the server
// returned, because `imex import -f sql` reads statements that end with one.
package show

import (
	"context"
	"database/sql"
	"io"
	"strings"

	"github.com/pkg/errors"

	"github.com/imex/internal/backend"
	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

// Kind is one object type `show` can print.
//
// It carries the CLI vocabulary only: the SHOW statement and the result column
// that carries the definition are the engine's (Backend.ShowDefinition), because
// they are server-visible SQL and a dialect that has no such statement has to be
// able to say so. Keeping them here made this package the second place a new
// engine had to be taught, and `show` the one command the dialect layer could not
// answer for.
type Kind struct {
	// Short is the three-letter name the CLI registers as the subcommand, and the
	// name help text and error messages use.
	Short string

	// Full is the English word kept as an alias, so `show table` and `show tab`
	// parse to the same command exactly like `list tables` and `list tab`. It is
	// also the key the dialect table is looked up by.
	Full string
}

// kindOrder is the registration order, and the order the parent command lists in
// its help and in its "unknown kind" message.
var kindOrder = []Kind{
	{Short: "tab", Full: "table"},
	{Short: "vws", Full: "view"},
	{Short: "trg", Full: "trigger"},
	{Short: "evt", Full: "event"},
	{Short: "prc", Full: "procedure"},
	{Short: "fcn", Full: "function"},
}

// Kinds returns the accepted kinds in help order. The slice is a copy, so a
// caller cannot change the registry through Cobra.
func Kinds() []Kind {
	out := make([]Kind, len(kindOrder))
	copy(out, kindOrder)
	return out
}

// Lookup resolves a kind by its three-letter name or its full English alias.
func Lookup(name string) (Kind, bool) {
	for _, k := range kindOrder {
		if name == k.Short || name == k.Full {
			return k, true
		}
	}
	return Kind{}, false
}

// ShortNames returns the three-letter names in help order, for the parent
// command's usage message.
func ShortNames() []string {
	out := make([]string, len(kindOrder))
	for i, k := range kindOrder {
		out[i] = k.Short
	}
	return out
}

// SplitName separates an optional schema qualifier from the object name.
//
// A backquoted first part decides on its own, so a quoted schema keeps the dots
// inside it (`we.ir`.`obj` splits at the second dot only) and `a.b` alone stays a
// single unqualified name. Two adjacent backquotes inside a quoted name are the
// escape for one literal backquote, so the scanner must consume them as a pair;
// reading the first backquote with a plain index() would split the name in the
// middle. Without a leading backquote the FIRST dot splits, because an unquoted
// MySQL name cannot contain one. Anything the grammar cannot read — unterminated
// backquotes, or text after a closed quoted name — is handed back whole as the
// object, so the server rejects the unknown object instead of the tool guessing
// a split point.
func SplitName(raw string) (schema, object string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	if strings.HasPrefix(raw, "`") {
		first, rest, ok := scanQuoted(raw)
		switch {
		case !ok, rest != "" && !strings.HasPrefix(rest, "."):
			return "", raw
		case rest == "":
			return "", first
		default:
			return first, unquoteIdent(strings.TrimPrefix(rest, "."))
		}
	}
	if i := strings.IndexByte(raw, '.'); i >= 0 {
		return unquoteIdent(raw[:i]), unquoteIdent(raw[i+1:])
	}
	return "", unquoteIdent(raw)
}

// scanQuoted reads one backquoted identifier from the front of s and returns its
// unescaped text plus everything after the closing backquote. ok is false when the
// span never closes.
func scanQuoted(s string) (name, rest string, ok bool) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '`' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '`' {
			b.WriteByte('`')
			i++
			continue
		}
		return b.String(), s[i+1:], true
	}
	return "", "", false
}

// unquoteIdent removes a pair of surrounding backquotes and folds the doubled
// backquotes an escaped name carries.
func unquoteIdent(s string) string {
	if len(s) >= 2 && strings.HasPrefix(s, "`") && strings.HasSuffix(s, "`") {
		s = s[1 : len(s)-1]
	}
	return strings.ReplaceAll(s, "``", "`")
}

// qualify renders the object reference the SHOW statement targets. An empty
// schema leaves the name unqualified, which makes the server resolve it in the
// connection's own database. The quoting is the dialect's (Backend.QuoteIdent),
// so an engine that spells identifiers another way gets its own spelling instead
// of this package's backquotes.
func qualify(be backend.Backend, schema, object string) string {
	name := be.QuoteIdent(object)
	if schema != "" {
		name = be.QuoteIdent(schema) + "." + name
	}
	return name
}

// Run prints the definition of the single object cfg names to out. It returns
// the number of bytes written.
//
// Like `list`, the statement is recorded through common.LogInteractionSQL before
// it goes out, so a default-level log shows what was asked of the server.
func Run(ctx context.Context, db *sql.DB, be backend.Backend, kind Kind, cfg *config.ShowConfig, out io.Writer) (int, error) {
	query, column, hasTerminator, ok := be.ShowDefinition(kind.Full, cfg.Schema, cfg.Object)
	if !ok {
		// Unreachable through the CLI: cmd asks Refusals(CmdShow) before it dials,
		// so an engine with no definition statement never gets here. The branch is
		// for a library caller and for a future dialect whose table is still empty:
		// a refusal, not a statement with no verb.
		return 0, errors.Errorf("show %s is not supported by engine %s", kind.Full, be.Engine())
	}
	common.LogInteractionSQL(query)

	rows, err := db.QueryContext(ctx, common.TagSQL(query))
	if err != nil {
		return 0, errors.Wrap(err, "definition query failed")
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return 0, errors.Wrap(err, "failed to read definition columns")
	}
	if len(columns) < 1 {
		return 0, errors.Errorf("%s returned no definition column", query)
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, errors.Wrap(err, "definition query failed")
		}
		return 0, errors.Errorf("no %s found: %s", kind.Full, qualify(be, cfg.Schema, cfg.Object))
	}

	values := make([]sql.NullString, len(columns))
	dest := make([]any, len(columns))
	for i := range values {
		dest[i] = &values[i]
	}
	if err := rows.Scan(dest...); err != nil {
		return 0, errors.Wrap(err, "failed to read definition row")
	}

	definition, ok := pickDefinition(columns, values, column)
	if !ok {
		return 0, errors.Errorf("%s returned no definition column (columns: %s)", query, strings.Join(columns, ", "))
	}

	// The server's text is copied verbatim; the only bytes this package adds are
	// one terminating semicolon and one newline, because `imex import -f sql`
	// reads statements ending with `;`. Whether the server already supplied that
	// semicolon is decided from the text itself rather than solely from the
	// dialect's hasTerminator flag: DM8 single's GET_DDL('VIEW',...) was measured
	// to end in its own `;` while the Dameng table listed view as having none,
	// which printed `;;` and made the DDL fail to replay. See normalizeDefinition.
	n, err := out.Write([]byte(normalizeDefinition(definition, hasTerminator)))
	if err != nil {
		return n, errors.Wrap(err, "failed to write definition")
	}
	if err := rows.Err(); err != nil {
		return n, errors.Wrap(err, "definition query failed")
	}
	return n, nil
}

// normalizeDefinition returns the exact bytes show writes for one definition:
// the server's text with exactly one terminating semicolon and one newline.
//
// hasTerminator is part of Backend.ShowDefinition's dialect contract (face ⑤)
// and stays in the signature so a reader can see the question is asked, but the
// bytes the server actually returned are the authority. A per-kind flag is
// measured against one server build and can be wrong on another: DM8 single
// returns `CREATE OR REPLACE VIEW ...;` for GET_DDL('VIEW',...), and a different
// DM8 build may just as well omit it, while Gauss's pg_get_tabledef always ends
// in `;` and MySQL's SHOW CREATE never does. Folding any trailing semicolons to
// exactly one is version-proof and keeps every engine's output replayable.
//
// Internal semicolons are untouched — a PL/SQL body ends in `END;` and a
// multi-statement definition is not this command's business — because only the
// trailing span is trimmed.
func normalizeDefinition(definition string, hasTerminator bool) string {
	trimmed := strings.TrimRight(definition, " \t\r\n")
	trimmed = strings.TrimRight(trimmed, ";")
	trimmed = strings.TrimRight(trimmed, " \t\r\n")
	return trimmed + ";\n"
}

// pickDefinition finds the definition text among one result row.
//
// The column is chosen by name, because its position differs across the six
// statements; the fallback takes the first column whose value begins with CREATE
// (case-insensitive), which every server-side definition text does. Without the
// name being registered the fallback still prints the right bytes rather than a
// session attribute, and a server that renames the column cannot silently change
// what the tool writes.
func pickDefinition(columns []string, values []sql.NullString, want string) (string, bool) {
	for i, name := range columns {
		if values[i].Valid && strings.EqualFold(name, want) {
			return values[i].String, true
		}
	}
	for i := range columns {
		if values[i].Valid && strings.HasPrefix(strings.ToUpper(values[i].String), "CREATE") {
			return values[i].String, true
		}
	}
	return "", false
}
