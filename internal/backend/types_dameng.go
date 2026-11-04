package backend

import (
	"strings"

	"github.com/imex/internal/common"
)

// normalizeDamengTypeName upper-cases a type name and drops a length or
// precision suffix such as NUMBER(10,2), while keeping multi-word names such as
// LONG RAW or TIMESTAMP WITH TIME ZONE intact. It accepts both the driver's
// DatabaseTypeName spelling and the catalogue's DATA_TYPE spelling.
func normalizeDamengTypeName(name string) string {
	up := strings.ToUpper(strings.TrimSpace(name))
	if i := strings.IndexByte(up, '('); i >= 0 {
		up = up[:i]
	}
	return strings.Join(strings.Fields(up), " ")
}

// classifyDamengType maps a Dameng Oracle-mode type name onto common.ColumnKind.
func classifyDamengType(name string) common.ColumnKind {
	switch normalizeDamengTypeName(name) {
	case "NUMBER", "DECIMAL", "DEC", "NUMERIC", "FLOAT", "BINARY_FLOAT", "BINARY_DOUBLE",
		"INTEGER", "INT", "SMALLINT", "TINYINT", "BIGINT", "REAL", "DOUBLE", "DOUBLE PRECISION":
		return common.KindNumber
	case "DATE":
		// Oracle-mode DATE carries both date and time; classify as timestamp so
		// the time component is preserved in text output.
		return common.KindTimestamp
	case "TIMESTAMP", "TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITH LOCAL TIME ZONE":
		return common.KindTimestamp
	case "YEAR":
		return common.KindYear
	case "TIME", "TIME WITH TIME ZONE":
		return common.KindTime
	default:
		return common.KindUnknown
	}
}

// isDamengBinaryType reports whether a Dameng Oracle-mode type name denotes raw
// binary content.
func isDamengBinaryType(name string) bool {
	switch normalizeDamengTypeName(name) {
	case "BLOB", "RAW", "LONG RAW", "BINARY", "VARBINARY":
		return true
	default:
		return false
	}
}
