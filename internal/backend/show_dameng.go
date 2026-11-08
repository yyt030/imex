package backend

import "strings"

// show_dameng.go is the Dameng half of ShowDefinition.
//
// Dameng runs in Oracle-compatible mode, and that mode ships the Oracle metadata
// package DBMS_METADATA, whose GET_DDL(object_type, name, schema) reconstructs a
// complete CREATE statement — byte-identical for tables, and a faithful
// round-trippable definition for the other kinds. Every kind routes through it,
// so the six statements are not hand-written per object the way MySQL's SHOW
// CREATE table is; one call shape covers table/view/trigger/procedure/function.
//
// Three properties were measured against a live DM8 instance and drive the code:
//
//   - The name and schema arguments are CASE-SENSITIVE: a lowercase 't_emp'
//     fails with object-not-found even though the object exists. Oracle-mode
//     DM folds unquoted identifiers to upper case, exactly the way every other
//     catalog query in this backend reads UPPER(OWNER)/UPPER(...), so the
//     arguments are upper-cased before they go into the literal.
//   - Terminator text is per kind: GET_DDL('TABLE',...) already ends in its own
//     `;` (and a trailing space), and the PL/SQL kinds end in the body-closing
//     `END;`, so those report hasTerminator and show must not add a second one.
//     GET_DDL('VIEW',...) returns the SELECT body with NO terminator, so the
//     view reports false and show appends the `;`.
//   - GET_DDL returns a CLOB; the Dameng Go driver surfaces it as a text column
//     named DDL, which the one-row-one-cell shape the show package already runs
//     definitions through reads directly.
//
// event is refused: DM8 Oracle mode has no MySQL-style event scheduler, so there
// is no object of that kind to print (see refusals_dameng.go).

// damengShowColumn is the result column a GET_DDL call names its output as.
const damengShowColumn = "DDL"

// damengMetadataKinds maps the show object word to the DBMS_METADATA object-type
// string, and records whether GET_DDL's text for that kind already ends in its
// own terminator.
var damengMetadataKinds = map[string]struct {
	Type          string
	HasTerminator bool
}{
	"table":     {"TABLE", true},
	"view":      {"VIEW", false},
	"trigger":   {"TRIGGER", true},
	"procedure": {"PROCEDURE", true},
	"function":  {"FUNCTION", true},
}

// ShowDefinition prints one Dameng object through DBMS_METADATA.GET_DDL.
//
// The object reference is passed as SQL string literals (the function takes text,
// not identifiers), so a single quote in a name is doubled to keep a crafted name
// inert. The name and schema are upper-cased to match the Oracle-mode folded
// storage the object actually lives under. An empty schema uses the two-argument
// form, which resolves against the session's current schema — the connection's
// own database, the same rule MySQL's unqualified SHOW CREATE follows.
func (damengBackend) ShowDefinition(object, schema, name string) (string, string, bool, bool) {
	k, ok := damengMetadataKinds[object]
	if !ok {
		return "", "", false, false
	}
	args := "'" + k.Type + "'," + dmShowLit(strings.ToUpper(name))
	if schema != "" {
		args += "," + dmShowLit(strings.ToUpper(schema))
	}
	return "SELECT DBMS_METADATA.GET_DDL(" + args + ") AS " + damengShowColumn + " FROM DUAL",
		damengShowColumn, k.HasTerminator, true
}

// dmShowLit renders s as a Dameng single-quoted string literal, doubling any
// single quote so a crafted object name cannot break out of the literal.
func dmShowLit(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
