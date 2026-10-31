package config

// List output formats. They name how a `list` result is rendered, not a file
// format: list never writes a file, it writes stdout. Only table remains; the
// option is kept so a caller can state the format explicitly.
const (
	ListFormatTable = "table"
)

// ListConfig holds the options of one `imex list <object>` run.
//
// The embedded Config carries the connection options. Database is the scope
// object commands look in; it is optional for databases/tables/views/triggers/
// routines/events and required for columns/indexes, whose table name would
// otherwise be ambiguous across schemas.
type ListConfig struct {
	Config

	Object  string // databases, tables, views, columns, indexes, triggers, routines, events
	Table   string // --table, used by columns/indexes/triggers
	Pattern string // --pattern, a SQL LIKE pattern on the object name
	Type    string // --type: base/view/all for tables, procedure/function/all for routines
	All     bool   // --all: include system schemas when --database is not given

	Format     string // table
	NoColNames bool   // do not print the header row
}

// NewListConfig returns a ListConfig with the shipped defaults.
func NewListConfig() *ListConfig {
	return &ListConfig{
		Config: defaultConfig(),
		Format: ListFormatTable,
	}
}

// ValidListFormat reports whether format is the supported renderer.
func ValidListFormat(format string) bool {
	return format == ListFormatTable
}
