package backend

import (
	"strings"
	"time"

	"github.com/imex/internal/common"
)

// One table answers every question imex asks about a Dameng Oracle-mode type
// name: how its text is formatted (ColumnKind), whether it carries raw bytes
// (binary), and what the dump must rewrite the driver's value into (textLayout
// / boolText). It is deliberately a single table: three parallel lists of type
// names is how `TIME` ended up classified but not rewritten, and a column whose
// driver text is left alone silently changes value between engines.
//
// Keys are the spellings normalizeDamengTypeName leaves behind: upper case,
// single-spaced, no length or precision suffix. Both the driver's
// DatabaseTypeName spelling and the catalogue's DATA_TYPE spelling go through
// the same normaliser, so a name written two ways (LONGVARBINARY and
// "LONG VARBINARY") needs both entries.
type damengType struct {
	kind common.ColumnKind
	// binary marks a raw-byte column: the DEL writer sends those as 0x hex,
	// which is the only form that survives both engines' text columns.
	binary bool
	// textLayout rewrites the RFC3339Nano text database/sql produces for the
	// time.Time the DM driver returns for every date/time type. Empty means the
	// driver text already matches what the dump should carry.
	textLayout string
	// boolText normalises Go's "true"/"false" — what the driver's bool for a
	// BOOLEAN column becomes on the way through database/sql — to one digit.
	boolText bool
}

var damengTypes = map[string]damengType{
	// Numbers: wire text passes through, --dec-pt/--strip-lzeros apply.
	"NUMBER":           {kind: common.KindNumber},
	"DECIMAL":          {kind: common.KindNumber},
	"DEC":              {kind: common.KindNumber},
	"NUMERIC":          {kind: common.KindNumber},
	"FLOAT":            {kind: common.KindNumber},
	"BINARY_FLOAT":     {kind: common.KindNumber},
	"BINARY_DOUBLE":    {kind: common.KindNumber},
	"INTEGER":          {kind: common.KindNumber},
	"INT":              {kind: common.KindNumber},
	"SMALLINT":         {kind: common.KindNumber},
	"TINYINT":          {kind: common.KindNumber},
	"BIGINT":           {kind: common.KindNumber},
	"REAL":             {kind: common.KindNumber},
	"DOUBLE":           {kind: common.KindNumber},
	"DOUBLE PRECISION": {kind: common.KindNumber},

	// Date and time. The driver returns time.Time for all of these, so each
	// needs a layout; the layouts differ because Oracle-mode DATE carries a
	// clock, TIME carries no date, and the zone-bearing types must keep their
	// offset or the dump no longer identifies an instant.
	"DATE": {kind: common.KindTimestamp,
		textLayout: "2006-01-02 15:04:05"},
	"TIME": {kind: common.KindTime,
		textLayout: "15:04:05.999999"},
	"TIME WITH TIME ZONE": {kind: common.KindTime,
		textLayout: "15:04:05.999999-07:00"},
	"DATETIME": {kind: common.KindTimestamp,
		textLayout: "2006-01-02 15:04:05.999999"},
	"DATETIME2": {kind: common.KindTimestamp,
		textLayout: "2006-01-02 15:04:05.999999"},
	"TIMESTAMP": {kind: common.KindTimestamp,
		textLayout: "2006-01-02 15:04:05.999999"},
	"DATETIME WITH TIME ZONE": {kind: common.KindTimestamp,
		textLayout: "2006-01-02 15:04:05.999999-07:00"},
	"TIMESTAMP WITH TIME ZONE": {kind: common.KindTimestamp,
		textLayout: "2006-01-02 15:04:05.999999-07:00"},
	"TIMESTAMP WITH LOCAL TIME ZONE": {kind: common.KindTimestamp,
		textLayout: "2006-01-02 15:04:05.999999-07:00"},
	// YEAR is a MySQL type; the entry keeps a catalogue spelling that names it
	// from falling into KindUnknown, which would stop --date-fmt from applying.
	"YEAR": {kind: common.KindYear},

	// Bit and boolean. The driver answers BOOLEAN with a Go bool ("true" in the
	// file) and BIT with an int8 ("1"), so only the first needs normalising;
	// both end up as one digit, which is the form every engine's smallint
	// column accepts. See requirements 11.2 for the target-column guidance.
	"BIT":     {boolText: true},
	"BOOLEAN": {boolText: true},

	// Raw binary. LONGVARBINARY and IMAGE are the spellings the DM driver and
	// its catalogue use for the large binary family; without them the bytes go
	// out as text and the file stops being valid in the dump's own charset.
	"BLOB":           {binary: true},
	"RAW":            {binary: true},
	"LONG RAW":       {binary: true},
	"BINARY":         {binary: true},
	"VARBINARY":      {binary: true},
	"LONGVARBINARY":  {binary: true},
	"LONG VARBINARY": {binary: true},
	"IMAGE":          {binary: true},

	// Character and large-text types: no rewrite, not binary.
	"CHAR":         {},
	"CHARACTER":    {},
	"VARCHAR":      {},
	"VARCHAR2":     {},
	"NCHAR":        {},
	"NVARCHAR2":    {},
	"CLOB":         {},
	"NCLOB":        {},
	"TEXT":         {},
	"LONG":         {},
	"LONGVARCHAR":  {},
	"LONG VARCHAR": {},
}

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

// damengTypeOf resolves a type name through the table.
func damengTypeOf(name string) (damengType, bool) {
	t, ok := damengTypes[normalizeDamengTypeName(name)]
	return t, ok
}

// classifyDamengType maps a Dameng Oracle-mode type name onto common.ColumnKind.
func classifyDamengType(name string) common.ColumnKind {
	if t, ok := damengTypeOf(name); ok {
		return t.kind
	}
	return common.KindUnknown
}

// isDamengBinaryType reports whether a Dameng Oracle-mode type name denotes raw
// binary content.
func isDamengBinaryType(name string) bool {
	t, _ := damengTypeOf(name)
	return t.binary
}

// damengCellFixup returns the rewriter the dump needs for one column, or nil
// when the driver's text is already what the file should carry.
func damengCellFixup(name string) func([]byte) []byte {
	t, ok := damengTypeOf(name)
	if !ok {
		return nil
	}
	switch {
	case t.textLayout != "":
		return dmTimeText(t.textLayout)
	case t.boolText:
		return dmBoolText
	default:
		return nil
	}
}

// dmTimeText parses the RFC3339 text database/sql produces for a time.Time and
// re-emits it in the layout the dump should carry.
func dmTimeText(layout string) func([]byte) []byte {
	return func(v []byte) []byte {
		t, err := time.Parse(time.RFC3339Nano, string(v))
		if err != nil {
			return v
		}
		return []byte(t.Format(layout))
	}
}

// dmBoolText renders the Go boolean text of a BOOLEAN column as one digit.
// Anything that is not exactly "true" or "false" — a BIT column's "1", an empty
// cell — passes through untouched.
func dmBoolText(v []byte) []byte {
	switch strings.ToLower(string(v)) {
	case "true":
		return []byte("1")
	case "false":
		return []byte("0")
	}
	return v
}
