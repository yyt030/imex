package backend

import (
	"testing"

	"github.com/imex/internal/common"
)

// The word table lives in this package now (types.go), so the classification
// table is asserted here rather than through an export test that happened to
// reach into common.

func TestClassifyColumnKind(t *testing.T) {
	cases := map[string]common.ColumnKind{
		"DECIMAL":          common.KindNumber,
		"int":              common.KindNumber,
		"BIGINT":           common.KindNumber,
		"UNSIGNED BIGINT":  common.KindNumber,
		"UNSIGNED INT":     common.KindNumber,
		"UNSIGNED DECIMAL": common.KindNumber,
		"DATE":             common.KindDate,
		"TIME":             common.KindTime,
		"DATETIME":         common.KindTimestamp,
		"TIMESTAMP":        common.KindTimestamp,
		"YEAR":             common.KindYear,
		"VARCHAR":          common.KindUnknown,
		"BLOB":             common.KindUnknown,
		"TINYTEXT":         common.KindUnknown,
		"ENUM":             common.KindUnknown,
	}
	for name, want := range cases {
		if got := classifyColumnKind(name); got != want {
			t.Errorf("classifyColumnKind(%q) = %v, want %v", name, got, want)
		}
	}
	if !isBinaryTypeName("LONGBLOB") || !isBinaryTypeName("VARBINARY") || isBinaryTypeName("TEXT") {
		t.Error("isBinaryTypeName misclassification")
	}
}
