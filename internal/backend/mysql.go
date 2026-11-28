package backend

import (
	"context"
	"database/sql"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/pkg/errors"

	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

// mysqlBackend drives MySQL, MariaDB and OceanBase over the MySQL protocol.
//
// Every statement returned here is copied out of the code that used to build it
// inline, character for character, including its whitespace and line breaks: the
// server-side general log is the evidence that the port did not disturb an
// existing run, and a reflowed SELECT would show up in that diff for no reason.
type mysqlBackend struct{}

func (mysqlBackend) Engine() Engine { return EngineMySQL }

// Open uses the dialect's own builder (dsn_mysql.go), which is where the
// --default-charset, parseTime, interpolateParams, session-variable and timeout
// decisions live.
//
// It hands the driver a Config through mysql.NewConnector rather than a DSN
// string: FormatDSN writes User/Passwd verbatim, so a username containing ':'
// (e.g. `test@SERVICE:attest`) or a password with ':' would be re-split by
// sql.Open's DSN parser. NewConnector keeps the fields intact. dsn_mysql.go still
// imports go-sql-driver/mysql for the Config type and dberr_mysql.go for the server
// code table, and postgres.go does the same with pgx's database/sql wrapper. That
// symmetry is why internal/export and internal/import carry no driver import of
// their own — a command that opens a connection must name an engine, never a driver.
func (mysqlBackend) Open(cfg *config.Config) (*sql.DB, error) {
	driverCfg, err := buildMySQLConfig(cfg)
	if err != nil {
		return nil, err
	}
	connector, err := mysql.NewConnector(driverCfg)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(connector), nil
}

// QuoteIdent backquotes one identifier, doubling any backquote inside it so a
// name that contains the quote character cannot end the quote early. Names like
// that are legal in MySQL; until this method existed the import path wrote the
// name raw, so `a“b` produced a syntax error rather than a reference to the
// column whose name is a`b.
func (mysqlBackend) QuoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// ValueGroups renders the VALUES tail of a multi-row INSERT. MySQL takes
// positional `?` markers, so every row's group is the same string.
func (mysqlBackend) ValueGroups(rowCount, ncols int) string {
	if rowCount <= 0 || ncols <= 0 {
		return ""
	}
	group := "(" + strings.TrimSuffix(strings.Repeat("?,", ncols), ",") + ")"
	return group + strings.Repeat(","+group, rowCount-1)
}

// BatchRowCap is 65535/ncols: a prepared statement cannot carry more parameters
// than that, so a wider table batches fewer rows per statement regardless of
// --batch-size. The limit is also the hard ceiling of a MySQL prepared
// statement — the parameter count is transmitted as an unsigned 16-bit integer,
// so a batch may never carry more than 65535 `?` markers in total. A table wider
// than that still gets one row per statement rather than an empty batch: it fails
// on the server, which is honest, instead of spinning here.
func (mysqlBackend) BatchRowCap(ncols int) (int, int) {
	if ncols <= 0 {
		return 0, mysqlMaxParams
	}
	rows := mysqlMaxParams / ncols
	if rows < 1 {
		rows = 1
	}
	return rows, mysqlMaxParams
}

// batchWords are the MySQL strings the import resolver has always logged. They
// are copied verbatim from the pre-backend code because the run's log records are
// a user-visible contract; the ceiling names are the ones an operator can then
// look up in the server's own documentation.
func (mysqlBackend) BatchWords() BatchWords {
	return BatchWords{
		PacketCap:      "max_allowed_packet",
		DriverCap:      "driver_packet_cap",
		PacketUnknown:  "could not read max_allowed_packet, assuming conservative batch limit",
		DriverOver:     "--statement-size is above the driver interpolation ceiling, generated statements will fall back to server-side prepared statements",
		BatchSizeClamp: "lowering --batch-size to stay under the MySQL placeholder limit",
	}
}

// UpsertSuffix is the ON DUPLICATE KEY UPDATE clause, built exactly as the
// importer built it before this method existed. table and conflictTarget are
// unused: this is the engine whose upsert names no key, and the server decides for
// itself that a row whose values already match the file is worth 0 affected rows
// rather than rewriting it — which is why no guard is spelled here.
func (b mysqlBackend) UpsertSuffix(table string, columns, conflictTarget []string) (string, string, error) {
	var od strings.Builder
	od.WriteString(" ON DUPLICATE KEY UPDATE ")
	for i, col := range columns {
		if i > 0 {
			od.WriteString(", ")
		}
		od.WriteString(b.QuoteIdent(col))
		od.WriteString(" = VALUES(")
		od.WriteString(b.QuoteIdent(col))
		od.WriteString(")")
	}
	// No RETURNING: 1, 2 and 0 affected rows already say which of new, changed and
	// identical a row was, so reading rows back would cost a round trip's worth of
	// result bytes for nothing.
	return od.String(), "", nil
}

// UpsertNeedsTarget is false here: ON DUPLICATE KEY UPDATE is decided by the
// server, which matches whatever unique key a row collides with.
func (mysqlBackend) UpsertNeedsTarget() bool { return false }

// FKCheckToggle is the pair --mode replace has always sent. MariaDB and MySQL
// both take it as a session setting; the re-enable is issued on the way out even
// when the run failed, which is why the caller defers it.
func (mysqlBackend) FKCheckToggle() (off, on string) {
	return "SET FOREIGN_KEY_CHECKS=0", "SET FOREIGN_KEY_CHECKS=1"
}

func (mysqlBackend) SnapshotSteps() []SnapshotStep {
	return []SnapshotStep{
		{SQL: "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ", Warn: "failed to set isolation level"},
		{SQL: "START TRANSACTION WITH CONSISTENT SNAPSHOT", Warn: "failed to start consistent snapshot"},
	}
}

func (mysqlBackend) MaxConnectionsSQL() string { return "SELECT @@max_connections" }

// MySQLPacketCeilingFallback is assumed when the server value cannot be read
// (permission errors, proxies that hide SHOW VARIABLES). It is deliberately
// conservative: under-splitting batches only costs a few more round-trips, while
// over-splitting nothing would fail mid-import. The number is also the OceanBase
// tenant default for V2.1.x/V2.2.x/V3.1.x/V3.2.x (4,194,304), so the fallback
// matches a real OB server rather than an invented one. V4.x documents
// 16,777,216 instead, and this constant cannot be made version-aware: the only
// way to learn which OB a server is runs through SHOW VARIABLES as well
// (version_comment), so the same failure that forces the fallback also hides the
// version.
const MySQLPacketCeilingFallback int64 = 4 << 20

// MySQLDriverInterpolationCap is the client-side ceiling the MySQL driver applies
// to an interpolated statement. go-sql-driver/mysql defaults
// Config.MaxAllowedPacket to 64 MiB, imex does not override it
// (internal/backend/dsn_mysql.go), and once the interpolated text passes that ceiling
// the driver returns driver.ErrSkip (connection.go: "if len(buf)+4 >
// mc.maxAllowedPacket"), which makes database/sql fall back to server-side
// prepared statements — a different protocol path than the one the batching
// design targets, and measurably slower (one 80 MiB statement took 2.078s where
// the same data in 8 MiB COM_QUERY batches took 1.195s). A server with a large
// max_allowed_packet therefore gets *less* suitable batches if nothing clamps
// the derived budget.
const MySQLDriverInterpolationCap int64 = 64 << 20

func (mysqlBackend) PacketCeiling(ctx context.Context, db *sql.DB) (int64, error) {
	if db == nil {
		return MySQLPacketCeilingFallback, nil
	}
	var name string
	var value int64
	showSQL := "SHOW VARIABLES LIKE 'max_allowed_packet'"
	common.LogInteractionSQL(showSQL)
	row := db.QueryRowContext(ctx, common.TagSQL(showSQL))
	if err := row.Scan(&name, &value); err != nil {
		return MySQLPacketCeilingFallback, err
	}
	if value <= 0 {
		return MySQLPacketCeilingFallback, errors.Errorf("server reported max_allowed_packet=%d", value)
	}
	return value, nil
}

func (mysqlBackend) DriverStatementCap() int64 { return MySQLDriverInterpolationCap }

// mysqlMaxParams is the hard ceiling of a MySQL prepared statement: the parameter
// count is transmitted as an unsigned 16-bit integer, so a batch may never carry
// more than 65535 `?` markers in total.
const mysqlMaxParams = 65535

// PlaceholderList renders `count` bare `?` markers. start is ignored: MySQL
// placeholders are positional, so a marker means "the next bound value" and
// carries no number of its own.
func (mysqlBackend) PlaceholderList(start, count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func (mysqlBackend) TableExistsSQL() string {
	return "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?"
}

func (b mysqlBackend) TargetColumnCheckSQL(n int) string {
	return "SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND UPPER(COLUMN_NAME) IN (" + b.PlaceholderList(1, n) + ")"
}

func (mysqlBackend) ColumnListSQL() string {
	return "SELECT UPPER(COLUMN_NAME) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?"
}

// ColumnTypesSQL answers one type word per target column, and for BIT that word
// carries its width (measured on MariaDB 10.6.23: DATA_TYPE is the bare "bit" for
// both bit(1) and bit(8), while COLUMN_TYPE is "bit(1)"/"bit(8)"). The width is not
// decoration: a 1-bit column cannot hold the ASCII digit byte the other engine's
// text form decodes to, while a wider BIT column can, and that difference is exactly
// what decides whether a quoted "0" is a payload byte or boolean text. See
// BindFixups below. Every other column keeps reporting DATA_TYPE unchanged, so
// MetadataKind and MetadataBinary see the same vocabulary they always did.
func (b mysqlBackend) ColumnTypesSQL(n int) string {
	return "SELECT COLUMN_NAME, CASE WHEN DATA_TYPE = 'bit' THEN COLUMN_TYPE ELSE DATA_TYPE END FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME IN (" + b.PlaceholderList(1, n) + ")"
}

// GeneratedColumnsSQL reads EXTRA, the only portable generated-column marker
// across MySQL 5.7/8.0, MariaDB 10.2+ and OceanBase's MySQL mode. See
// internal/import/generated_columns.go for why GENERATION_EXPRESSION stays out
// of the select list.
func (mysqlBackend) GeneratedColumnsSQL() string {
	return `SELECT UPPER(COLUMN_NAME)
FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND EXTRA LIKE '%GENERATED%'`
}

func (mysqlBackend) UniqueKeyColumnsSQL() string {
	return `SELECT COUNT(DISTINCT INDEX_NAME)
FROM information_schema.STATISTICS
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND (NON_UNIQUE = 0 OR INDEX_NAME = 'PRIMARY')`
}

// UniqueKeyIndexColumnsSQL is the same catalogue one row per key column, with a
// primary-key flag in place of NON_UNIQUE because a unique index and a primary key
// share NON_UNIQUE = 0 and only one of them is the key an upsert means. A
// functional index has no COLUMN_NAME here, only its expression, so those rows are
// left out: a name is what a conflict target has to be built from.
func (mysqlBackend) UniqueKeyIndexColumnsSQL() string {
	return `SELECT INDEX_NAME, (INDEX_NAME = 'PRIMARY'), COLUMN_NAME
FROM information_schema.STATISTICS
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
  AND (NON_UNIQUE = 0 OR INDEX_NAME = 'PRIMARY') AND COLUMN_NAME IS NOT NULL
ORDER BY (INDEX_NAME = 'PRIMARY') DESC, INDEX_NAME, SEQ_IN_INDEX`
}

func (mysqlBackend) ColumnKinds(colTypes []*sql.ColumnType) []common.ColumnKind {
	return applyColumnKinds(colTypes)
}

func (mysqlBackend) BinaryFlags(colTypes []*sql.ColumnType) []bool {
	return binaryColumnFlags(colTypes)
}

// BooleanFlags reports none: a MySQL boolean is a tinyint(1)/BIT whose text the
// other writers already handle, so the SQL writer never needs a boolean column
// mark for this engine.
func (mysqlBackend) BooleanFlags(colTypes []*sql.ColumnType) []bool {
	return make([]bool, len(colTypes))
}

func (mysqlBackend) MetadataKind(dataType string) common.ColumnKind {
	return classifyColumnKind(dataType)
}

func (mysqlBackend) MetadataBinary(dataType string) bool {
	return isBinaryTypeName(dataType)
}

func (mysqlBackend) DefaultPort() int { return 3306 }

// HexBlobMandatory is false: MySQL's binary columns are only unsafe in a charset
// that cannot carry the bytes, which is the rule config.HexBlobForCharset applies
// to --default-charset. A byte-safe charset keeps hex off under
// --blob-encoding auto, and the dump carries the raw bytes.
func (mysqlBackend) HexBlobMandatory() bool { return false }

// CellFixups returns a nil rewriter for every column: the MySQL driver's text
// protocol already hands over the bytes the server stores.
func (mysqlBackend) CellFixups(colTypes []*sql.ColumnType) []func([]byte) []byte {
	return make([]func([]byte) []byte, len(colTypes))
}

// BindFixups answers per target column how an imported value has to reach the
// driver. Nothing is needed for a MySQL file's own values in the common shapes —
// the server parses the text it is given for DECIMAL and integer types, and a binary
// target (BIT or BLOB) is classified as binary, so the DEL reader decodes the 0x
// token to bytes and the Parquet reader hands the payload over as bytes.
//
// Three column shapes are NOT already covered, and each gets one adapter:
//
//   - A one-bit BIT target. A BIT(1) column cannot hold the ASCII digit byte: a
//     PostgreSQL or Dameng DEL / Parquet file writes that column as the text "1"/"0"
//     (measured: pg and dm both emit "1"), and the driver reads the 1-character text
//     as 0x31 = 49, which the server rejects with `Error 1406 (22001): Data too long
//     for column 'bit1'`. mysqlBindBit turns only that text into the byte payload a
//     BIT column stores.
//   - A wider BIT target (BIT(2)..BIT(64)). Here the same text is NOT provably wrong,
//     because MySQL's own file may put the payload byte in quotes: under
//     `--blob-encoding auto`/`raw` a bit(8) holding 0x30 is written as the character
//     "0" (measured: `1,"0","<0x01>"` — the 1-bit column arrives as a raw byte, the
//     8-bit one as printable text). Folding that to 0 silently rewrote 48 as 0 on the
//     same-engine round trip, which is the defect this width key closes. So a wide
//     BIT column binds text verbatim — which is also what makes the payload correct,
//     since MySQL takes a string's own bytes as the BIT value — and only a real Go
//     bool gets a byte. mysqlBindBitWide is that narrower adapter.
//   - A numeric target. A boolean column has no integer-literal spelling in the other
//     engine's text: pg's DEL and Parquet files carry "t"/"f" and its SQL dump carries
//     the word true (both measured), and each lands on a TINYINT(1) column as
//     `Error 1366 (22007): Incorrect integer value: 't' for column ...` — through a
//     placeholder MySQL parses neither the single letters nor the words.
//     mysqlBindBoolText folds them to "1"/"0".
//
// All three are deliberately keyed to strings, and the two BIT ones to width: the
// unquoted-hex form the same column carries in a MySQL file reaches the placeholder as
// []byte (decodeHexField in sql_reader.go runs first), and []byte is left exactly as it
// is. What moves is provably-broken text only — the four boolean spellings on a numeric
// column (a numeric column cannot store "t" or "true" under any sql_mode), and on a
// 1-bit column the six spellings that cannot fit as bytes. A wide BIT column keeps
// every text verbatim, so the only cost there is the cross-engine bit text form README
// 2.1.12 already routes to `--blob-encoding hex` or Parquet.
func (mysqlBackend) BindFixups(dataTypes []string) []func(interface{}) interface{} {
	fixups := make([]func(interface{}) interface{}, len(dataTypes))
	for i, dt := range dataTypes {
		switch {
		case isMySQLBitType(dt):
			if mysqlBitWidth(dt) == 1 {
				fixups[i] = mysqlBindBit
			} else {
				fixups[i] = mysqlBindBitWide
			}
		case isMySQLNumericType(dt):
			fixups[i] = mysqlBindBoolText
		}
	}
	return fixups
}

// mysqlBitWidth reads the width suffix ColumnTypesSQL supplies for a BIT column.
// A bare "bit" is MySQL's own default — bit(1) — and an unparsable suffix is treated
// the same way, because folding is only provably needed at one bit; anything a wider
// column would need is left to bind verbatim.
func mysqlBitWidth(dataType string) int {
	name := strings.ToUpper(strings.TrimSpace(dataType))
	p := strings.IndexByte(name, '(')
	if p < 0 {
		return 1
	}
	rest := strings.TrimSpace(name[p+1:])
	if q := strings.IndexByte(rest, ')'); q >= 0 {
		rest = strings.TrimSpace(rest[:q])
	}
	n := 0
	for _, r := range rest {
		if r < '0' || r > '9' {
			return 1
		}
		n = n*10 + int(r-'0')
		if n > 64 {
			return 64
		}
	}
	if n <= 1 {
		return 1
	}
	return n
}

// baseTypeName is the catalogue DATA_TYPE word on its own: case- and
// space-insensitive, with any "(n)" length suffix dropped, because
// information_schema reports TINYINT(1) and BIGINT(20) under the same base name.
func baseTypeName(dataType string) string {
	name := strings.ToUpper(strings.TrimSpace(dataType))
	if p := strings.IndexByte(name, '('); p >= 0 {
		name = strings.TrimSpace(name[:p])
	}
	return name
}

// isMySQLBitType reports a BIT target column from the catalogue DATA_TYPE word
// (information_schema.DATA_TYPE is "bit"; a length suffix, if any, is dropped).
func isMySQLBitType(dataType string) bool {
	return baseTypeName(dataType) == "BIT"
}

// isMySQLNumericType reports an integer, decimal or floating-point target column.
// It reuses the export side's one type vocabulary (classifyColumnKind) rather than
// a second list, so "is this column numeric to MySQL" has exactly one answer in
// this codebase.
func isMySQLNumericType(dataType string) bool {
	return classifyColumnKind(baseTypeName(dataType)) == common.KindNumber
}

// mysqlBindBoolText folds the boolean spellings another engine writes for a boolean
// column into the integer text a MySQL numeric target parses. Measured: a pg cell
// carrying "t" (DEL and Parquet) or the word true (SQL dump) lands on a TINYINT(1)
// column as `Error 1366 (22007): Incorrect integer value: 't' for column ...` and
// the whole batch fails — PostgreSQL's boolean text has no form the placeholder
// parses, which README 2.1.12 had recorded as a cross-engine boundary.
//
// The fold is safe by construction rather than a guess about intent: a numeric
// MySQL column cannot store any of those four spellings under any sql_mode, so
// every value this changes is one the server was about to reject. Character targets
// keep their "t" verbatim (they are not numeric columns), NULL markers never reach
// here as these words, and every other value passes through untouched.
func mysqlBindBoolText(v interface{}) interface{} {
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case []byte:
		s = string(x)
	default:
		return v
	}
	if folded, ok := boolTextFold(s); ok {
		return folded
	}
	return v
}

// boolTextFold reports the integer text one boolean spelling means, and whether the
// value was one of the four spellings at all: the two single letters pg's DEL and
// Parquet files write, and the two words its SQL dumps write. Anything else keeps
// the value — and its Go type, which decides how the driver quotes it — exactly as
// the reader produced it, so a numeric column that was already receiving usable
// text is unaffected.
func boolTextFold(s string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "T", "TRUE":
		return "1", true
	case "F", "FALSE":
		return "0", true
	}
	return "", false
}

// mysqlBindBit reduces the boolean text another engine writes for a one-bit
// column to the single byte a MySQL BIT stores. It is the BIT(1) adapter: at one
// bit the ASCII digit byte cannot fit, so the fold is the only reading left. Bytes
// (MySQL's own 0x form, once decoded, and every Parquet payload) and every other
// value pass through untouched, so this only ever fixes the pg→my / dm→my boolean
// case.
func mysqlBindBit(v interface{}) interface{} {
	switch x := v.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "1", "true", "t":
			return []byte{1}
		case "0", "false", "f":
			return []byte{0}
		}
		return x
	case bool:
		if x {
			return []byte{1}
		}
		return []byte{0}
	}
	return v
}

// mysqlBindBitWide is the adapter for BIT(2)..BIT(64), and it deliberately leaves
// every string alone. A wider column can hold the ASCII byte a printable character
// stands for, and MySQL's own DEL / SQL dump under --blob-encoding auto or raw puts
// exactly that byte in the quotes — a bit(8) holding 0x30 is written as "0" and
// measuring it back as 0 is the silent 48→0 rewrite this avoids. Binding the text
// verbatim is what MySQL itself means by it: the server takes the string's bytes as
// the BIT value, so "0" arrives as 0x30 and "AB" as 0x4142. Only a genuine Go bool,
// which no file spelling can be confused with, still becomes a byte.
func mysqlBindBitWide(v interface{}) interface{} {
	switch x := v.(type) {
	case bool:
		if x {
			return []byte{1}
		}
		return []byte{0}
	}
	return v
}
