package list

// objectAliases maps a list object to the single three-letter name its
// subcommand accepts. They are registered with Cobra, so `imex list dbs -D mydb`
// parses exactly like `imex list databases -D mydb` (same flags, same help,
// same exit codes).
//
// The set is fixed rather than prefix-generated: three letters are short enough
// to type, long enough to stay unambiguous, and stable across releases. Every
// alias is checked to be unique and disjoint from the object names by
// TestObjectAliasesAreUnique.
var objectAliases = map[string][]string{
	ObjectDatabases: {"dbs"},
	ObjectTables:    {"tab"},
	ObjectViews:     {"vws"},
	ObjectColumns:   {"col"},
	ObjectIndexes:   {"idx"},
	ObjectTriggers:  {"trg"},
	ObjectRoutines:  {"rtn"},
	ObjectEvents:    {"evt"},
}

// Aliases returns the short names of one object in help order. The slice is a
// copy, so a caller cannot change the registry through Cobra.
func Aliases(object string) []string {
	src := objectAliases[object]
	out := make([]string, len(src))
	copy(out, src)
	return out
}
