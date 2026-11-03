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
)
