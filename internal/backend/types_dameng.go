package backend

import (
	"strings"
	"time"

	"github.com/imex/internal/common"
)

// damengBind says how a value must reach the driver for one Dameng type. The
// question is separate from ColumnKind on purpose: ColumnKind describes the
// *text* (which layout, which modifier), while this describes the *bind*, and
// DM's BIT column has no kind of its own at all.
//
// The categories come from what the driver does with a Go string, measured
// against a live DM8 for gitee.com/chunanyong/dm v1.8.23 (`fromString` in zi.go):
// an integer or decimal target parses the text with a 64-bit big.Float, so the
// digits past about the 15th are the binary float's, not the value's.
type damengBind int

const (
	// bindUnset is the zero value, and it is a bug: TestDamengTypeTableSingleSource
	// fails the build if any row of damengTypes is left unclassified, which is
	// how a new type name becomes a measured decision instead of a silent
	// fall-through to the drifting text path.
	bindUnset damengBind = iota
	// bindExactNumber: NUMBER and friends. The value goes over as an exact
	// decimal so no digit is lost on the way.
	bindExactNumber
	// bindInteger: TINYINT, SMALLINT, INT, INTEGER, BIGINT. The coefficient is
	// rounded half away from zero first — the number of digits that text
	// landing in these columns produces is the point of the exercise — then
	// bound exactly, which is what turns a wraparound into the server's own
	// overflow error.
	bindInteger
	// bindFloatNumber: FLOAT, REAL, DOUBLE and friends. A binary-float target
	// has nowhere to put a 20th significant digit, so the text path is already
	// right; the only thing to fix is a raw byte payload arriving here.
	bindFloatNumber
	// bindBit: BIT and BOOLEAN. The driver's text path answers "0" for anything
	// it cannot read as one of the four words it knows, so the value has to be
	// reduced to 0 or 1 by us.
	bindBit
	// bindText: character and date/time targets. Text passes through; a raw
	// byte payload becomes text, which is what the file's own text column
	// already was.
	bindText
	// bindRaw: binary targets already receive bytes, and bytes are what these
	// columns store.
	bindRaw
)

// One table answers every question imex asks about a Dameng Oracle-mode type
// name: how its text is formatted (ColumnKind), whether it carries raw bytes
// (binary), what the dump must rewrite the driver's value into (textLayout
// / boolText), and how an imported value must reach the driver (bind). It is
// deliberately a single table: three parallel lists of type names is how `TIME`
// ended up classified but not rewritten, and a column whose driver text is left
// alone silently changes value between engines.
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
	// bind is how an imported value reaches the driver for this type. Every row
	// must set it; bindUnset is a build failure, not a default.
	bind damengBind
}

var damengTypes = map[string]damengType{
	// Numbers: wire text passes through, --dec-pt/--strip-lzeros apply.
	"NUMBER":           {kind: common.KindNumber, bind: bindExactNumber},
	"DECIMAL":          {kind: common.KindNumber, bind: bindExactNumber},
	"DEC":              {kind: common.KindNumber, bind: bindExactNumber},
	"NUMERIC":          {kind: common.KindNumber, bind: bindExactNumber},
	"FLOAT":            {kind: common.KindNumber, bind: bindFloatNumber},
	"BINARY_FLOAT":     {kind: common.KindNumber, bind: bindFloatNumber},
	"BINARY_DOUBLE":    {kind: common.KindNumber, bind: bindFloatNumber},
	"INTEGER":          {kind: common.KindNumber, bind: bindInteger},
	"INT":              {kind: common.KindNumber, bind: bindInteger},
	"SMALLINT":         {kind: common.KindNumber, bind: bindInteger},
	"TINYINT":          {kind: common.KindNumber, bind: bindInteger},
	"BIGINT":           {kind: common.KindNumber, bind: bindInteger},
	"REAL":             {kind: common.KindNumber, bind: bindFloatNumber},
	"DOUBLE":           {kind: common.KindNumber, bind: bindFloatNumber},
	"DOUBLE PRECISION": {kind: common.KindNumber, bind: bindFloatNumber},

	// Date and time. The driver returns time.Time for all of these, so each
	// needs a layout; the layouts differ because Oracle-mode DATE carries a
	// clock, TIME carries no date, and the zone-bearing types must keep their
	// offset or the dump no longer identifies an instant.
	//
	// The three zone-bearing timestamp types get common.KindTimestampTZ rather
	// than KindTimestamp: their text ends in an offset, which is the one part
	// of a timestamp --timestamp-fmt cannot re-lay out without changing the
	// instant, and it is what the import side looks for to hand Dameng the
	// literal form its own parser accepts.
	"DATE": {kind: common.KindTimestamp,
		textLayout: "2006-01-02 15:04:05", bind: bindText},
	"TIME": {kind: common.KindTime,
		textLayout: "15:04:05.999999", bind: bindText},
	"TIME WITH TIME ZONE": {kind: common.KindTime,
		textLayout: "15:04:05.999999-07:00", bind: bindText},
	"DATETIME": {kind: common.KindTimestamp,
		textLayout: "2006-01-02 15:04:05.999999", bind: bindText},
	"DATETIME2": {kind: common.KindTimestamp,
		textLayout: "2006-01-02 15:04:05.999999", bind: bindText},
	"TIMESTAMP": {kind: common.KindTimestamp,
		textLayout: "2006-01-02 15:04:05.999999", bind: bindText},
	"DATETIME WITH TIME ZONE": {kind: common.KindTimestampTZ,
		textLayout: "2006-01-02 15:04:05.999999-07:00", bind: bindText},
	"TIMESTAMP WITH TIME ZONE": {kind: common.KindTimestampTZ,
		textLayout: "2006-01-02 15:04:05.999999-07:00", bind: bindText},
	"TIMESTAMP WITH LOCAL TIME ZONE": {kind: common.KindTimestampTZ,
		textLayout: "2006-01-02 15:04:05.999999-07:00", bind: bindText},
	// YEAR is a MySQL type; the entry keeps a catalogue spelling that names it
	// from falling into KindUnknown, which would stop --date-fmt from applying.
	"YEAR": {kind: common.KindYear, bind: bindText},

	// Bit and boolean. The driver answers BOOLEAN with a Go bool ("true" in the
	// file) and BIT with an int8 ("1"), so only the first needs normalising;
	// both end up as one digit, which is the form every engine's smallint
	// column accepts. See requirements 11.2 for the target-column guidance.
	"BIT":     {boolText: true, bind: bindBit},
	"BOOLEAN": {boolText: true, bind: bindBit},

	// Raw binary. LONGVARBINARY and IMAGE are the spellings the DM driver and
	// its catalogue use for the large binary family; without them the bytes go
	// out as text and the file stops being valid in the dump's own charset.
	"BLOB":           {binary: true, bind: bindRaw},
	"RAW":            {binary: true, bind: bindRaw},
	"LONG RAW":       {binary: true, bind: bindRaw},
	"BINARY":         {binary: true, bind: bindRaw},
	"VARBINARY":      {binary: true, bind: bindRaw},
	"LONGVARBINARY":  {binary: true, bind: bindRaw},
	"LONG VARBINARY": {binary: true, bind: bindRaw},
	"IMAGE":          {binary: true, bind: bindRaw},

	// Character and large-text types: no rewrite, not binary.
	"CHAR":         {bind: bindText},
	"CHARACTER":    {bind: bindText},
	"VARCHAR":      {bind: bindText},
	"VARCHAR2":     {bind: bindText},
	"NCHAR":        {bind: bindText},
	"NVARCHAR2":    {bind: bindText},
	"CLOB":         {bind: bindText},
	"NCLOB":        {bind: bindText},
	"TEXT":         {bind: bindText},
	"LONG":         {bind: bindText},
	"LONGVARCHAR":  {bind: bindText},
	"LONG VARCHAR": {bind: bindText},
}

// normalizeDamengTypeName upper-cases a type name and drops a length or
// precision suffix such as NUMBER(10,2), while keeping multi-word names such as
// LONG RAW or TIMESTAMP WITH TIME ZONE intact. The suffix is removed rather than
// treated as the end of the name, because a zone-bearing spelling can carry it in
// the middle — "TIMESTAMP(6) WITH TIME ZONE" must still find the TIMESTAMP WITH
// TIME ZONE entry, whose absence would leave the offset out of the dump.
// It accepts both the driver's DatabaseTypeName spelling and the catalogue's
// DATA_TYPE spelling; measured on DM8, the catalogue never writes a suffix at all
// ("DATETIME WITH TIME ZONE" for a TIMESTAMP(6) WITH TIME ZONE column).
func normalizeDamengTypeName(name string) string {
	up := strings.ToUpper(strings.TrimSpace(name))
	if i := strings.IndexByte(up, '('); i >= 0 {
		if j := strings.IndexByte(up[i:], ')'); j >= 0 {
			up = up[:i] + up[i+j+1:]
		} else {
			up = up[:i]
		}
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

// damengBindOf answers how a value must reach the driver for one Dameng type
// name. The second result is false for a name the table does not know at all,
// which is the only case that leaves the value exactly as it arrived.
func damengBindOf(name string) (damengBind, bool) {
	t, ok := damengTypeOf(name)
	return t.bind, ok && t.bind != bindUnset
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
