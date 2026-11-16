package config

import "time"

// ImportMode represents the import operation mode.
type ImportMode string

const (
	ImportModeInsert       ImportMode = "insert"
	ImportModeInsertUpdate ImportMode = "insert_update"
	ImportModeReplace      ImportMode = "replace"
)

// SQLDialect is the value of --sql-dialect: the SQL grammar a `-f sql` dump is
// written in. The point is portability, not which server the run connects to, so
// the type lives next to the format constants rather than next to backend.Engine,
// and a MySQL source may deliberately emit a PostgreSQL dump.
type SQLDialect string

const (
	// SQLDialectAuto (the default) resolves to the running engine's own dialect.
	// A MySQL engine keeps the historical MySQL grammar byte for byte, so an
	// existing run changes nothing until the option is named.
	SQLDialectAuto SQLDialect = "auto"
	// SQLDialectMySQL writes the MySQL grammar: backquoted identifiers, 0x hex
	// literals and MySQL backslash escaping.
	SQLDialectMySQL SQLDialect = "mysql"
	// SQLDialectPG writes the PostgreSQL grammar: double-quoted identifiers,
	// '\x..'::bytea for binary, E'..' for strings carrying control characters
	// and true/false for a boolean value.
	SQLDialectPG SQLDialect = "pg"
	// SQLDialectDM writes the Dameng (Oracle-compat) grammar: double-quoted
	// identifiers, HEXTORAW('..') for binary, and SQL-standard string literals
	// where the single quote is doubled and a backslash stays literal (Oracle
	// has no backslash escape). Specifics pinned by live DM8 disql probes.
	SQLDialectDM SQLDialect = "dm"
)

// ValidSQLDialect reports whether name is one --sql-dialect accepts.
// cmd/validate.go turns a false into a usage error; the writer treats an
// unregistered value defensively by resolving it to MySQL, never to PG.
func ValidSQLDialect(name string) bool {
	switch SQLDialect(name) {
	case SQLDialectAuto, SQLDialectMySQL, SQLDialectPG, SQLDialectDM:
		return true
	default:
		return false
	}
}

// HeaderMode is --header, shared by export, import, list, show and exec. "on"
// writes/reads the column-name row, "off" omits it. One spelling now covers
// both a DEL file's first record and a result set's header; the effective value
// the writers read stays the bool NoColNames, resolved by HeaderEnabled.
type HeaderMode string

const (
	HeaderOn  HeaderMode = "on"
	HeaderOff HeaderMode = "off"
)

// BlobEncoding is --blob-encoding: how binary columns are written in the DEL
// and SQL text formats. It replaces the old --hex-blob / --no-hex-blob pair,
// which was two mutually exclusive booleans for one three-way choice.
//
//   - auto (the default): let the engine and charset decide. An engine whose
//     text protocol cannot carry a NUL (pg, dm) always gets hex; otherwise hex
//     is enabled only when --default-charset is not byte-safe.
//   - hex: force 0x hexadecimal literals.
//   - raw: force raw binary passthrough. It may corrupt data under a charset
//     that cannot carry the bytes, and pg/dm refuse it outright.
type BlobEncoding string

const (
	BlobEncodingAuto BlobEncoding = "auto"
	BlobEncodingHex  BlobEncoding = "hex"
	BlobEncodingRaw  BlobEncoding = "raw"
)

// LogLevel represents the logging level.
type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

// Pipeline tuning defaults.
//
// DefaultBatchSize is the number of rows handed from the reader goroutine to
// the writer in one batch. DefaultPipelineBuffer is the depth of that hand-off
// channel, measured in batches (0 means synchronous hand-off).
//
// These are *proportional* controls over the in-flight backlog, not a hard byte
// ceiling: rows currently reachable from the Go heap, the driver's read buffer
// (sized by the server's max_allowed_packet) and, for the SQL import path, the
// materialised body of one INSERT statement all live outside this budget. For a
// process-level soft ceiling use the Go runtime's GOMEMLIMIT environment
// variable.
const (
	DefaultBatchSize      = 1000
	DefaultPipelineBuffer = 10
)

// DefaultStatementSize is the default max bytes per generated INSERT statement
// (8 MiB). Kept well below typical max_allowed_packet settings so the produced
// file can be replayed by stock clients while bounding the import-side
// per-statement materialisation spike.
const DefaultStatementSize = 8 << 20

// Parquet page-compression codecs accepted by --parquet-compression. The default
// is "none": the option only describes how the Parquet pages are compressed, and
// leaving it alone keeps the product bytes of a Parquet export unchanged.
const (
	ParquetCompressionNone   = "none"
	ParquetCompressionSnappy = "snappy"
	ParquetCompressionZstd   = "zstd"
)

// ValidParquetCompression reports whether the codec name is one the Parquet
// writer can apply. cmd/validate.go turns a false into a usage error, and the
// writer turns it into an error instead of silently writing an uncompressed file
// under a name that promised otherwise.
func ValidParquetCompression(s string) bool {
	switch s {
	case ParquetCompressionNone, ParquetCompressionSnappy, ParquetCompressionZstd:
		return true
	}
	return false
}

// DefaultCharset is used for both the connection and the text file formats when
// --default-charset is not given.
const DefaultCharset = "utf8mb4"

// Connection timeout defaults, aligned with the JDBC-equivalent names exposed
// on the command line.
const (
	DefaultConnectTimeout = 5 * time.Second
	DefaultSocketTimeout  = 5 * time.Minute
)

// DefaultProgressInterval is how often a running export or import writes its
// periodic progress record. --progress-interval overrides it.
const DefaultProgressInterval = 5 * time.Second

// MinProgressInterval is the lowest --progress-interval the CLI accepts. The
// record is one line of ~170 bytes, and a ticker only fires as fast as its
// consumer can drain it: at --progress-interval 1ns a 10-row import that took
// 4ms produced 780 progress records and 132 KB of log (8 records, 1.6 KB at
// 1ms), so the option that spaces a log could also delete it. 100ms keeps the
// heartbeat slower than the work it reports: a 200,000-row import runs roughly
// 200 batches in 1.4s, i.e. ~7ms per batch, so a 10ms floor could put one
// progress line next to every batch (measured on the local MariaDB test bed).
const MinProgressInterval = 100 * time.Millisecond

// DelModifier holds DEL format modifier options.
type DelModifier struct {
	CharDel      string // string delimiter (wraps string values); empty = bare fields
	ColDel       string // column delimiter
	RowDel       string // row delimiter
	Escape       string // escape character; empty = no escaping (bare values)
	TimestampFmt string // timestamp format
	DateFmt      string // date format
	NoColNames   bool   // do not output/read column names (effective --header value)
	Header       string // --header: on or off; folded into NoColNames by cmd
}

// Config holds all connection and common options.
//
// SSL/TLS and the OceanBase --tenant option are intentionally absent: the CLI
// exposes only host/port, so there is no DSN to carry driver TLS settings,
// and --session-var rejects the driver's TLS parameters. Encrypted
// transport must be terminated outside the tool (for example a local TLS
// proxy). The OB tenant is still expressible through the
// `user@tenant#cluster` username. Numeric formatting always follows the MySQL
// wire text (decimal point '.', no leading-zero stripping), so those DB2-era
// modifiers are gone as well.
type Config struct {
	// Engine selects the backend the connection options are sent to: "mysql",
	// "pg" or "dm". On the CLI it is required — -E/--engine or IMEX_ENGINE must
	// supply it, and cmd/engine.go rejects an empty value as a usage error. The
	// library-level defaultConfig still seeds "mysql" for callers that build a
	// Config by hand; cmd/flags_common.go registers the flag with an empty
	// default so that seed is overwritten before any CLI run resolves a backend.
	// See internal/backend for what each engine does with the rest of this struct.
	Engine string

	Host string
	Port int
	// PortSpec is the raw --port / IMEX_PORT value. It is a string because the
	// option accepts a comma-separated list; the resolved first port is Port.
	// An empty PortSpec means the caller built Config by hand (or IMEX_PORT
	// supplied one plain number), in which case Port alone is the value.
	PortSpec string
	// Addrs is the resolved endpoint list. Nil means a single address: Host and
	// Port are the whole story, exactly as before multi-address support. See
	// internal/config/addrs.go and cmd/addrs.go.
	Addrs    []Addr
	User     string
	Password string
	Database string
	LogLevel LogLevel

	// DefaultCharset governs both the connection charset and the encoding of
	// the DEL/SQL output file. The two are deliberately tied together: the
	// file is written in exactly the charset the connection decodes.
	DefaultCharset string

	ConnectTimeout     time.Duration // TCP connect deadline
	SocketReadTimeout  time.Duration // per-read socket deadline
	SocketWriteTimeout time.Duration // per-write socket deadline

	// ProgressInterval is how often a running job writes its periodic progress
	// record (--progress-interval). Non-positive values resolve to
	// DefaultProgressInterval; see EffectiveProgressInterval.
	ProgressInterval time.Duration

	SessionVars []string          // raw --session-var values ("name=value")
	Vars        map[string]string // parsed session vars, SQL-literal-quoted (nil when unset)
}

// ExportConfig holds export-specific options.
type ExportConfig struct {
	Config

	Output string
	Format string // "del", "sql" or "parquet"
	Query  string
	// QueryFile is the path named by a "-q @file" value whose contents become
	// Query. Empty when the statement came inline; by the time the exporter runs,
	// RunE has read the file into Query and this field only carries the origin
	// for the configuration record.
	QueryFile string

	// NoOverwrite refuses to overwrite an existing output file. The default is
	// to overwrite (os.Create truncates).
	NoOverwrite bool

	DelModifier DelModifier

	// SQL format options
	StatementSize int // max bytes per generated INSERT statement
	// ExtendedInsert merges rows into multi-row INSERT statements bounded by
	// StatementSize. The default is false: one row is one INSERT statement, because
	// the import side re-batches rows by BatchSize whatever the file looks like
	// (verified against the server's general log), so merging on the export side
	// only inflates the file and makes the reader materialise a whole statement.
	ExtendedInsert bool
	// SQLDialect is --sql-dialect: the SQL grammar the writer emits for -f sql
	// ("auto", "mysql", "pg"). It is deliberately independent of the connected
	// engine, so a MySQL run can produce a dump meant to be replayed on
	// PostgreSQL. "auto" resolves to the engine's own dialect.
	SQLDialect string
	// No DDL fields here on purpose: a SQL export is data-only, and table
	// definitions are read by `imex show tab` instead. The SQL import reader still
	// skips a CREATE TABLE line a foreign dump carries, but it no longer replays
	// DDL: imex moves rows into an existing table, and creating the schema is the
	// job of the engine's own dump tool or your migrations.

	// ParquetCompression is the page-compression codec for --format parquet
	// (none, snappy or zstd). Only the Parquet writer reads it; the DEL and SQL
	// writers ignore the value, and cmd/validate.go rejects it for them.
	ParquetCompression string

	// BlobEncoding is --blob-encoding, the single three-way choice (auto/hex/raw)
	// for binary columns in DEL/SQL. It replaced the mutually exclusive
	// --hex-blob / --no-hex-blob boolean pair. effectiveHexBlob resolves it
	// together with the engine's HexBlobMandatory() and the charset rule; an
	// empty value (a caller building ExportConfig by hand) behaves as auto.
	BlobEncoding string

	// Pipeline hand-off controls (see the constants above).
	BatchSize      int
	PipelineBuffer int

	// Parallel export options
	Threads  int    // number of parallel export threads (1 = no parallelism)
	ShardKey string // column name for sharding (auto-detect primary key if empty; only exportParallel reads it — a single-threaded run warns that it is ignored)

	// SnapshotConsistency is --snapshot-consistency: whether each read connection
	// first opens a repeatable-read consistent snapshot with the engine's own
	// statements (MySQL: SET TRANSACTION ISOLATION LEVEL REPEATABLE READ then
	// START TRANSACTION WITH CONSISTENT SNAPSHOT; PostgreSQL and GaussDB: BEGIN
	// ISOLATION LEVEL REPEATABLE READ; Dameng: SET TRANSACTION ISOLATION LEVEL
	// SERIALIZABLE). The default is false: the read that follows is one statement,
	// and the server fixes that statement's snapshot at its start, so on the
	// single-threaded path the pair bought nothing, and on the parallel one each
	// shard connection still took its own snapshot at its own start time. Sending
	// them also leaves a read transaction open for the whole run, which is what
	// holds back InnoDB purge and blocks vacuum. A caller that wants the historical
	// behaviour, or wants the level pinned against a session default of READ
	// COMMITTED, turns it on.
	SnapshotConsistency bool
}

// ImportConfig holds import-specific options.
type ImportConfig struct {
	Config

	Input   string
	Format  string // "del", "sql" or "parquet"
	Mode    ImportMode
	Table   string
	Columns string // Target column list (comma-separated, one per input column) or a name mapping (file_col=table_col,...; a subset is allowed)

	DelModifier DelModifier

	// Import-specific DEL options
	SkipCount   int
	RowCount    int
	CommitCount int

	// Pipeline hand-off controls (see the constants above).
	BatchSize      int
	PipelineBuffer int

	// StatementSize is an explicit ceiling, in bytes, for each INSERT statement
	// the importer builds. Import batching is row-count driven plus a byte budget
	// derived from the server's max_allowed_packet (see batchcap.go); this option
	// lowers that budget further and never raises it. 0 (the default) keeps the
	// server-derived budget alone, so an ordinary run changes nothing. Unlike the
	// export side, where the same name bounds a merged statement, this cannot
	// split anything: a single row larger than the budget still goes out as its
	// own statement.
	StatementSize int

	// Parallel import options
	ImportThreads int // number of parallel import threads (1 = no parallelism)
}

func defaultDelModifier() DelModifier {
	return DelModifier{
		CharDel:      `"`,
		ColDel:       ",",
		RowDel:       "\n",
		Escape:       `"`,
		TimestampFmt: "yyyy-MM-dd HH:mm:ss",
		DateFmt:      "yyyy-MM-dd",
		Header:       string(HeaderOn),
	}
}

func defaultConfig() Config {
	return Config{
		Engine:             "mysql",
		Host:               "127.0.0.1",
		Port:               3306,
		User:               "root",
		LogLevel:           LogLevelInfo,
		DefaultCharset:     DefaultCharset,
		ConnectTimeout:     DefaultConnectTimeout,
		SocketReadTimeout:  DefaultSocketTimeout,
		SocketWriteTimeout: DefaultSocketTimeout,
		ProgressInterval:   DefaultProgressInterval,
	}
}

// EffectiveProgressInterval is the interval the progress ticker runs on. A
// library caller that left the field unset gets the shipped default rather than
// a non-positive duration, which time.NewTicker rejects by panicking; the CLI
// rejects such values earlier still, as a usage error that exits 2.
func (c *Config) EffectiveProgressInterval() time.Duration {
	if c.ProgressInterval > 0 {
		return c.ProgressInterval
	}
	return DefaultProgressInterval
}

// NewExportConfig returns an ExportConfig with default values.
func NewExportConfig() *ExportConfig {
	return &ExportConfig{
		Config:             defaultConfig(),
		Format:             "del",
		DelModifier:        defaultDelModifier(),
		StatementSize:      DefaultStatementSize,
		ParquetCompression: ParquetCompressionNone,
		SQLDialect:         string(SQLDialectAuto),
		BlobEncoding:       string(BlobEncodingAuto),
		Threads:            1,
		BatchSize:          DefaultBatchSize,
		PipelineBuffer:     DefaultPipelineBuffer,
	}
}

// NewImportConfig returns an ImportConfig with default values.
func NewImportConfig() *ImportConfig {
	return &ImportConfig{
		Config:         defaultConfig(),
		Format:         "del",
		Mode:           ImportModeInsert,
		DelModifier:    defaultDelModifier(),
		CommitCount:    1000,
		BatchSize:      DefaultBatchSize,
		PipelineBuffer: DefaultPipelineBuffer,
		ImportThreads:  1,
	}
}

// HexBlobForCharset reports whether binary columns must be written as 0x
// hexadecimal literals for the given charset. utf8mb4 (and its aliases) encode
// byte-for-byte identically to the wire text, so raw passthrough is safe; every
// other charset — notably gbk/gb18030, whose trail bytes can equal 0x5C or 0x27
// — can corrupt binary data in transit, so hex is forced on.
func HexBlobForCharset(charset string) bool {
	switch charset {
	case "utf8mb4", "utf8", "UTF8MB4", "UTF8":
		return false
	default:
		return true
	}
}
