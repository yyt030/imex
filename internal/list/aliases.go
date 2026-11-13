package list

// objectShort maps a list object (its long name, which the query builders key
// on) to the three-letter name its subcommand is invoked by. Aligned with
// `show`, the short name is the Cobra command's Use and the long name is the
// registered alias, so `imex list --help` lists the short names and both
// `imex list tab -D mydb` and `imex list tables -D mydb` resolve to the same
// leaf (same flags, same help, same exit codes).
//
// The set is fixed rather than prefix-generated: three letters are short enough
// to type, long enough to stay unambiguous, and stable across releases. Every
// short name is checked to be unique, three letters, and disjoint from the
// object names by TestListShortNamesAreUnique.
var objectShort = map[string]string{
	ObjectDatabases: "dbs",
	ObjectSchemas:   "sch",
	ObjectTables:    "tab",
	ObjectViews:     "vws",
	ObjectColumns:   "col",
	ObjectIndexes:   "idx",
	ObjectTriggers:  "trg",
	ObjectRoutines:  "rtn",
	ObjectEvents:    "evt",
}

// ShortName returns the canonical command name for one object, falling back to
// the object itself when the registry has no entry.
func ShortName(object string) string {
	if s, ok := objectShort[object]; ok {
		return s
	}
	return object
}

// ShortNames returns every object's canonical short name in Objects() order,
// matching how show.ShortNames() feeds the parent usage message.
func ShortNames() []string {
	objs := Objects()
	out := make([]string, len(objs))
	for i, o := range objs {
		out[i] = ShortName(o)
	}
	return out
}
