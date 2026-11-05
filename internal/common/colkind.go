package common

// ColumnKind classifies a database column type for DEL text formatting and
// reverse-parsing on import. It is the tool's own vocabulary, not an engine's:
// the DEL, SQL and Parquet writers and the import readers all branch on it, and
// each engine's backend translates its type words into it (see
// internal/backend/types.go). Adding an engine adds a word table there, never a
// case here.
type ColumnKind int

const (
	// KindUnknown is any type whose wire text must pass through untouched
	// (string/enum/set/json and unknown future types).
	KindUnknown   ColumnKind = iota
	KindNumber               // numeric types: --dec-pt / --strip-lzeros apply
	KindDate                 // DATE: --date-fmt applies
	KindTime                 // TIME: text passthrough
	KindTimestamp            // DATETIME / TIMESTAMP: --timestamp-fmt applies
	KindYear                 // YEAR: passthrough ("0000" normalization upstream)
	// KindTimestampTZ is a DATETIME/TIMESTAMP whose text also carries a UTC
	// offset ("2024-05-06 07:08:09.123456+08:00"). The offset is part of the
	// value, so --date-fmt/--timestamp-fmt do not apply to it: a layout that
	// drops the offset would silently move the instant to another timezone.
	// The import side re-lays the text out where an engine's own literal
	// parser wants it — see internal/import/del_reader.go.
	KindTimestampTZ
)
