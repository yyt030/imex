package common

import (
	"database/sql"
	"strings"
)

// ColumnKind classifies a database column type for DEL text formatting and
// reverse-parsing on import.
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

// ClassifyColumnKind maps a driver DatabaseTypeName to a ColumnKind.
func ClassifyColumnKind(databaseTypeName string) ColumnKind {
	switch strings.ToUpper(databaseTypeName) {
	case "DECIMAL", "DEC", "NUMERIC", "FLOAT", "DOUBLE", "REAL",
		"TINYINT", "SMALLINT", "MEDIUMINT", "INT", "INTEGER", "BIGINT", "SERIAL":
		return KindNumber
	case "DATE":
		return KindDate
	case "TIME":
		return KindTime
	case "DATETIME", "TIMESTAMP":
		return KindTimestamp
	case "YEAR":
		return KindYear
	default:
		return KindUnknown
	}
}

// IsBinaryTypeName reports whether a driver DatabaseTypeName denotes a raw
// binary column (BLOB/BINARY/...). These map to plain BYTE_ARRAY (no UTF-8
// logical annotation) in Parquet so non-UTF8 content round-trips intact.
func IsBinaryTypeName(databaseTypeName string) bool {
	switch strings.ToUpper(databaseTypeName) {
	case "TINYBLOB", "BLOB", "MEDIUMBLOB", "LONGBLOB", "LONG",
		"BINARY", "VARBINARY", "GEOMETRY", "BIT":
		return true
	default:
		return false
	}
}

// ApplyColumnKinds derives ColumnKinds from driver column types.
func ApplyColumnKinds(colTypes []*sql.ColumnType) []ColumnKind {
	kinds := make([]ColumnKind, len(colTypes))
	for i, ct := range colTypes {
		kinds[i] = ClassifyColumnKind(ct.DatabaseTypeName())
	}
	return kinds
}

// BinaryColumnFlags derives per-column binary flags from driver column types.
func BinaryColumnFlags(colTypes []*sql.ColumnType) []bool {
	flags := make([]bool, len(colTypes))
	for i, ct := range colTypes {
		flags[i] = IsBinaryTypeName(ct.DatabaseTypeName())
	}
	return flags
}
