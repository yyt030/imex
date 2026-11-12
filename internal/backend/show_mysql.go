package backend

// showDefinitions is the MySQL half of ShowDefinition: the six SHOW CREATE
// statements and the result column that carries the definition text.
//
// The column is named rather than indexed because its position is not stable
// across the six statements — measured on MariaDB 10.6.23: table and view return
// it second, trigger, procedure and function third, event fourth. Selecting a
// fixed index would print sql_mode.
//
// The text itself is copied verbatim from show.go, where it lived until round
// 133; the statements are byte-identical to the ones this tool has always sent
// (face ⑤), which is why the move needed no oracle re-baseline.
var showDefinitions = map[string]struct {
	Statement string
	Column    string
}{
	// A MySQL schema is a database, and SHOW CREATE DATABASE is the only
	// statement that prints a definition for that layer.
	"schema":    {"SHOW CREATE DATABASE", "Create Database"},
	"table":     {"SHOW CREATE TABLE", "Create Table"},
	"view":      {"SHOW CREATE VIEW", "Create View"},
	"trigger":   {"SHOW CREATE TRIGGER", "SQL Original Statement"},
	"event":     {"SHOW CREATE EVENT", "Create Event"},
	"procedure": {"SHOW CREATE PROCEDURE", "Create Procedure"},
	"function":  {"SHOW CREATE FUNCTION", "Create Function"},
}

// ShowDefinition answers from the MySQL statement table, building the same
// verb + " " + quoted-reference bytes the show package emitted before round
// 133. schema and name are the resolved reference halves; an empty schema
// leaves the name unqualified, which the server resolves in the connection's
// own database — exactly what the old show.qualify did.
//
// MySQL's SHOW CREATE returns no terminator, so hasTerminator is false and the
// tool still appends the `imex import -f sql`-readable `;`.
func (m mysqlBackend) ShowDefinition(object, schema, name string) (string, string, bool, bool) {
	d, ok := showDefinitions[object]
	if !ok {
		return "", "", false, false
	}
	if object == "schema" {
		// One namespace: a schema IS a database, so the whole reference is the
		// schema and a qualifier would be a second database name the server
		// rejects. SCHEMATA is per-database, so `schema` cannot appear here.
		return d.Statement + " " + m.QuoteIdent(name), d.Column, false, true
	}
	ref := m.QuoteIdent(name)
	if schema != "" {
		ref = m.QuoteIdent(schema) + "." + ref
	}
	return d.Statement + " " + ref, d.Column, false, true
}
