package backend

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"

	_ "gitee.com/chunanyong/dm"

	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

// damengBackend drives Dameng DM8 in Oracle-compatible mode over the Dameng Go
// driver. Only Oracle mode is in scope: the driver is opened with
// compatibleMode=oracle and the catalogue queries read the Oracle-mode system
// views (SYS.DBA_*). Placeholders are positional `?` as requested.
type damengBackend struct{}

func (damengBackend) Engine() Engine { return EngineDM }

// Open builds the Dameng DSN and returns an unopened *sql.DB. The DSN always
// requests Oracle-compatible mode and binds --database as the initial schema.
func (damengBackend) Open(cfg *config.Config) (*sql.DB, error) {
	dsn, err := buildDamengDSN(cfg)
	if err != nil {
		return nil, err
	}
	return sql.Open("dm", dsn)
}

// IsReservedConnParam reports DSN parameter names that the driver or this tool
// owns. --session-var cannot override them.
func (damengBackend) IsReservedConnParam(name string) bool {
	return reservedDamengDSNParams[name]
}

// QuoteIdent double-quotes one identifier. Unquoted identifiers are folded to
// upper case by the server, so preserving the original case requires quotes;
// internal double quotes are doubled.
func (damengBackend) QuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// ValueGroups renders `?` placeholders the same way MySQL does: positional and
// repeated per row.
func (damengBackend) ValueGroups(rowCount, ncols int) string {
	if rowCount <= 0 || ncols <= 0 {
		return ""
	}
	group := "(" + strings.TrimSuffix(strings.Repeat("?,", ncols), ",") + ")"
	return group + strings.Repeat(","+group, rowCount-1)
}

// PlaceholderList renders a flat list of `?` markers.
func (damengBackend) PlaceholderList(start, count int) string {
	_ = start // positional `?` markers do not carry an ordinal.
	if count <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

// BatchRowCap mirrors MySQL: a prepared-statement parameter count is encoded as
// an unsigned 16-bit integer, so the practical ceiling is 65535 placeholders.
func (damengBackend) BatchRowCap(ncols int) (int, int) {
	if ncols <= 0 {
		return 0, damengMaxParams
	}
	rows := damengMaxParams / ncols
	if rows < 1 {
		rows = 1
	}
	return rows, damengMaxParams
}

// BatchWords names Dameng-specific ceilings. There is no exact equivalent of
// MySQL's max_allowed_packet in Oracle-mode DM, so the byte budget uses a
// conservative fallback and the wording says so.
func (damengBackend) BatchWords() BatchWords {
	return BatchWords{
		PacketCap:      "dm_packet_ceiling_fallback",
		DriverCap:      "dm_packet_ceiling_fallback",
		PacketUnknown:  "could not read a Dameng packet ceiling, assuming conservative 4 MiB batch limit",
		DriverOver:     "--statement-size is above the Dameng conservative byte ceiling, the server may reject a statement that large",
		BatchSizeClamp: "lowering --batch-size to stay under the Dameng placeholder limit",
	}
}

// UpsertSuffix is not implemented for the first cut: --mode insert_update is
// refused at the CLI, and library callers that bypass the refusal get an error.
func (damengBackend) UpsertSuffix(string, []string, []string) (string, string, error) {
	return "", "", errors.New("Dameng backend does not implement upsert in this MVP")
}

// UpsertNeedsTarget is false here only because the suffix is never built.
func (damengBackend) UpsertNeedsTarget() bool { return false }

// FKCheckToggle returns empty strings: Oracle-mode DM has no session switch
// that disables referential integrity for the current session.
func (damengBackend) FKCheckToggle() (off, on string) { return "", "" }

// SnapshotSteps opens a serializable transaction. Oracle-mode DM understands
// SET TRANSACTION ISOLATION LEVEL SERIALIZABLE as the transaction starter.
func (damengBackend) SnapshotSteps() []SnapshotStep {
	return []SnapshotStep{
		{SQL: "SET TRANSACTION ISOLATION LEVEL SERIALIZABLE", Warn: "failed to set serializable isolation"},
	}
}

// MaxConnectionsSQL reads MAX_SESSIONS from the DM instance parameters view.
func (damengBackend) MaxConnectionsSQL() string {
	return "SELECT PARA_VALUE FROM V$DM_INI WHERE PARA_NAME = 'MAX_SESSIONS'"
}

// damengPacketCeilingFallback is the assumed server statement size limit when
// the engine cannot read a tunable ceiling. It is deliberately conservative.
const damengPacketCeilingFallback int64 = 4 << 20

// damengMaxParams is the same placeholder ceiling MySQL and PostgreSQL share:
// the frontend protocol encodes the parameter count in 16 bits.
const damengMaxParams = 65535

func (damengBackend) PacketCeiling(context.Context, *sql.DB) (int64, error) {
	return damengPacketCeilingFallback, nil
}

func (damengBackend) DriverStatementCap() int64 { return damengPacketCeilingFallback }

func (damengBackend) TableExistsSQL() string {
	return "SELECT COUNT(*) FROM SYS.DBA_TABLES WHERE UPPER(OWNER) = UPPER(?) AND TABLE_NAME = ?"
}

func (b damengBackend) TargetColumnCheckSQL(n int) string {
	return "SELECT COLUMN_NAME FROM SYS.DBA_TAB_COLUMNS WHERE UPPER(OWNER) = UPPER(?) AND TABLE_NAME = ? AND UPPER(COLUMN_NAME) IN (" + b.PlaceholderList(1, n) + ")"
}

func (damengBackend) ColumnListSQL() string {
	return "SELECT UPPER(COLUMN_NAME) FROM SYS.DBA_TAB_COLUMNS WHERE UPPER(OWNER) = UPPER(?) AND TABLE_NAME = ?"
}

func (b damengBackend) ColumnTypesSQL(n int) string {
	return "SELECT COLUMN_NAME, DATA_TYPE FROM SYS.DBA_TAB_COLUMNS WHERE UPPER(OWNER) = UPPER(?) AND TABLE_NAME = ? AND UPPER(COLUMN_NAME) IN (" + b.PlaceholderList(1, n) + ")"
}

// GeneratedColumnsSQL returns a query that always produces zero rows: DM
// Oracle mode has no generated-column concept that maps cleanly to MySQL's
// EXTRA='VIRTUAL GENERATED' marker. The statement shape still binds owner and
// table.
func (damengBackend) GeneratedColumnsSQL() string {
	return "SELECT COLUMN_NAME FROM SYS.DBA_TAB_COLUMNS WHERE UPPER(OWNER) = UPPER(?) AND TABLE_NAME = ? AND 1 = 0"
}

func (damengBackend) UniqueKeyColumnsSQL() string {
	return "SELECT COUNT(DISTINCT CONSTRAINT_NAME) FROM SYS.DBA_CONSTRAINTS WHERE UPPER(OWNER) = UPPER(?) AND TABLE_NAME = ? AND CONSTRAINT_TYPE IN ('P','U')"
}

func (damengBackend) UniqueKeyIndexColumnsSQL() string {
	return `SELECT con.CONSTRAINT_NAME,
       CASE con.CONSTRAINT_TYPE WHEN 'P' THEN 1 ELSE 0 END,
       col.COLUMN_NAME
FROM SYS.DBA_CONSTRAINTS con
JOIN SYS.DBA_CONS_COLUMNS col ON con.OWNER = col.OWNER AND con.CONSTRAINT_NAME = col.CONSTRAINT_NAME
WHERE UPPER(con.OWNER) = UPPER(?) AND con.TABLE_NAME = ? AND con.CONSTRAINT_TYPE IN ('P','U')
ORDER BY CASE con.CONSTRAINT_TYPE WHEN 'P' THEN 0 ELSE 1 END, con.CONSTRAINT_NAME, col.POSITION`
}

func (b damengBackend) ColumnKinds(colTypes []*sql.ColumnType) []common.ColumnKind {
	kinds := make([]common.ColumnKind, len(colTypes))
	for i, ct := range colTypes {
		kinds[i] = b.MetadataKind(ct.DatabaseTypeName())
	}
	return kinds
}

func (b damengBackend) BinaryFlags(colTypes []*sql.ColumnType) []bool {
	flags := make([]bool, len(colTypes))
	for i, ct := range colTypes {
		flags[i] = b.MetadataBinary(ct.DatabaseTypeName())
	}
	return flags
}

func (damengBackend) MetadataKind(dataType string) common.ColumnKind {
	return classifyDamengType(dataType)
}

func (damengBackend) MetadataBinary(dataType string) bool {
	return isDamengBinaryType(dataType)
}

func (damengBackend) DefaultPort() int { return 5236 }

// HexBlobMandatory is true: Oracle-mode DM rejects a NUL byte inside a text
// value the way PostgreSQL does, so binary columns must leave as 0x hex.
func (damengBackend) HexBlobMandatory() bool { return true }

// ShowDefinition refuses every kind in the first cut.
func (damengBackend) ShowDefinition(string) (string, string, bool) { return "", "", false }

// KnownCharset reuses the MySQL charset vocabulary. The file encoding decision
// lives in the tool, and the driver does not expose a per-connection charset
// parameter that this backend sets.
func (damengBackend) KnownCharset(name string) bool {
	return mysqlCharsets[mysqlCharsetName(name)]
}

// ValidateCharset resolves every name the vocabulary accepts; the driver will
// report an invalid name at connect time.
func (damengBackend) ValidateCharset(string) error { return nil }

// CellFixups re-renders the values the DM driver hands back as Go types whose
// database/sql text is not what a dump should carry: every date/time type comes
// as a time.Time (RFC3339Nano: an ISO "T", a zone offset, nanoseconds no
// Oracle-mode literal uses) and BOOLEAN comes as a Go bool ("true"). The
// per-type layouts and the boolean normalisation live in the one Dameng type
// table (types_dameng.go) that also answers ColumnKind and binary questions, so
// a name cannot be classified without being rewritten.
func (damengBackend) CellFixups(colTypes []*sql.ColumnType) []func([]byte) []byte {
	fixups := make([]func([]byte) []byte, len(colTypes))
	for i, ct := range colTypes {
		fixups[i] = damengCellFixup(ct.DatabaseTypeName())
	}
	return fixups
}

// buildDamengDSN renders a dm:// DSN with Oracle-compatible mode always on.
func buildDamengDSN(c *config.Config) (string, error) {
	if strings.TrimSpace(c.User) == "" {
		return "", errors.New("--user is required for --engine dm")
	}
	u := url.URL{
		Scheme: "dm",
		User:   url.UserPassword(c.User, c.Password),
		Host:   fmt.Sprintf("%s:%d", c.Host, c.Port),
	}
	q := u.Query()
	q.Set("compatibleMode", "oracle")
	if c.Database != "" {
		q.Set("schema", c.Database)
	}
	if c.ConnectTimeout > 0 {
		q.Set("connectTimeout", strconv.FormatInt(c.ConnectTimeout.Milliseconds(), 10))
	}
	if c.SocketReadTimeout > 0 {
		// socketTimeout is in seconds in the Dameng driver.
		secs := int64((c.SocketReadTimeout + time.Second - 1) / time.Second)
		q.Set("socketTimeout", strconv.FormatInt(secs, 10))
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
