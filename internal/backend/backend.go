// Package backend isolates everything that differs between the database
// engines imex talks to: how a connection string is built, how an identifier is
// quoted, how a placeholder is spelled, which statement opens a consistent
// snapshot, and where the metadata questions are asked.
//
// The split exists because the rest of the tool is engine-independent: the DEL,
// SQL and Parquet writers, the record readers, the batching pipeline, the stats
// and the log records. Everything below is a string or a driver setting, so each
// method has one job and the MySQL implementations return the exact statements
// the tool has always sent. That is a contract, not a convenience: contract face
// ⑤ in CONTEXT.md is "the SQL the server sees", and a port that quietly rewrites
// `START TRANSACTION WITH CONSISTENT SNAPSHOT` into something else would change
// the tool's observable behaviour for every existing MySQL run. The general_log
// diff recorded in the change record for this feature is what proves the claim.
package backend

import (
	"context"
	"database/sql"
	"strings"

	"github.com/pkg/errors"

	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

// Engine is the value of --engine. The names are the CLI spelling; "pg" is the
// conventional abbreviation of PostgreSQL and is also what the tool prints, so
// the flag value, the log field and the error text all agree.
type Engine string

const (
	// EngineMySQL is the default and the historical behaviour: MySQL, MariaDB
	// and OceanBase, all reached through the MySQL wire protocol.
	EngineMySQL Engine = "mysql"
	// EnginePG is PostgreSQL, reached through the PostgreSQL wire protocol via
	// pgx. Dameng (DM8) was evaluated and is not supported: see the change
	// record that introduced this package for the measured reasons.
	EnginePG Engine = "pg"
)

// engineOrder is the order --engine values are listed in help text and in the
// rejection message. It is a slice, not a map, for the same reason the delimiter
// validation in cmd/validate.go uses one: the order a value is reported in is
// part of the error-text contract.
var engineOrder = []Engine{EngineMySQL, EnginePG}

// Engines returns the accepted --engine values in contract order.
func Engines() []string {
	out := make([]string, len(engineOrder))
	for i, e := range engineOrder {
		out[i] = string(e)
	}
	return out
}

// Parse turns a --engine value into an Engine. An empty value is the default
// engine rather than an error: `imex export -h ... -u ...` without --engine must
// keep doing exactly what it did before this option existed.
func Parse(s string) (Engine, error) {
	switch Engine(strings.ToLower(strings.TrimSpace(s))) {
	case "", EngineMySQL:
		return EngineMySQL, nil
	case EnginePG:
		return EnginePG, nil
	default:
		return "", errors.Errorf("invalid --engine %q: must be one of %s", s, strings.Join(Engines(), "/"))
	}
}

// SnapshotStep is one statement of a consistent read, plus the log message that
// names it when it fails.
type SnapshotStep struct {
	SQL  string
	Warn string
}

// BatchWords is the engine-specific wording of the import batch resolver's log
// records: which ceiling is being named, and what the run says when one of them
// decides the batch instead of the configured size. The strings are data rather
// than code because the resolver's arithmetic is identical for every engine —
// only the names differ, and only the MySQL values are pinned by the existing
// message contract (face ③). An engine whose ceiling genuinely has no analogue
// must not be described by MySQL's words: PostgreSQL has no max_allowed_packet
// and does not interpolate a statement, so it names the protocol message cap and
// says nothing about a driver fallback it cannot have.
type BatchWords struct {
	// PacketCap names the server-side ceiling, both as the log field of the
	// resolved record and as the limiting_factor value that picks it.
	PacketCap string
	// DriverCap is the limiting_factor value when the client-side ceiling, not
	// the server's, decides the budget.
	DriverCap string
	// PacketUnknown is reported when the server ceiling could not be read and
	// the assumed value is in use.
	PacketUnknown string
	// DriverOver is reported when an explicit --statement-size is above the
	// client-side ceiling, so the caller's number is honoured as given.
	DriverOver string
	// BatchSizeClamp is reported when --batch-size had to be lowered.
	BatchSizeClamp string
}

// Backend is the engine-specific behaviour the rest of the tool needs. The
// method set is deliberately the union of the call sites that were MySQL-only,
// and nothing more: sharding, SQL-format dumps and SHOW CREATE are absent
// because an engine that cannot do them refuses the option at the CLI level
// instead of pretending.
type Backend interface {
	// Engine reports which engine this backend drives. It goes into the
	// configuration echo so a log line says what was actually dialed.
	Engine() Engine

	// Open builds the connection string and hands back an unopened *sql.DB.
	// Neither implementation dials here: both return lazily so the caller's
	// PingWithRetry stays the single place a connectivity failure is decided.
	Open(cfg *config.Config) (*sql.DB, error)

	// QuoteIdent quotes one identifier (a table or column name) the way this
	// engine spells it.
	QuoteIdent(name string) string

	// ValueGroups renders rowCount placeholder groups of ncols placeholders
	// each, numbered the way the engine requires. MySQL repeats the positional
	// `?`; PostgreSQL carries an explicit ordinal per parameter across the whole
	// statement.
	ValueGroups(rowCount, ncols int) string

	// PlaceholderList renders count bare placeholders separated by commas, the
	// first one numbered start. This is what an `IN (...)` membership test needs:
	// a flat list of scalars. ValueGroups must not be used for that, because its
	// parentheses turn the list into a single row constructor and the server
	// answers `varchar = row` with a type error instead of a match.
	PlaceholderList(start, count int) string

	// BatchRowCap answers how many rows one statement may carry for a table with
	// ncols columns, and what the protocol ceiling behind that number is. rows is
	// 0 when the engine sets no ceiling beyond the byte budget. Both engines land
	// on 65535 for their own reason (see the implementations), and the wording
	// that reports a clamp lives in BatchWords.
	BatchRowCap(ncols int) (rows int, limit int)

	// BatchWords returns the engine-specific wording of the batch-budget records.
	BatchWords() BatchWords

	// UpsertSuffix returns the clause that turns an INSERT into
	// "update on duplicate key", or an error for engines whose syntax the tool
	// does not emit. table is the target of the INSERT, already the way the
	// statement spells it; conflictTarget is what UpsertNeedsTarget asks for. An
	// engine that needs neither passes them straight through unused.
	//
	// The second result is a RETURNING list, and it is what makes the counters
	// true. PostgreSQL's affected-rows number counts a rewritten row and a new
	// row the same way, so a run that updated 999 rows and inserted 1 reports
	// exactly what a run that inserted all 1,000 reports; asking for one row per
	// row the statement touched is the only way to tell them apart. MySQL answers
	// 1, 2 or 0 per row and needs nothing back, so it returns "".
	//
	// The engines report the same three categories, not two. An INSERT that meets
	// a row whose values are already the ones the file carries leaves that row
	// alone on both engines — MySQL counts 0 affected rows, PostgreSQL's clause
	// adds a WHERE guard and answers no row — and the importer files the difference
	// under unchanged. See UpsertSuffix on each engine for the measured form.
	UpsertSuffix(table string, columns, conflictTarget []string) (suffix, returning string, err error)

	// UpsertNeedsTarget says whether the engine's upsert syntax has to be told
	// which key it conflicts on. MySQL's ON DUPLICATE KEY UPDATE matches any
	// unique key the server finds, so there is nothing to name; PostgreSQL's
	// ON CONFLICT does not have that freedom, and an unnamed one is a syntax
	// error. The caller resolves the target only where this answers true, so a
	// MySQL run pays no extra catalogue query for PostgreSQL's grammar.
	UpsertNeedsTarget() bool

	// FKCheckToggle returns the two statements that turn referential checks off
	// and back on around a --mode replace run, or empty strings for an engine
	// with no such session setting. The caller emits neither when they are empty:
	// a run must not log and retry a statement the server cannot parse.
	FKCheckToggle() (off, on string)

	// SnapshotSteps lists, in order, the statements that open a repeatable-read
	// consistent read on a fresh connection, and the wording the tool has always
	// used when each one fails. The wording is part of the message contract, so
	// it travels with the statement instead of being invented by the caller:
	// MySQL has two steps with two distinct warnings, PostgreSQL has one.
	SnapshotSteps() []SnapshotStep

	// MaxConnectionsSQL reads the server's connection ceiling, which is what the
	// parallel paths compare --threads against before any data moves.
	MaxConnectionsSQL() string

	// PacketCeiling reads the largest statement the server accepts in one
	// message, which is what bounds an import batch's byte budget. A nil db (a
	// library caller that never connected) returns the assumed value without an
	// error, so the caller can resolve a budget for a dry run.
	PacketCeiling(ctx context.Context, db *sql.DB) (int64, error)

	// DriverStatementCap is the client-side ceiling past which the driver stops
	// using the protocol the batching design targets. Batches stay under it even
	// when the server would accept more.
	DriverStatementCap() int64

	// TableExistsSQL, TargetColumnCheckSQL, ColumnListSQL, ColumnTypesSQL and
	// GeneratedColumnsSQL are the import-side metadata probes. Each takes the
	// number of user-supplied names it has to bind and returns the statement
	// with placeholders already in place; the arguments always come in the same
	// order: database, table, then the names.
	TableExistsSQL() string
	TargetColumnCheckSQL(n int) string
	ColumnListSQL() string
	ColumnTypesSQL(n int) string
	GeneratedColumnsSQL() string

	// UniqueKeyColumnsSQL lists the columns of the table's unique indexes, which
	// is what decides whether an upsert can ever match. Engines that refuse
	// --mode insert_update still answer with a valid statement, because the
	// exporter-side column pre-flight is not mode-dependent.
	UniqueKeyColumnsSQL() string

	// UniqueKeyIndexColumnsSQL returns the same catalogue one row per key column
	// instead of one row per table: (index or constraint name, primary-key flag,
	// column name), ordered so a primary key's rows come before a unique key's
	// and each group is in key order. This is what names an ON CONFLICT target;
	// UniqueKeyColumnsSQL stays the count the pre-flight uses, so a MySQL run
	// emits exactly the statements it always did.
	UniqueKeyIndexColumnsSQL() string

	// ColumnKinds and BinaryFlags classify the columns of a result set for the
	// DEL/SQL/Parquet writers. They take driver type names, which are
	// engine-vocabulary, and return the tool's own vocabulary.
	ColumnKinds(colTypes []*sql.ColumnType) []common.ColumnKind
	BinaryFlags(colTypes []*sql.ColumnType) []bool

	// MetadataKind and MetadataBinary do the same two classifications for the
	// type words a catalogue view reports (information_schema.DATA_TYPE and
	// friends), which is what the import side reads before it knows anything
	// about the result set. The vocabularies differ per engine, so a MySQL word
	// is not a PostgreSQL word and neither is portable.
	MetadataKind(dataType string) common.ColumnKind
	MetadataBinary(dataType string) bool

	// DefaultPort is the port the engine listens on when neither --port nor
	// IMEX_PORT named one.
	DefaultPort() int

	// CellFixups returns one rewriter per result column, or nil for a column
	// whose driver text is already what the dump should carry. The MySQL
	// implementation returns only nils: its text protocol hands over the bytes
	// the server stores, which is the whole basis of the sql.RawBytes read path
	// (see exporter.go's parseTime comment).
	CellFixups(colTypes []*sql.ColumnType) []func([]byte) []byte
}

// Get returns the backend for one engine. The returned value is a stateless
// singleton, so callers may keep it on a struct without worrying about lifetime.
func Get(e Engine) (Backend, error) {
	switch e {
	case EngineMySQL:
		return mysqlBackend{}, nil
	case EnginePG:
		return postgresBackend{}, nil
	default:
		return nil, errors.Errorf("unsupported engine %q: must be one of %s", string(e), strings.Join(Engines(), "/"))
	}
}

// Must returns the backend for one engine and panics on an unknown value. Every
// call site reaches it through config.Config.Engine, which the CLI has already
// run through Parse, so a panic here is a programming error rather than user
// input; library callers that build a Config by hand are covered by Get.
func Must(e Engine) Backend {
	b, err := Get(e)
	if err != nil {
		panic(err)
	}
	return b
}

// ForConfig resolves the engine stored on a connection config.
func ForConfig(cfg *config.Config) (Backend, error) {
	e, err := Parse(cfg.Engine)
	if err != nil {
		return nil, err
	}
	return Get(e)
}
