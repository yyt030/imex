package backend

import (
	"context"
	"database/sql"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

// postgresBackend drives PostgreSQL over its own wire protocol, through pgx's
// database/sql wrapper.
//
// Three differences dominate the port, in order of how much they hurt: the
// placeholder syntax ($n rather than ?, and numbered across the whole statement
// rather than repeated per row), the identifier quoting (double quotes, because
// PostgreSQL folds unquoted names to lower case and MySQL folds them to the
// table's case), and the row values: pgx decodes every column into a Go value
// and database/sql renders it back to bytes for a sql.RawBytes destination, so
// a DATE arrives as "2024-02-29T00:00:00Z" and a boolean as "true". Those are
// Go's spellings, not the server's, and a dump file that carries them is neither
// like the MySQL dumps this tool writes nor like what psql shows. CellFixups is
// what puts them right.
type postgresBackend struct{}

func (postgresBackend) Engine() Engine { return EnginePG }

// pgPortDefault is what --port becomes for --engine pg when neither the flag
// nor IMEX_PORT named one. The shipped --port default is 3306 and the help text
// says so; changing that for everybody to suit one engine would be an
// incompatible change to a long-standing option, so the adjustment happens only
// when the engine was named explicitly. It is not logged: cmd/engine.go applies
// it at validation time, before the logger exists.
const pgPortDefault = 5432

func (postgresBackend) DefaultPort() int { return pgPortDefault }

// HexBlobMandatory is true whatever --default-charset says: a PostgreSQL server
// rejects a NUL in any text value (measured: `ERROR: invalid byte sequence for
// encoding "UTF8": 0x00`, SQLSTATE 22021, on the first row of a bytea column
// dumped raw under utf8mb4), and bytea is binary in every client encoding. The
// charset rule that protects MySQL therefore cannot apply here; cmd refuses
// --no-hex-blob on this engine rather than overriding the operator silently.
func (postgresBackend) HexBlobMandatory() bool { return true }

// pgClientEncoding maps the --default-charset values cmd/validate.go accepts onto
// PostgreSQL encoding names. A charset with no PostgreSQL counterpart is refused
// here, before the connection is dialled, rather than as a SET that would fail on
// every new physical connection.
var pgClientEncoding = map[string]string{
	"utf8mb4": "UTF8", "utf8": "UTF8", "utf-8": "UTF8", "utf8mb3": "UTF8",
	"latin1": "LATIN1", "iso-8859-1": "LATIN1", "819": "LATIN1",
	"gbk": "GBK", "936": "GBK", "1386": "GBK",
	"gb18030": "GB18030", "5488": "GB18030", "1392": "GB18030",
	"ascii": "SQL_ASCII", "us-ascii": "SQL_ASCII", "384": "SQL_ASCII",
}

// ClientEncoding resolves --default-charset to a PostgreSQL client_encoding.
func ClientEncoding(charset string) (string, error) {
	if enc, ok := pgClientEncoding[strings.ToLower(strings.TrimSpace(charset))]; ok {
		return enc, nil
	}
	return "", errors.Errorf("invalid --default-charset %q with --engine pg: PostgreSQL has no such client encoding (supported: utf8mb4, latin1, gbk, gb18030, ascii)", charset)
}

// statementTimeoutLiteral renders a duration the way PostgreSQL accepts it. The
// server rejects Go's own spelling — '5m0s' fails with `invalid value for
// parameter "statement_timeout": "5m0s"`, while '300000ms' and '5min' are both
// fine — so the value always goes out in milliseconds.
func statementTimeoutLiteral(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
}

// StatementTimeout maps the two socket-timeout options onto the one server-side
// deadline PostgreSQL offers. Neither lib/pq nor pgx has a per-read or per-write
// socket deadline (measured: a stalled server holds the read open regardless of
// the DSN), so without this mapping --socket-read-timeout and
// --socket-write-timeout would be silently dropped for --engine pg. The smaller
// of the two is used, which can only ever be stricter than what the user asked
// for, never looser.
func StatementTimeout(cfg *config.Config) time.Duration {
	d := cfg.SocketReadTimeout
	if cfg.SocketWriteTimeout > 0 && (d == 0 || cfg.SocketWriteTimeout < d) {
		d = cfg.SocketWriteTimeout
	}
	return d
}

// Open builds a PostgreSQL connection URL and returns the lazy *sql.DB, matching
// sql.Open's contract exactly: nothing is dialled here, so the caller's
// PingWithRetry stays the single place a connectivity failure is decided and
// reported.
//
// sslmode=disable is deliberate. It is the same promise the tool already makes
// for MySQL (requirements 8.4: transport is plaintext TCP, TLS must be
// terminated outside the tool). Left unset, pgx negotiates a TLS attempt on
// every connection and would silently upgrade a run that the documentation says
// is plaintext.
func (postgresBackend) Open(cfg *config.Config) (*sql.DB, error) {
	s, err := connectURL(cfg)
	if err != nil {
		return nil, err
	}
	// The error is returned unwrapped: every caller already wraps an Open failure
	// with "failed to connect to database", and saying it twice would change the
	// text an operator reads for an unreachable server.
	return sql.Open("pgx", s)
}

// connectURL spells the DSN Open() hands to pgx. It is separate from Open only so
// that a test can read the four parameters back: sslmode, client_encoding,
// statement_timeout and connect_timeout are the whole of what this tool promises a
// PostgreSQL session is configured with, and nothing else in the suite would notice
// one of them being dropped or re-spelled.
func connectURL(cfg *config.Config) (string, error) {
	enc, err := ClientEncoding(cfg.DefaultCharset)
	if err != nil {
		return "", err
	}
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(cfg.User, cfg.Password),
		Host:   net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Path:   cfg.Database,
	}
	q := u.Query()
	q.Set("sslmode", "disable")
	q.Set("client_encoding", enc)
	if d := StatementTimeout(cfg); d > 0 {
		q.Set("statement_timeout", statementTimeoutLiteral(d))
	}
	if cfg.ConnectTimeout > 0 {
		// connect_timeout is in whole seconds, so a sub-second deadline rounds
		// up rather than down: rounding down would make the dial fail instantly.
		secs := int64((cfg.ConnectTimeout + time.Second - 1) / time.Second)
		q.Set("connect_timeout", strconv.FormatInt(secs, 10))
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// IsReservedConnParam is false for every name, because this engine's connection
// string has no parameter the tool owns under a MySQL-shaped name: connectURL
// writes sslmode, client_encoding, statement_timeout and connect_timeout, and
// --session-var is refused on this engine before the CLI ever parses a name
// (cmd/engine.go). Answering honestly here rather than reusing MySQL's list is
// the point — a third engine must not inherit this dialect's vocabulary.
func (postgresBackend) IsReservedConnParam(string) bool { return false }

// TransientError deliberately has no table yet.
//
// PostgreSQL reports SQLSTATE strings rather than MySQL's numbers, and naming
// which of them a reconnect can heal (53300 too many clients, 57P01 admin
// shutdown, 58030 internal error) is a decision about this engine's failure
// model that has not been measured against a server here. Inventing the table
// would turn failures that end a run today into silent retries — that is a new
// capability on contract faces ③ and ⑤, not a move, and it is recorded as a
// pending item in CONTEXT.md. Answering (false, false) keeps the decision exactly
// where it was before this method existed: the neutral rules in
// internal/common/dberr.go (sockets, EOF, connection-flavoured text) decide, and
// a real connection loss on pgx arrives through those as *net.OpError or io.EOF.
func (postgresBackend) TransientError(error) (bool, bool) { return false, false }

// QuoteIdent double-quotes one identifier, doubling any double quote inside it.
// Unlike MySQL, PostgreSQL is case-sensitive once the name is quoted and folds an
// unquoted name to lower case, so quoting is not cosmetic here: it is the only
// way a table created as "Users" is addressed at all.
func (postgresBackend) QuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// ValueGroups numbers every placeholder across the whole statement. PostgreSQL
// parameters are ordinal, so a three-row, two-column batch is
// ($1,$2),($3,$4),($5,$6) — not three copies of ($1,$2).
func (b postgresBackend) ValueGroups(rowCount, ncols int) string {
	if rowCount <= 0 || ncols <= 0 {
		return ""
	}
	n := rowCount * ncols
	if n < 1000 {
		// Small batches dominate a --commit-count-sized run; building the string
		// from a slice of pre-rendered "$n" tokens is cheaper than one builder
		// per placeholder because the ordinals are reused across statements of
		// the same shape.
		return b.groupsFromTable(rowCount, ncols)
	}
	var sb strings.Builder
	next := 1
	for r := 0; r < rowCount; r++ {
		if r > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('(')
		for c := 0; c < ncols; c++ {
			if c > 0 {
				sb.WriteByte(',')
			}
			sb.WriteByte('$')
			sb.WriteString(strconv.Itoa(next))
			next++
		}
		sb.WriteByte(')')
	}
	return sb.String()
}

// pgOrdinalTable holds the "$n" spellings for the ordinals a batch can need. It
// is lazily filled and capped by maxParams (see pgMaxParams), so the worst case
// is one slice of 65536 small strings per process.
var pgOrdinalTable []string

func (b postgresBackend) groupsFromTable(rowCount, ncols int) string {
	need := rowCount*ncols + 1
	if len(pgOrdinalTable) < need {
		t := make([]string, need)
		for i := 1; i < need; i++ {
			t[i] = "$" + strconv.Itoa(i)
		}
		pgOrdinalTable = t
	}
	var sb strings.Builder
	next := 1
	for r := 0; r < rowCount; r++ {
		if r > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('(')
		for c := 0; c < ncols; c++ {
			if c > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(pgOrdinalTable[next])
			next++
		}
		sb.WriteByte(')')
	}
	return sb.String()
}

// pgMaxParams is the bind-parameter ceiling of the PostgreSQL frontend protocol:
// the parameter count in the Bind message is an Int16 — the same number MySQL
// reaches through its prepared-statement header, but there it selects a slower
// protocol path and here the statement is simply rejected. A batch over it has to
// be cut before the SQL is built.
const pgMaxParams = 65535

func (postgresBackend) BatchRowCap(ncols int) (int, int) {
	if ncols <= 0 {
		return 0, pgMaxParams
	}
	rows := pgMaxParams / ncols
	if rows < 1 {
		rows = 1
	}
	return rows, pgMaxParams
}

// BatchWords names PostgreSQL's own ceilings. The server has no
// max_allowed_packet: the equivalent is the protocol's 1 GiB message limit, and
// there is no client-side interpolation to fall back from, because pgx sends the
// statement and its parameters separately. Naming MySQL's variable here would
// point an operator at a setting that does not exist.
func (postgresBackend) BatchWords() BatchWords {
	return BatchWords{
		PacketCap:      "protocol_message_cap",
		DriverCap:      "protocol_message_cap",
		PacketUnknown:  "could not read the PostgreSQL protocol message cap, assuming conservative batch limit",
		DriverOver:     "--statement-size is above the PostgreSQL protocol message limit, the server rejects a statement that large",
		BatchSizeClamp: "lowering --batch-size to stay under the PostgreSQL parameter limit",
	}
}

// UpsertSuffix is PostgreSQL's ON CONFLICT DO UPDATE, with the two expressions the
// counters need.
//
// `xmax = 0` is true for a row the statement created and false for a row it
// rewrote, measured on 14.24 against a table holding one identical row, one
// changed row and one new one. The affected-rows number alone cannot say that — it
// counts both kinds as 1 — so without this RETURNING a run that rewrote every row
// would report that it inserted every row.
//
// The WHERE guard is what makes the third category exist. A DO UPDATE with no
// guard fires even when every value already matches, and PostgreSQL writes a fresh
// row version for it (measured: xmin 202360 became 202361 and ctid (0,1) became
// (0,2) for a row inserted with its own values), so the row would be counted as an
// update and rewritten for nothing. Comparing every SET column with IS DISTINCT
// FROM — which, unlike `<>`, also tells NULL from a value rather than answering
// NULL — leaves such a row alone and returns no row for it, which is what MySQL's
// 0 affected rows means for the same row. The columns have to be qualified with
// the table: measured on 14.24, a bare `v IS DISTINCT FROM EXCLUDED.v` fails with
// `column reference "v" is ambiguous`, because the target row is in scope under its
// own names as well.
//
// An empty target means the table has no key to name, which is the same degenerate
// case MySQL reports: the clause would be a syntax error, so the caller gets an
// upsert-free INSERT and the pre-flight has already warned that repeated imports
// duplicate rows.
func (b postgresBackend) UpsertSuffix(table string, columns, conflictTarget []string) (string, string, error) {
	if len(conflictTarget) == 0 {
		return "", "", nil
	}
	var up strings.Builder
	up.WriteString(" ON CONFLICT (")
	for i, col := range conflictTarget {
		if i > 0 {
			up.WriteString(", ")
		}
		up.WriteString(b.QuoteIdent(col))
	}
	up.WriteString(") DO UPDATE SET ")
	for i, col := range columns {
		if i > 0 {
			up.WriteString(", ")
		}
		up.WriteString(b.QuoteIdent(col))
		up.WriteString(" = EXCLUDED.")
		up.WriteString(b.QuoteIdent(col))
	}
	if len(columns) > 0 {
		quotedTable := b.QuoteIdent(table)
		up.WriteString(" WHERE ")
		for i, col := range columns {
			if i > 0 {
				up.WriteString(" OR ")
			}
			up.WriteString(quotedTable)
			up.WriteString(".")
			up.WriteString(b.QuoteIdent(col))
			up.WriteString(" IS DISTINCT FROM EXCLUDED.")
			up.WriteString(b.QuoteIdent(col))
		}
	}
	return up.String(), " RETURNING (xmax = 0)", nil
}

// UpsertNeedsTarget is true here: an ON CONFLICT without a target is a syntax
// error, so the importer has to read the key columns before it can build one.
func (postgresBackend) UpsertNeedsTarget() bool { return true }

// FKCheckToggle returns empty strings. PostgreSQL has no session switch for
// referential integrity — it is a property of each constraint — so a --mode
// replace run on this engine empties the table with checks still in force and a
// row another table depends on fails with SQLSTATE 23503 instead of being
// silently detached.
func (postgresBackend) FKCheckToggle() (off, on string) { return "", "" }

// ShowDefinition has no rows: PostgreSQL has no SHOW CREATE, and the equivalents
// (pg_get_functiondef, pg_index, a view's stored definition) would have to be
// re-imported by --mode create through the SQL reader this engine also refuses.
// cmd refuses every `imex show` kind before it dials (Refusals(CmdShow)), so this
// answer is only reached by a library caller.
func (postgresBackend) ShowDefinition(string) (string, string, bool) { return "", "", false }

// SnapshotSteps opens the read transaction. PostgreSQL has no
// START TRANSACTION WITH CONSISTENT SNAPSHOT (it is a MySQL/MariaDB extension),
// but a plain BEGIN at REPEATABLE READ already gives the same guarantee: the
// snapshot is taken at the first read and every later statement sees it, which
// is what a long export needs. The isolation level is named in the BEGIN so the
// session's default cannot decide it.
func (postgresBackend) SnapshotSteps() []SnapshotStep {
	return []SnapshotStep{
		{SQL: "BEGIN ISOLATION LEVEL REPEATABLE READ", Warn: "failed to begin the consistent read"},
	}
}

// MaxConnectionsSQL reads the cluster ceiling. PostgreSQL has no @@-prefixed
// variable and SHOW is a statement rather than a function, but current_setting
// is readable by any role for a non-restricted GUC (SET log_statement is
// superuser-only and is not used here).
func (postgresBackend) MaxConnectionsSQL() string {
	return "SELECT current_setting('max_connections')::bigint"
}

// pgProtocolMessageCap is the ceiling PostgreSQL puts on any single protocol
// message: the length prefix is an int32, so a statement cannot exceed 1 GiB. It
// is not a tunable setting the way max_allowed_packet is, which is why reading it
// costs no round-trip; the batch budget still gets the same 9/10 safety margin
// through batchByteLimit, so a batch stays under 1 GiB by construction.
const pgProtocolMessageCap int64 = 1 << 30

func (postgresBackend) PacketCeiling(context.Context, *sql.DB) (int64, error) {
	return pgProtocolMessageCap, nil
}

// DriverStatementCap equals the protocol ceiling: pgx sends the statement and its
// parameters as separate Bind values, so there is no client-side interpolation
// ceiling to fall back from, and no prepared-statement penalty for a large
// --statement-size either.
func (postgresBackend) DriverStatementCap() int64 { return pgProtocolMessageCap }

// The import-side catalogue probes below share one scoping rule, and it is the
// one thing PostgreSQL's two-level namespace forces: --database is already spent
// as the DSN database (a connection can only be in one), so it cannot also name a
// schema. Each probe therefore binds the database as a confirmation of where the
// connection is (table_catalog / current_database()) and takes the schema from the
// connection's own search_path via current_schema(). A table outside that schema is
// reached by giving the role a search_path, which is server-side configuration and
// needs no new option here.
//
// Note the lower case throughout: the standard views' columns are folded, so
// upper-casing them the way the MySQL statements do would change the meaning.
// TableExistsSQL asks information_schema, which PostgreSQL implements as the
// SQL-standard view over the catalogue.
func (postgresBackend) TableExistsSQL() string {
	return "SELECT COUNT(*) FROM information_schema.tables" +
		" WHERE table_catalog = $1 AND table_schema = current_schema() AND table_name = $2"
}

// TargetColumnCheckSQL compares on UPPER because the caller upper-cases the names it
// binds, and a quoted PostgreSQL identifier can be any case at all.
func (b postgresBackend) TargetColumnCheckSQL(n int) string {
	return "SELECT column_name FROM information_schema.columns" +
		" WHERE table_catalog = $1 AND table_schema = current_schema() AND table_name = $2" +
		" AND UPPER(column_name) IN (" + b.PlaceholderList(3, n) + ")"
}

func (postgresBackend) ColumnListSQL() string {
	return "SELECT UPPER(column_name) FROM information_schema.columns" +
		" WHERE table_catalog = $1 AND table_schema = current_schema() AND table_name = $2"
}

// ColumnTypesSQL folds both sides with LOWER rather than relying on a
// case-insensitive collation: MySQL's information_schema comparison is
// case-insensitive by default, so the importer's file-header names reach this query
// as written, and a dump with `Name` against a column created as `name` must still
// resolve.
func (b postgresBackend) ColumnTypesSQL(n int) string {
	return "SELECT column_name, data_type FROM information_schema.columns" +
		" WHERE table_catalog = $1 AND table_schema = current_schema() AND table_name = $2" +
		" AND LOWER(column_name) IN (" + b.lowerRange(3, n) + ")"
}

// GeneratedColumnsSQL lists the columns the server computes. PostgreSQL has two
// independent mechanisms with the same consequence for an INSERT: a generated
// column (attgenerated 's' stored / 'v' virtual) and an ALWAYS identity column,
// which rejects a supplied value outright (428C9). Both come back here so the
// INSERT omits both. BY-DEFAULT identities are deliberately not included: those
// accept an explicit value, which is what a dump restore of a populated id
// column needs.
func (postgresBackend) GeneratedColumnsSQL() string {
	return `SELECT UPPER(a.attname)
FROM pg_catalog.pg_attribute a
JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relname = $2 AND n.nspname = current_schema() AND current_database() = $1
  AND (a.attgenerated <> '' OR a.attidentity = 'a')`
}

func (postgresBackend) UniqueKeyColumnsSQL() string {
	return `SELECT COUNT(*)
FROM pg_catalog.pg_constraint con
JOIN pg_catalog.pg_class c ON c.oid = con.conrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relname = $2 AND n.nspname = current_schema() AND current_database() = $1
  AND con.contype IN ('p', 'u')`
}

// UniqueKeyIndexColumnsSQL unwinds the same constraints one row per key column,
// with the primary key's rows first and a primary-key flag MySQL's catalogue can
// answer the same way: an ON CONFLICT target has to be named, and the order is what
// lets the caller take the primary key without a second pass.
//
// Only constraints appear here. A standalone unique *index* — the same effect,
// created with CREATE UNIQUE INDEX instead of a constraint clause — is a valid
// conflict target to PostgreSQL but has no pg_constraint row, and
// information_schema's equivalent does not list it either, so a table whose only
// unique key is such an index reads as "no key" on both engines.
func (postgresBackend) UniqueKeyIndexColumnsSQL() string {
	return `SELECT con.conname,
       CASE WHEN con.contype = 'p' THEN 1 ELSE 0 END,
       a.attname
FROM pg_catalog.pg_constraint con
JOIN pg_catalog.pg_class c ON c.oid = con.conrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN LATERAL unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord) ON true
JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
WHERE c.relname = $2 AND n.nspname = current_schema() AND current_database() = $1
  AND con.contype IN ('p', 'u')
ORDER BY (con.contype = 'p') DESC, con.conname, k.ord`
}

// lowerRange renders "LOWER($3),LOWER($4)" for n parameters beginning at `first`.
func (postgresBackend) lowerRange(first, n int) string {
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		parts = append(parts, "LOWER($"+strconv.Itoa(first+i)+")")
	}
	return strings.Join(parts, ",")
}

// PlaceholderList renders "$3,$4,$5" for count parameters beginning at ordinal
// start. PostgreSQL numbers its parameters, so an IN list has to know where it
// sits in the statement: the catalogue probes bind the database and the table as
// $1 and $2, and their column list therefore starts at $3.
func (postgresBackend) PlaceholderList(start, count int) string {
	parts := make([]string, 0, count)
	for i := 0; i < count; i++ {
		parts = append(parts, "$"+strconv.Itoa(start+i))
	}
	return strings.Join(parts, ",")
}

func (b postgresBackend) ColumnKinds(colTypes []*sql.ColumnType) []common.ColumnKind {
	kinds := make([]common.ColumnKind, len(colTypes))
	for i, ct := range colTypes {
		kinds[i] = b.MetadataKind(ct.DatabaseTypeName())
	}
	return kinds
}

func (b postgresBackend) BinaryFlags(colTypes []*sql.ColumnType) []bool {
	flags := make([]bool, len(colTypes))
	for i, ct := range colTypes {
		flags[i] = b.MetadataBinary(ct.DatabaseTypeName())
	}
	return flags
}

func (postgresBackend) MetadataKind(dataType string) common.ColumnKind {
	return ClassifyPostgresType(dataType)
}

func (postgresBackend) MetadataBinary(dataType string) bool {
	return IsPostgresBinaryType(dataType)
}

// pgTypesToMySQLVocabulary maps a PostgreSQL type name onto the MySQL word that
// classifyColumnKind already understands, so the DEL/Parquet formatting
// rules keep working from one vocabulary. Two spellings are accepted for each
// type because the driver's DatabaseTypeName ("INT4", "BYTEA") and
// information_schema's data_type ("integer", "bytea") differ; both reach this
// table. A type that is absent classifies as KindUnknown, which means text
// passthrough — the safe answer for anything the tool does not know.
var pgTypesToMySQLVocabulary = map[string]string{
	"INT2": "SMALLINT", "SMALLINT": "SMALLINT",
	"INT4": "INT", "INTEGER": "INT", "INT": "INT", "MEDIUMINT": "INT",
	"INT8": "BIGINT", "BIGINT": "BIGINT",
	"SERIAL2": "SMALLINT", "SERIAL4": "INT", "SERIAL8": "BIGINT",
	"SMALLSERIAL": "SMALLINT", "SERIAL": "INT", "BIGSERIAL": "BIGINT",
	"NUMERIC": "DECIMAL", "DECIMAL": "DECIMAL",
	"MONEY": "DECIMAL", "MONEY[]": "",
	"FLOAT4": "DOUBLE", "REAL": "FLOAT",
	"FLOAT8": "DOUBLE", "DOUBLE PRECISION": "DOUBLE",
	"DATE": "DATE",
	"TIME": "TIME", "TIME WITHOUT TIME ZONE": "TIME", "TIMETZ": "", "TIME WITH TIME ZONE": "",
	"TIMESTAMP": "DATETIME", "TIMESTAMP WITHOUT TIME ZONE": "DATETIME",
	"TIMESTAMPTZ": "TIMESTAMP", "TIMESTAMP WITH TIME ZONE": "TIMESTAMP",
	"BOOL": "TINYINT", "BOOLEAN": "TINYINT",
	"YEAR": "YEAR",
}

// ClassifyPostgresType maps a PostgreSQL type name onto the tool's column kinds.
func ClassifyPostgresType(name string) common.ColumnKind {
	up := strings.ToUpper(strings.TrimSpace(name))
	mysqlName, ok := pgTypesToMySQLVocabulary[up]
	if !ok || mysqlName == "" {
		return common.KindUnknown
	}
	return classifyColumnKind(mysqlName)
}

// pgBinaryTypes are the PostgreSQL types whose value is not printable text, so
// they take the same path through the writers as a MySQL BLOB does: BYTE_ARRAY
// in Parquet, and the --hex-blob rule in DEL and SQL.
//
// uuid, json and xml stay out of the list: their driver text is already valid
// ASCII and re-imports as itself.
//
// bit and varbit are out, against the assumption this list once recorded. What
// pgx hands back for a bit column is the ASCII bit string, not a packed byte
// string — measured on 14.24: `B'10100101'` comes through as the eight characters
// "10100101", which the hex rule then wrote out as 0x3130313030313031 (18 bytes
// for a value the server prints in 8, and 34 bytes for a bit(16)). Text is both
// what the server prints and what its bit input accepts, so a bit column now
// leaves and returns as its own spelling.
var pgBinaryTypes = map[string]bool{
	"BYTEA": true,
	// PostGIS types are named here although the extension is not a dependency:
	// where it exists its text output is WKB hex, which no text column takes.
	"GEOMETRY": true, "GEOGRAPHY": true, "TID": true, "PG_LSN": true,
	"INTERVAL": false,
}

// IsPostgresBinaryType reports whether a PostgreSQL type name denotes binary
// content.
func IsPostgresBinaryType(name string) bool {
	return pgBinaryTypes[strings.ToUpper(strings.TrimSpace(name))]
}

// CellFixups returns one rewriter per column, nil where the driver text is
// already what the dump should carry. Measured values that need it: DATE
// "2024-02-29T00:00:00Z" -> "2024-02-29", TIMESTAMP
// "2024-02-29T10:00:00.123456Z" -> "2024-02-29 10:00:00.123456", TIMESTAMPTZ
// "2024-02-29T07:00:00.123456Z" -> "2024-02-29 07:00:00.123456+00" (pgx converts
// to UTC, so the offset is written out rather than dropped), BOOL
// "true"/"false" -> "t"/"f".
//
// numeric arrives as the server's own decimal text and bytea as the decoded
// bytes, so neither needs a fixup; a float8 keeps Go's shortest formatting,
// which matches PostgreSQL except for the two special values, where Go's "+Inf"
// is not PostgreSQL's "Infinity" (documented in requirements 2.4).
func (postgresBackend) CellFixups(colTypes []*sql.ColumnType) []func([]byte) []byte {
	fixups := make([]func([]byte) []byte, len(colTypes))
	for i, ct := range colTypes {
		switch strings.ToUpper(strings.TrimSpace(ct.DatabaseTypeName())) {
		case "BOOL", "BOOLEAN":
			fixups[i] = pgBoolText
		case "DATE":
			fixups[i] = pgTimeText("2006-01-02")
		case "TIMESTAMP", "TIMESTAMP WITHOUT TIME ZONE":
			fixups[i] = pgTimeText("2006-01-02 15:04:05.999999")
		case "TIMESTAMPTZ", "TIMESTAMP WITH TIME ZONE":
			fixups[i] = pgTimeText("2006-01-02 15:04:05.999999-07")
		case "FLOAT4", "REAL":
			fixups[i] = pgFloatText(32)
		case "FLOAT8", "DOUBLE PRECISION":
			fixups[i] = pgFloatText(64)
		}
	}
	return fixups
}

// pgFloatText re-renders a float the way PostgreSQL does. Two differences are
// measured: a REAL (float4) arrives widened to float64, so 3.14 comes through as
// "3.140000104904175" where the server prints "3.14" — reformatting at 32-bit
// shortest precision gets it back and stays lossless, because the widened value
// still parses to the same float32. And Go's spellings for the two infinities,
// "+Inf" and "-Inf", are not PostgreSQL's "Infinity" and "-Infinity" (NaN
// happens to match). Every other float8 text matches: PostgreSQL and Go both
// emit the shortest round-tripping digits, measured over 1e+15, 1e+16, 1e-06,
// -1.5e-08, 0.1 and 12345678901234568.
func pgFloatText(bits int) func([]byte) []byte {
	return func(v []byte) []byte {
		f, err := strconv.ParseFloat(string(v), bits)
		if err != nil {
			return v
		}
		switch {
		case math.IsInf(f, 1):
			return []byte("Infinity")
		case math.IsInf(f, -1):
			return []byte("-Infinity")
		case math.IsNaN(f):
			return []byte("NaN")
		}
		return []byte(strconv.FormatFloat(f, 'g', -1, bits))
	}
}

// pgBoolText renders a Go boolean the way PostgreSQL does: one character, and
// the same one a t/f column reads back from.
func pgBoolText(v []byte) []byte {
	switch string(v) {
	case "true":
		return []byte("t")
	case "false":
		return []byte("f")
	}
	return v
}

// pgTimeText parses the RFC3339 text database/sql produces for a time.Time and
// re-emits it in PostgreSQL's own layout. A value that does not parse is passed
// through untouched, which is exactly right for the two words PostgreSQL uses
// for out-of-range dates: infinity and -infinity.
func pgTimeText(layout string) func([]byte) []byte {
	return func(v []byte) []byte {
		t, err := time.Parse(time.RFC3339Nano, string(v))
		if err != nil {
			return v
		}
		return []byte(t.Format(layout))
	}
}
