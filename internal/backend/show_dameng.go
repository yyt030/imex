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
//     `;` (and a trailing space), the PL/SQL kinds end in the body-closing
//     `END;`, and — measured on DM8 single, dm8_20241022_rev244896 —
//     GET_DDL('VIEW',...) also returns its own `;`. All six kinds therefore
//     report hasTerminator. An earlier build of this table claimed the view
//     text had NO terminator; show then appended a second `;` and the DDL
//     failed to replay with `[-2007] Syntax error`. show now normalises the
//     actual text to exactly one `;` as a second guard, so a DM8 build that
//     omits the terminator is still replayable (see show.normalizeDefinition).
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
// own terminator. On DM8 single every kind does; the map stays per-kind because
// the property is a property of the object type, not of the engine.
var damengMetadataKinds = map[string]struct {
	Type          string
	HasTerminator bool
}{
	"table":     {"TABLE", true},
	"view":      {"VIEW", true},
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
	if object == "schema" {
		// GET_DDL('SCHEMA',...) does not exist: DM8 answers -20001 "invalid
		// object type" (measured on dm8_20241022_rev244896). In Oracle-mode DM a
		// schema and its account are one object, so the definition is the one
		// GET_DDL('USER',...) returns -- CREATE USER "X" IDENTIFIED BY <PASSWORD>
		// ..., with the password masked by the server's own metadata layer. A
		// schema with no account (CTISYS, the built-in full-text schema) answers
		// -20008 "object is not found", which surfaces as a failed query rather
		// than a fabricated statement. A schema name is the whole reference, so
		// `schema` is not consulted.
		return "SELECT DBMS_METADATA.GET_DDL('USER'," + dmShowLit(strings.ToUpper(name)) + ") AS " + damengShowColumn + " FROM DUAL",
			damengShowColumn, false, true
	}
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
