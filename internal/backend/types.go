package backend

import (
	"database/sql"
	"strings"

	"github.com/imex/internal/common"
)

// This file holds one layer of the type system and deliberately holds no engine
// name. The three things the tool needs to know about a column are the driver's
// own type word, the engine's binary decision, and common.ColumnKind — the
// output vocabulary every writer and reader branches on.
//
// classifyColumnKind is the shared resolver from a type word to that vocabulary,
// and its word list is the MySQL/MariaDB spelling (MEDIUMINT, the "UNSIGNED "
// prefix go-sql-driver puts in front of unsigned columns, and so on) because
// that vocabulary was defined by the first engine the tool supported. It is still
// not MySQL-specific in the sense that matters: PostgreSQL answers `bigint` and
// `numeric` and `timestamp` with words this table already understands, so
// postgres.go maps its catalogue words into them and calls the same resolver
// rather than duplicating the kind arithmetic. A new engine that needs a third
// vocabulary adds its own translator in its own file, next to its binary-type
// table; this file stays the one place the vocabulary is defined.

// classifyColumnKind maps a driver DatabaseTypeName to a common.ColumnKind.
func classifyColumnKind(databaseTypeName string) common.ColumnKind {
	// go-sql-driver/mysql prefixes unsigned columns with "UNSIGNED "
	// (e.g. "UNSIGNED BIGINT"), while the base type is what decides the kind.
	name := strings.TrimPrefix(strings.ToUpper(databaseTypeName), "UNSIGNED ")
	switch name {
	case "DECIMAL", "DEC", "NUMERIC", "FLOAT", "DOUBLE", "REAL",
		"TINYINT", "SMALLINT", "MEDIUMINT", "INT", "INTEGER", "BIGINT", "SERIAL":
		return common.KindNumber
	case "DATE":
		return common.KindDate
	case "TIME":
		return common.KindTime
	case "DATETIME", "TIMESTAMP":
		return common.KindTimestamp
	case "YEAR":
		return common.KindYear
	default:
		return common.KindUnknown
	}
}

// isBinaryTypeName reports whether a driver DatabaseTypeName denotes a raw
// binary column (BLOB/BINARY/...). These map to plain BYTE_ARRAY (no UTF-8
// logical annotation) in Parquet so non-UTF8 content round-trips intact.
func isBinaryTypeName(databaseTypeName string) bool {
	switch strings.ToUpper(databaseTypeName) {
	case "TINYBLOB", "BLOB", "MEDIUMBLOB", "LONGBLOB", "LONG",
		"BINARY", "VARBINARY", "GEOMETRY", "BIT":
		return true
	default:
		return false
	}
}

// applyColumnKinds derives ColumnKinds from driver column types.
func applyColumnKinds(colTypes []*sql.ColumnType) []common.ColumnKind {
	kinds := make([]common.ColumnKind, len(colTypes))
	for i, ct := range colTypes {
		kinds[i] = classifyColumnKind(ct.DatabaseTypeName())
	}
	return kinds
}

// binaryColumnFlags derives per-column binary flags from driver column types.
func binaryColumnFlags(colTypes []*sql.ColumnType) []bool {
	flags := make([]bool, len(colTypes))
	for i, ct := range colTypes {
		flags[i] = isBinaryTypeName(ct.DatabaseTypeName())
	}
	return flags
}
