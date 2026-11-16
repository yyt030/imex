package ddl

import (
	"strings"

	"github.com/pkg/errors"
)

// view.go rebuilds one Dameng view's CREATE statement in MySQL grammar.
//
// Unlike a table, a view has no typed catalogue shape to rebuild from: the only
// authority is the text DBMS_METADATA.GET_DDL('VIEW', ...) returns. That text is
// Oracle grammar, so this file does a *controlled* rewrite rather than a parse
// and re-render: it splits the CREATE shell from the SELECT body, translates the
// shell and every double-quoted identifier, and copies the SELECT body through
// a lexical scanner that refuses any Oracle-only construct it cannot restate.
//
// The refusal rule is the same one the table path lives by (see the package
// comment): an Oracle fact this build cannot render is a REFUSAL, never a gap.
// A view whose SELECT uses ROWNUM, CONNECT BY, DECODE, (+), || or an Oracle
// package function is refused by name — printing it unchanged would hand MySQL
// a statement that either fails to parse or, worse (||), parses with a different
// meaning.

// View is one view's definition split into the parts the MySQL printer needs.
type View struct {
	Schema string // owning schema ("" when the reference was unqualified)
	Name   string
	// Body is the SELECT text between AS and the optional WITH CHECK OPTION,
	// with source (Oracle) spellings still intact; the renderer translates it.
	Body string
	// CheckOption is "" or "CASCADED" / "LOCAL", from WITH ... CHECK OPTION.
	CheckOption string
	// OrReplace records whether the source said OR REPLACE.
	OrReplace bool
}

// parseDamengView splits a GET_DDL('VIEW', ...) text into a View.
//
// It is deliberately a small, strict recogniser of the CREATE VIEW shell, not a
// SQL parser. Anything it cannot place with certainty is a refusal: the shell
// grammar is fixed ("CREATE [OR REPLACE] VIEW name AS select [WITH ...]"), so a
// surprise there means the input is not the view definition this build expects.
func parseDamengView(text string) (*View, error) {
	s := strings.TrimSpace(stripTrailingSemicolon(text))
	upper := strings.ToUpper(s)

	// The shell starts at "CREATE" and the body begins at the top-level "AS".
	// Locate AS with a scanner so an "AS" inside a quoted alias or string does
	// not end the shell early.
	if !strings.HasPrefix(upper, "CREATE ") {
		return nil, errors.New("view definition does not start with CREATE")
	}
	rest := strings.TrimSpace(s[len("CREATE "):])
	upperRest := strings.ToUpper(rest)

	v := &View{}
	if strings.HasPrefix(upperRest, "OR REPLACE ") {
		v.OrReplace = true
		rest = strings.TrimSpace(rest[len("OR REPLACE "):])
		upperRest = strings.ToUpper(rest)
	}
	if !strings.HasPrefix(upperRest, "VIEW ") {
		return nil, errors.New("view definition is not a CREATE VIEW statement")
	}
	rest = strings.TrimSpace(rest[len("VIEW "):])

	// The view name runs to the top-level AS keyword.
	nameEnd := topLevelKeyword(rest, "AS")
	if nameEnd < 0 {
		return nil, errors.New("view definition has no AS before its SELECT body")
	}
	namePart := strings.TrimSpace(rest[:nameEnd])
	body := strings.TrimSpace(rest[nameEnd+2:])

	schema, name, err := splitDamengQualifiedName(namePart)
	if err != nil {
		return nil, err
	}
	v.Schema, v.Name = schema, name

	// Pull a trailing WITH [CASCADED|LOCAL] CHECK OPTION off the body. It is a
	// view property, not part of the SELECT, and MySQL spells it the same way.
	body, check, err := stripCheckOption(body)
	if err != nil {
		return nil, err
	}
	v.CheckOption = check
	v.Body = strings.TrimSpace(body)
	if v.Body == "" {
		return nil, errors.New("view definition has an empty SELECT body")
	}
	return v, nil
}

// stripTrailingSemicolon removes one terminating ';' the server may have added.
func stripTrailingSemicolon(s string) string {
	s = strings.TrimRight(s, " \t\r\n")
	return strings.TrimSuffix(s, ";")
}

// splitDamengQualifiedName reads "SCHEMA"."NAME", SCHEMA.NAME, or NAME.
// Quoted parts keep their inner spelling; unquoted parts are upper-cased the
// way Dameng's Oracle mode stores them.
func splitDamengQualifiedName(part string) (schema, name string, err error) {
	dots := topLevelDots(part)
	switch len(dots) {
	case 0:
		name = decodeDamengIdent(strings.TrimSpace(part))
	case 1:
		schema = decodeDamengIdent(strings.TrimSpace(part[:dots[0]]))
		name = decodeDamengIdent(strings.TrimSpace(part[dots[0]+1:]))
	default:
		return "", "", errors.New("view name has more than one qualifier")
	}
	if name == "" {
		return "", "", errors.New("view name is empty")
	}
	return schema, name, nil
}

// decodeDamengIdent strips double quotes around an identifier and collapses the
// doubled quotes inside. Unquoted text is folded to upper case, matching how
// Dameng stores an unquoted identifier in Oracle mode.
func decodeDamengIdent(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return strings.ToUpper(s)
}

// stripCheckOption removes a trailing WITH [CASCADED|LOCAL] CHECK OPTION and
// returns its kind. Everything else is left untouched.
func stripCheckOption(body string) (rest, check string, err error) {
	upper := strings.ToUpper(body)
	for _, word := range []string{"WITH CASCADED CHECK OPTION", "WITH LOCAL CHECK OPTION", "WITH CHECK OPTION"} {
		if strings.HasSuffix(upper, word) {
			kind := ""
			switch word {
			case "WITH CASCADED CHECK OPTION":
				kind = "CASCADED"
			case "WITH LOCAL CHECK OPTION":
				kind = "LOCAL"
			default:
				kind = "CASCADED" // MySQL's default when no kind is named
			}
			return strings.TrimSpace(body[:len(body)-len(word)]), kind, nil
		}
	}
	return body, "", nil
}

// topLevelKeyword finds the first case-insensitive occurrence of kw that sits
// outside quotes, comments and parentheses, returning its byte offset in s.
func topLevelKeyword(s, kw string) int {
	b := []byte(s)
	depth := 0
	for i := 0; i < len(b); {
		switch {
		case b[i] == '\'':
			i = skipSingleQuoted(b, i)
		case b[i] == '"':
			i = skipDoubleQuoted(b, i)
		case b[i] == '`':
			i = skipBackquoted(b, i)
		case b[i] == '(':
			depth++
			i++
		case b[i] == ')':
			if depth > 0 {
				depth--
			}
			i++
		case depth == 0 && i+len(kw) <= len(b) && strings.EqualFold(string(b[i:i+len(kw)]), kw) &&
			(i == 0 || !isIdentByte(b[i-1])) && (i+len(kw) == len(b) || !isIdentByte(b[i+len(kw)])):
			return i
		default:
			i++
		}
	}
	return -1
}

// topLevelDots returns byte offsets of '.' outside quotes and parentheses.
func topLevelDots(s string) []int {
	b := []byte(s)
	var dots []int
	depth := 0
	for i := 0; i < len(b); {
		switch {
		case b[i] == '"':
			i = skipDoubleQuoted(b, i)
		case b[i] == '`':
			i = skipBackquoted(b, i)
		case b[i] == '(':
			depth++
			i++
		case b[i] == ')':
			if depth > 0 {
				depth--
			}
			i++
		case b[i] == '.' && depth == 0:
			dots = append(dots, i)
			i++
		default:
			i++
		}
	}
	return dots
}

// translateViewBody rewrites the Oracle spelling of a SELECT body into MySQL.
// It returns a refusal naming the first Oracle construct it meets, if any.
func translateViewBody(body string) (string, error) {
	b := []byte(body)
	var out strings.Builder
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == '\'':
			j := skipSingleQuoted(b, i)
			out.Write(b[i:j])
			i = j
		case c == '"':
			// Oracle quoted identifier -> MySQL backquoted identifier.
			j := skipDoubleQuoted(b, i)
			inner := strings.ReplaceAll(string(b[i+1:j-1]), `""`, `"`)
			out.WriteString(mysqlIdent(inner))
			i = j
		case c == '`':
			// Already MySQL-quoted (unusual from DM) — keep as-is.
			j := skipBackquoted(b, i)
			out.Write(b[i:j])
			i = j
		case c == '-' && i+1 < len(b) && b[i+1] == '-':
			j := endOfLine(b, i)
			out.Write(b[i:j])
			i = j
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			j := endBlockComment(b, i)
			out.Write(b[i:j])
			i = j
		case isIdentStart(c):
			j := i + 1
			for j < len(b) && isIdentByte(b[j]) {
				j++
			}
			word := string(b[i:j])
			if repl, ok := oracleWordReplacement[strings.ToUpper(word)]; ok {
				if repl == "" {
					return "", errors.Errorf("the view SELECT uses %q, an Oracle construct with no MySQL equivalent", word)
				}
				out.WriteString(repl)
			} else {
				if refused, ok := oracleRefusal[word]; ok {
					return "", errors.Errorf("the view SELECT uses %q: %s", word, refused)
				}
				out.WriteString(word)
			}
			i = j
		case c == '|' && i+1 < len(b) && b[i+1] == '|':
			return "", errors.New("the view SELECT uses ||, which is string concatenation in Oracle but logical OR in MySQL")
		case c == '(' && i+1 < len(b) && b[i+1] == '+':
			return "", errors.New("the view SELECT uses the Oracle (+) outer-join operator, which MySQL does not have")
		default:
			out.WriteByte(c)
			i++
		}
	}
	return out.String(), nil
}

// oracleWordReplacement maps Oracle function words that have an exact MySQL
// spelling. A value of "" means the word is known but deliberately refused.
// Only NVL is mapped: both are NULL-coalescing with the same 2-arg semantics.
var oracleWordReplacement = map[string]string{
	"NVL": "COALESCE",
}

// oracleRefusal names Oracle-only constructs that have no safe MySQL spelling.
// A bare CONNECT is the reliable signal for CONNECT BY hierarchical queries;
// a bare START is not refused because it is a normal word in other positions.
// DUAL is deliberately absent: MySQL has a DUAL table with the same meaning.
var oracleRefusal = map[string]string{
	"ROWNUM":       "MySQL has no ROWNUM; use LIMIT in the query instead",
	"LEVEL":        "hierarchical queries have no MySQL equivalent",
	"CONNECT":      "CONNECT BY hierarchical queries have no MySQL equivalent",
	"DECODE":       "rewrite DECODE as a CASE expression for MySQL",
	"MINUS":        "MySQL spells set difference EXCEPT (8.0.31+)",
	"NVL2":         "rewrite NVL2 as a CASE expression for MySQL",
	"SYSDATE":      "MySQL has no SYSDATE; NOW() differs in type/precision, so rewrite it explicitly",
	"SYSTIMESTAMP": "MySQL has no SYSTIMESTAMP; NOW(6) differs in type",
}

// RenderMySQLView prints one view in MySQL grammar, or refuses by name.
func RenderMySQLView(v *View) (string, error) {
	body, err := translateViewBody(v.Body)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("CREATE ")
	if v.OrReplace {
		b.WriteString("OR REPLACE ")
	}
	b.WriteString("VIEW ")
	ref := mysqlIdent(v.Name)
	if v.Schema != "" {
		ref = mysqlIdent(v.Schema) + "." + ref
	}
	b.WriteString(ref + " AS ")
	b.WriteString(body)
	if v.CheckOption != "" {
		b.WriteString(" WITH " + v.CheckOption + " CHECK OPTION")
	}
	return b.String(), nil
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isIdentByte(c byte) bool {
	return isIdentStart(c) || c >= '0' && c <= '9'
}

func skipSingleQuoted(b []byte, i int) int {
	i++
	for i < len(b) {
		if b[i] == '\'' {
			if i+1 < len(b) && b[i+1] == '\'' {
				i += 2
				continue
			}
			return i + 1
		}
		i++
	}
	return i
}

func skipDoubleQuoted(b []byte, i int) int {
	i++
	for i < len(b) {
		if b[i] == '"' {
			if i+1 < len(b) && b[i+1] == '"' {
				i += 2
				continue
			}
			return i + 1
		}
		i++
	}
	return i
}

func skipBackquoted(b []byte, i int) int {
	i++
	for i < len(b) {
		if b[i] == '`' {
			if i+1 < len(b) && b[i+1] == '`' {
				i += 2
				continue
			}
			return i + 1
		}
		i++
	}
	return i
}

func endOfLine(b []byte, i int) int {
	for i < len(b) && b[i] != '\n' {
		i++
	}
	return i
}

func endBlockComment(b []byte, i int) int {
	i += 2
	for i+1 < len(b) {
		if b[i] == '*' && b[i+1] == '/' {
			return i + 2
		}
	}
	return len(b)
}
