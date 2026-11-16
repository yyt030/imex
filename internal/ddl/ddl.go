// Package ddl rebuilds one table's CREATE statement in a grammar other than the
// connected engine's own.
//
// It is the `imex show --sql-dialect` conversion path. The alternative — read the
// server's own DDL text and rewrite it — was measured first and rejected: the
// text a DM8 GET_DDL call returns is Oracle grammar that a MySQL parser refuses
// outright (`syntax error at position 22 near 'IMEXIT'`), and the one MySQL text
// a parser could read came back 44% different from its own input once it was
// re-rendered, with the loss spread over defaults, index clauses and table
// options. A grammar-aware rewrite of an already-lossy reconstruction compounds
// the error, so this package takes the other route: read the catalogue, which
// holds the facts in typed columns, and print the target dialect from there.
//
// The design rule that follows is the one that makes the route safe: a fact the
// catalogue exposes and this package cannot render is a REFUSAL, never a gap. A
// reconstruction from text can only warn about what it dropped after the fact;
// a rebuild from typed columns knows the shape before it prints anything.
//
// Nothing here imports the engine's DDL text. internal/ddl owns the catalogue
// reads (catalog_dameng.go, the way internal/list owns list's per-engine queries)
// and the target-grammar printers (mysql.go); the neutral Table/Column/Index
// types in ddl.go are the only thing the two halves share.
package ddl

import (
	"context"
	"database/sql"

	"github.com/pkg/errors"

	"github.com/imex/internal/config"
)

// ErrNotFound says the named object is not in the catalogue at all.
//
// It is a sentinel rather than a message because the wording of a not-found
// belongs to the command: `show` prints `no table found: <reference>` for the
// server's own definition path too, and a conversion that answered with a
// differently-shaped message would make the object name look like a property of
// the grammar rather than of the run.
var ErrNotFound = errors.New("table not found in catalogue")

// Table is one table described from the catalogue: the facts a CREATE statement
// needs, with no dialect vocabulary in them.
//
// Names keep the spelling the server stores them in, which on Dameng's
// Oracle-compatible mode is upper case. Nothing here folds or quotes a name: the
// printer owns its dialect's identifier grammar.
type Table struct {
	Schema string // owning schema ("" when the reference was unqualified)
	Name   string

	Columns    []Column
	PrimaryKey []string // column names, in catalogue order
	// PrimaryKeyName is the constraint's own name. MySQL gives a primary key no
	// name of its own, so the printer drops it — but the reader still records it,
	// because a refusal that says "the primary key" is worth being able to name.
	PrimaryKeyName string
	UniqueKeys     []Key // unique constraints and unique indexes, in name order
	Indexes        []Key // everything else, in name order

	// ForeignKeys and Checks are not rendered; they are collected so the printer
	// can refuse by name instead of silently leaving a rule out of the statement.
	ForeignKeys []NamedColumns
	Checks      []NamedColumns

	// DisabledConstraints are the rules the source server is not enforcing. They
	// are a refusal rather than a skip: printing them would hand the target a rule
	// the source does not apply, and dropping them would print a table that is
	// freer than the one that was read.
	DisabledConstraints []string

	TableComment string

	// Partitioned and Temporary are the two shapes the catalogue can name that
	// this build refuses outright: a partitioned table needs a PARTITION clause
	// the neutral type does not carry, and a temporary table means something
	// different in the target's own grammar than it does in the source's.
	Partitioned bool
	Temporary   bool
}

// Key is one index or unique constraint: a name and its columns in position
// order.
type Key struct {
	Name    string
	Columns []string
	// Backing is the index name a unique constraint carries, recorded so the
	// reader can drop the index from Indexes and print the rule once.
	Backing string
	// Expression marks an index the catalogue reports as something other than a
	// plain column list (a functional or domain index); it is a refusal, and the
	// flag is what lets the message name the index.
	Expression bool
}

// NamedColumns is a constraint the printer refuses to render, kept with its
// columns so the error can point at the object by name.
type NamedColumns struct {
	Name    string
	Columns []string
	// RefSchema, RefConstraint and Rule are the extra text a refusal needs to be
	// actionable: what a foreign key points at, and the check condition. Dameng's
	// ALL_CONSTRAINTS names the referenced constraint but not its table (measured:
	// R_OWNER and R_CONSTRAINT_NAME exist, R_TABLE_NAME answers -2207), and a
	// constraint name is what an operator can look up in the source schema.
	RefSchema     string
	RefConstraint string
	Rule          string
}

// Column is one column described from the catalogue.
//
// Every field is what the server's own column means, not what the target would
// call it: Type is the source's type word, and Length/Precision/Scale carry the
// source's constraints on it. The printer decides how — and whether — it maps.
type Column struct {
	Name string
	// Type is the catalogue's type word, normalised: upper case, single-spaced,
	// no length or precision suffix (see backend.NormalizeDamengTypeName).
	Type string

	// ByteLength is the declared storage size in bytes (DATA_LENGTH), and
	// CharLength the declared size in characters (CHAR_LENGTH). CharUsed says
	// which of the two the declaration meant: 'B' bytes, 'C' characters, and ""
	// when the type has no size at all.
	ByteLength int
	CharLength int
	CharUsed   string

	// Precision and Scale answer "did the server even say?" — an absent NUMBER
	// precision means an unconstrained number, which is a different fact from a
	// declared precision of 1.
	Precision sql.NullInt64
	Scale     sql.NullInt64

	Nullable bool

	// Default is the catalogue's default text, and HasDefault distinguishes a
	// column that declares NULL from one that declares nothing.
	Default    string
	HasDefault bool

	// Virtual marks a generated column, Hidden an internal column. Both are
	// refusals: neither has a target-side spelling this build can produce.
	Virtual bool
	Hidden  bool
	// Identity marks the auto-increment column, which the printer may render as
	// the target's own auto-increment clause — but only in the one shape the
	// target grammar can hold without a caveat.
	Identity bool
	// Mark is a catalogue attribute bit this build has not measured, kept as the
	// number the server reported. It is never a rendering input: a column that
	// carries one is refused by name, because an unmeasured flag is a fact about
	// the column that the printed statement would simply not have.
	Mark string

	Comment string
}

// Convert prints schema.table's CREATE statement in the target grammar.
//
// engine names the source, target the grammar asked for. cmd refuses an
// unsupported pair before it dials — that refusal is the operator's answer — and
// the check here is the guard for a library caller and for the next pair added
// to one half of the table but not the other.
func Convert(ctx context.Context, db *sql.DB, engine, schema, table string, target config.SQLDialect) (string, error) {
	if !SupportedPair(engine, target) {
		return "", errors.Errorf("converting a table definition from engine %s to grammar %s is not supported", engine, target)
	}
	desc, err := describe(ctx, db, engine, schema, table)
	if err != nil {
		return "", err
	}
	return RenderMySQL(desc)
}

// describe reads the catalogue in the source engine's own vocabulary.
func describe(ctx context.Context, db *sql.DB, engine, schema, table string) (*Table, error) {
	switch engine {
	case "dm":
		return describeDameng(ctx, db, schema, table)
	}
	return nil, errors.Errorf("converting a table definition from engine %s is not supported: only dm converts to mysql", engine)
}

// Asked reports whether a run asked for a grammar other than the engine's own.
//
// `auto` and the engine's native name both mean "print what the server says", so
// neither is a conversion: a Dameng run named `--sql-dialect dm` is asking for
// the grammar the definition already arrived in, and refusing it would be a
// refusal of the identity map.
func Asked(engine string, target config.SQLDialect) bool {
	return target != config.SQLDialectAuto && target != config.NativeDialect(engine)
}

// SupportedPair reports whether engine→target is a conversion this build can
// print. cmd/show asks it before dialling, so an unsupported pair is answered
// without a connection and without a half-read catalogue; the wording of the
// refusal stays in cmd, because the message is CLI contract (face ③).
//
// The pair table and describe's switch are deliberately adjacent: a new source
// engine or a new target grammar has to appear in both, and
// TestConvertMatchesSupportedPair fails the build when it appears in only one.
func SupportedPair(engine string, target config.SQLDialect) bool {
	return engine == "dm" && target == config.SQLDialectMySQL
}
