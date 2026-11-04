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
	"table":     {"SHOW CREATE TABLE", "Create Table"},
	"view":      {"SHOW CREATE VIEW", "Create View"},
	"trigger":   {"SHOW CREATE TRIGGER", "SQL Original Statement"},
	"event":     {"SHOW CREATE EVENT", "Create Event"},
	"procedure": {"SHOW CREATE PROCEDURE", "Create Procedure"},
	"function":  {"SHOW CREATE FUNCTION", "Create Function"},
}

// ShowDefinition answers from the MySQL statement table.
func (mysqlBackend) ShowDefinition(object string) (string, string, bool) {
	d, ok := showDefinitions[object]
	if !ok {
		return "", "", false
	}
	return d.Statement, d.Column, true
}
