package config

import "time"

// ImportMode represents the import operation mode.
type ImportMode string

const (
	ImportModeInsert       ImportMode = "insert"
	ImportModeInsertUpdate ImportMode = "insert_update"
	ImportModeReplace      ImportMode = "replace"
	ImportModeCreate       ImportMode = "create"
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
// periodic progress record. --progress-interval overrides it; --quiet drops the
// records altogether instead of making them rarer.
const DefaultProgressInterval = 5 * time.Second

// MinProgressInterval is the lowest --progress-interval the CLI accepts. The
// record is one line of ~170 bytes, and a ticker only fires as fast as its
// consumer can drain it, so a nanosecond-scale interval is not "very frequent
// progress" but an unbounded write loop: a 10-row import that took 4ms produced
// 780 progress records and 132 KB of log at --progress-interval 1ns (8 records,
// 1.6 KB at 1ms). A long run at that setting writes gigabytes. The floor is well
// below any interval that is useful as a heartbeat and above the rate at which
// the log can be written.
const MinProgressInterval = 10 * time.Millisecond

// DelModifier holds DEL format modifier options.
type DelModifier struct {
	CharDel      string // string delimiter (wraps string values)
	ColDel       string // column delimiter
	RowDel       string // row delimiter
	Escape       string // escape character
	NoEscape     bool   // disable escaping
	TimestampFmt string // timestamp format
	DateFmt      string // date format
	NoCharDel    bool   // do not use string delimiter
	NoColNames   bool   // do not output/read column names
}

// Config holds all connection and common options.
//
// SSL/TLS and the OceanBase --tenant option are intentionally absent: the CLI
// exposes only host/port/socket, so there is no DSN to carry driver TLS
// settings, and --session-var rejects the driver's TLS parameters. Encrypted
// transport must be terminated outside the tool (for example a local TLS
// proxy). The OB tenant is still expressible through the
// `user@tenant#cluster` username. Numeric formatting always follows the MySQL
// wire text (decimal point '.', no leading-zero stripping), so those DB2-era
// modifiers are gone as well.
type Config struct {
	Host     string
	Port     int
	User     string
	Password string
	Socket   string
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
	// QueryFile is a path passed to --query-file whose contents become Query. It
	// is mutually exclusive with --query on the command line; by the time the
	// exporter runs, RunE has read the file into Query and this field only
	// carries the origin for the configuration record.
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
	// Create opts a SQL export into emitting a CREATE TABLE statement. SQL
	// export defaults to data-only, so the layout the common case produces needs
	// no extra flag; --create re-attaches the table structure when the dump must
	// be self-contained (import --mode create). There is no --no-create: with the
	// structure already opt-in, a second switch for turning it off would only be
	// a way to contradict --create.
	Create bool
	NoData bool // skip INSERT statements

	// Binary column encoding. HexBlob forces 0x literals; NoHexBlob forces raw
	// passthrough. Both false means "automatic": hex is enabled whenever
	// DefaultCharset is not a byte-safe charset. The two flags are mutually
	// exclusive.
	HexBlob   bool
	NoHexBlob bool

	// Pipeline hand-off controls (see the constants above).
	BatchSize      int
	PipelineBuffer int

	// Parallel export options
	Threads  int    // number of parallel export threads (1 = no parallelism)
	ShardKey string // column name for sharding (auto-detect primary key if empty; only exportParallel reads it — a single-threaded run warns that it is ignored)
}

// ImportConfig holds import-specific options.
type ImportConfig struct {
	Config

	Input   string
	Format  string // "del", "sql" or "parquet"
	Mode    ImportMode
	Table   string
	Columns string // Target column list (comma-separated) or mapping (file_col=table_col,...)

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
	}
}

func defaultConfig() Config {
	return Config{
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
		Config:         defaultConfig(),
		Format:         "del",
		DelModifier:    defaultDelModifier(),
		StatementSize:  DefaultStatementSize,
		Threads:        1,
		BatchSize:      DefaultBatchSize,
		PipelineBuffer: DefaultPipelineBuffer,
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
