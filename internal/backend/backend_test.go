package backend

import (
	"testing"

	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

// TestParseEngine pins the accepted spellings and the rejection text. --engine's
// value vocabulary is contract face ① and ③ at once: the default must be MySQL so
// every existing run keeps doing what it did, and the rejection names the values in
// the order the help text lists them.
func TestParseEngine(t *testing.T) {
	cases := []struct {
		in     string
		want   Engine
		reject bool
	}{
		{"", EngineMySQL, false},
		{"mysql", EngineMySQL, false},
		{"MYSQL", EngineMySQL, false},
		{"  Pg  ", EnginePG, false},
		{"pg", EnginePG, false},
		{"postgres", "", true},
		{"dameng", "", true},
		{"mariadb", "", true},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if c.reject {
			if err == nil {
				t.Fatalf("Parse(%q) = %q, want a rejection", c.in, got)
			}
			if want := `invalid --engine "` + c.in + `"`; len(err.Error()) < len(want) || err.Error()[:len(want)] != want {
				t.Errorf("Parse(%q) error = %q, want it to open with %q", c.in, err.Error(), want)
			}
			continue
		}
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("Parse(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := Engines(); len(got) != 2 || got[0] != "mysql" || got[1] != "pg" {
		t.Fatalf("Engines() = %v, want [mysql pg]: the order is printed in help and in the rejection", got)
	}
}

// TestForConfigRejectsUnknownEngine covers the library caller: a Config built by
// hand must not silently fall through to MySQL.
func TestForConfigRejectsUnknownEngine(t *testing.T) {
	cfg := config.NewExportConfig()
	cfg.Engine = "oracle"
	if _, err := ForConfig(&cfg.Config); err == nil {
		t.Fatal("ForConfig accepted an unknown engine")
	}
	cfg.Engine = ""
	be, err := ForConfig(&cfg.Config)
	if err != nil {
		t.Fatalf("an unset engine must mean MySQL: %v", err)
	}
	if be.Engine() != EngineMySQL {
		t.Fatalf("Engine() = %q", be.Engine())
	}
}

// TestValueGroups is the placeholder contract for both engines: MySQL repeats the
// positional marker per row, PostgreSQL numbers every parameter across the whole
// statement. Getting either wrong is a server error on every batch, so the exact
// strings are pinned rather than sketched.
func TestValueGroups(t *testing.T) {
	mysql := Must(EngineMySQL)
	pg := Must(EnginePG)

	cases := []struct {
		rows, cols        int
		wantMySQL, wantPG string
	}{
		{1, 1, "(?)", "($1)"},
		{1, 3, "(?,?,?)", "($1,$2,$3)"},
		{2, 2, "(?,?),(?,?)", "($1,$2),($3,$4)"},
		{3, 2, "(?,?),(?,?),(?,?)", "($1,$2),($3,$4),($5,$6)"},
		{0, 3, "", ""},
		{3, 0, "", ""},
	}
	for _, c := range cases {
		if got := mysql.ValueGroups(c.rows, c.cols); got != c.wantMySQL {
			t.Errorf("mysql ValueGroups(%d,%d) = %q, want %q", c.rows, c.cols, got, c.wantMySQL)
		}
		if got := pg.ValueGroups(c.rows, c.cols); got != c.wantPG {
			t.Errorf("pg ValueGroups(%d,%d) = %q, want %q", c.rows, c.cols, got, c.wantPG)
		}
	}

	// The two build paths PostgreSQL has (the ordinal table under 1000
	// parameters, the builder above it) must agree, and the ordinals must run
	// 1..rows*cols with no gap or repeat.
	big := pg.ValueGroups(250, 8)
	if small := pg.ValueGroups(250, 8); big != small {
		t.Fatal("the fast path is not stable")
	}
	if n := countOcc(big, "$"); n != 2000 {
		t.Fatalf("250x8 batch carries %d ordinals, want 2000", n)
	}
	if last := pg.ValueGroups(2, 650); !endsWith(last, "$1300)") {
		t.Fatalf("large batch must number through the whole statement, got ...%s", last[len(last)-12:])
	}
}

func countOcc(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
		}
	}
	return n
}

func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// TestQuoteIdent pins both quoting dialects, including the doubled-quote escape.
// A name containing the engine's own quote character must not be able to end the
// quote early: that is an injection surface as well as a correctness bug.
func TestQuoteIdent(t *testing.T) {
	mysql := Must(EngineMySQL)
	pg := Must(EnginePG)

	if got := mysql.QuoteIdent("a`b"); got != "`a``b`" {
		t.Fatalf("mysql QuoteIdent = %q, want `a``b`", got)
	}
	if got := pg.QuoteIdent(`a"b`); got != `"a""b"` {
		t.Fatalf("pg QuoteIdent = %q, want \"a\"\"b\"", got)
	}
	if got := pg.QuoteIdent("MixedCase"); got != `"MixedCase"` {
		t.Fatalf("pg must quote to preserve case, got %q", got)
	}
}

// TestBatchRowCap checks the two engines' ceilings land where their protocols do.
// Both are 65535, by different routes: MySQL's prepared-statement parameter count
// is a uint16, PostgreSQL's Bind message parameter count is an int16.
func TestBatchRowCap(t *testing.T) {
	for _, e := range []Engine{EngineMySQL, EnginePG} {
		be := Must(e)
		if rows, limit := be.BatchRowCap(5); limit != 65535 || rows != 13107 {
			t.Errorf("%s BatchRowCap(5) = (%d,%d), want (13107,65535)", e, rows, limit)
		}
		if rows, _ := be.BatchRowCap(0); rows != 0 {
			t.Errorf("%s BatchRowCap(0) = %d, want 0 = no ceiling beyond the byte budget", e, rows)
		}
		if rows, _ := be.BatchRowCap(70000); rows != 1 {
			t.Errorf("%s BatchRowCap(70000) = %d, want one row rather than an empty batch", e, rows)
		}
	}
}

// TestBatchWordsIsMySqlCommandKept is the message-contract half of the backend: the
// MySQL wording must survive the move verbatim, while PostgreSQL names its own
// ceilings instead of borrowing MySQL's vocabulary.
func TestBatchWords(t *testing.T) {
	mw := Must(EngineMySQL).BatchWords()
	if mw.PacketCap != "max_allowed_packet" || mw.DriverCap != "driver_packet_cap" {
		t.Fatalf("MySQL ceiling names drifted: %+v", mw)
	}
	if mw.PacketUnknown != "could not read max_allowed_packet, assuming conservative batch limit" {
		t.Fatalf("MySQL fallback warning drifted: %q", mw.PacketUnknown)
	}
	if mw.DriverOver != "--statement-size is above the driver interpolation ceiling, generated statements will fall back to server-side prepared statements" {
		t.Fatalf("MySQL driver warning drifted: %q", mw.DriverOver)
	}
	if mw.BatchSizeClamp != "lowering --batch-size to stay under the MySQL placeholder limit" {
		t.Fatalf("MySQL clamp warning drifted: %q", mw.BatchSizeClamp)
	}

	pw := Must(EnginePG).BatchWords()
	for _, field := range []string{pw.PacketCap, pw.DriverCap, pw.PacketUnknown, pw.DriverOver, pw.BatchSizeClamp} {
		if contains(field, "max_allowed_packet") || contains(field, "MySQL") {
			t.Fatalf("PostgreSQL record borrows MySQL vocabulary: %q", field)
		}
	}
	if !contains(pw.BatchSizeClamp, "PostgreSQL") {
		t.Fatalf("the clamp record must name the engine that caused it: %q", pw.BatchSizeClamp)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestSnapshotSteps keeps the MySQL statements and their two distinct warnings
// exactly as the tool has always emitted them, and pins the single PostgreSQL step.
func TestSnapshotSteps(t *testing.T) {
	ms := Must(EngineMySQL).SnapshotSteps()
	if len(ms) != 2 {
		t.Fatalf("MySQL snapshot steps = %d, want 2", len(ms))
	}
	if ms[0].SQL != "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ" || ms[0].Warn != "failed to set isolation level" {
		t.Fatalf("first MySQL step drifted: %+v", ms[0])
	}
	if ms[1].SQL != "START TRANSACTION WITH CONSISTENT SNAPSHOT" || ms[1].Warn != "failed to start consistent snapshot" {
		t.Fatalf("second MySQL step drifted: %+v", ms[1])
	}

	ps := Must(EnginePG).SnapshotSteps()
	if len(ps) != 1 || ps[0].SQL != "BEGIN ISOLATION LEVEL REPEATABLE READ" {
		t.Fatalf("PostgreSQL snapshot steps = %+v", ps)
	}
}

// TestUpsertSuffixRefusesPG is the library-caller guard behind the CLI refusal.
func TestUpsertSuffix(t *testing.T) {
	cols := []string{"a", "b"}
	got, err := Must(EngineMySQL).UpsertSuffix(cols)
	if err != nil {
		t.Fatalf("mysql upsert: %v", err)
	}
	if want := " ON DUPLICATE KEY UPDATE `a` = VALUES(`a`), `b` = VALUES(`b`)"; got != want {
		t.Fatalf("mysql suffix = %q, want %q", got, want)
	}
	if _, err := Must(EnginePG).UpsertSuffix(cols); err == nil {
		t.Fatal("PostgreSQL must refuse to guess an ON CONFLICT target")
	}
}

// TestMetadataProbesAreEngineShaped asserts the placeholder style of every
// catalogue statement, because a `?` reaching PostgreSQL is a syntax error and a
// `$n` reaching MySQL is one too.
func TestMetadataProbes(t *testing.T) {
	must := func(t *testing.T, sql string, wantDollar bool, n int) {
		t.Helper()
		if has := contains(sql, "$"); has != wantDollar {
			t.Fatalf("statement %q dollar-placeholders=%v want %v", sql, has, wantDollar)
		}
		if wantDollar {
			for i := 1; i <= n; i++ {
				if !contains(sql, "$"+itoa(i)) {
					t.Fatalf("statement is missing $%d: %s", i, sql)
				}
			}
		} else if countOcc(sql, "?") != n {
			t.Fatalf("statement carries %d `?` for %d args: %s", countOcc(sql, "?"), n, sql)
		}
	}

	for _, c := range []struct {
		name string
		fn   func(Backend) string
		n    int
	}{
		{"table_exists", func(b Backend) string { return b.TableExistsSQL() }, 2},
		{"column_list", func(b Backend) string { return b.ColumnListSQL() }, 2},
		{"generated_columns", func(b Backend) string { return b.GeneratedColumnsSQL() }, 2},
		{"unique_keys", func(b Backend) string { return b.UniqueKeyColumnsSQL() }, 2},
		{"target_check", func(b Backend) string { return b.TargetColumnCheckSQL(3) }, 5},
		{"column_types", func(b Backend) string { return b.ColumnTypesSQL(3) }, 5},
	} {
		t.Run(c.name, func(t *testing.T) {
			must(t, c.fn(Must(EngineMySQL)), false, c.n)
			must(t, c.fn(Must(EnginePG)), true, c.n)
		})
	}

	// PostgreSQL resolves the schema from the connection, not from --database,
	// which is already spent as the DSN database.
	pg := Must(EnginePG)
	for _, sql := range []string{pg.TableExistsSQL(), pg.ColumnListSQL(), pg.TargetColumnCheckSQL(2), pg.ColumnTypesSQL(2), pg.GeneratedColumnsSQL(), pg.UniqueKeyColumnsSQL()} {
		if !contains(sql, "current_schema()") {
			t.Fatalf("probe must scope to the connection's schema: %s", sql)
		}
	}
}

// TestProbeStatementsAreByteExact pins the catalogue probes to the exact text the
// server receives. The MySQL column is the statement internal/import emitted
// before any engine abstraction existed, written out here in full because the
// first version of this refactor built the IN lists with ValueGroups: that renders
// "(?,?)", so the query became `IN ((?,?))` — one row constructor instead of two
// scalars — and MariaDB answered every import with "Error 4078 (HY000): Illegal
// parameter data types varchar and row for operation '='". The placeholder count
// stayed right, so only an exact-text assertion catches that shape.
func TestProbeStatementsAreByteExact(t *testing.T) {
	m := Must(EngineMySQL)
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"table_exists", m.TableExistsSQL(),
			"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?"},
		{"column_list", m.ColumnListSQL(),
			"SELECT UPPER(COLUMN_NAME) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?"},
		{"target_check", m.TargetColumnCheckSQL(3),
			"SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND UPPER(COLUMN_NAME) IN (?,?,?)"},
		{"column_types", m.ColumnTypesSQL(3),
			"SELECT COLUMN_NAME, DATA_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME IN (?,?,?)"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, c.got, c.want)
		}
	}

	// The same lists on PostgreSQL carry ordinals, and they start at $3 because
	// the database and the table already took $1 and $2.
	p := Must(EnginePG)
	if got, want := p.TargetColumnCheckSQL(3), " AND UPPER(column_name) IN ($3,$4,$5)"; !contains(got, want) {
		t.Errorf("pg target check: got %q, want it to contain %q", got, want)
	}
	if got, want := p.ColumnTypesSQL(3), " AND LOWER(column_name) IN (LOWER($3),LOWER($4),LOWER($5))"; !contains(got, want) {
		t.Errorf("pg column types: got %q, want it to contain %q", got, want)
	}
	for _, n := range []int{0, 1, 2} {
		if got := m.PlaceholderList(1, n); contains(got, "(") {
			t.Errorf("PlaceholderList(1,%d) = %q, must not contain parentheses", n, got)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestDefaultPort is the only option value --engine changes by itself.
func TestDefaultPort(t *testing.T) {
	if got := Must(EngineMySQL).DefaultPort(); got != 3306 {
		t.Fatalf("mysql default port = %d", got)
	}
	if got := Must(EnginePG).DefaultPort(); got != 5432 {
		t.Fatalf("pg default port = %d", got)
	}
}

// TestPacketCeilingWithoutConnection keeps the dry-run path alive: a caller that
// never dialled still needs a budget, and it must be the assumed one, not a panic.
func TestPacketCeilingWithoutConnection(t *testing.T) {
	v, err := Must(EngineMySQL).PacketCeiling(nil, nil)
	if err != nil || v != MySQLPacketCeilingFallback {
		t.Fatalf("mysql nil-db ceiling = (%d,%v), want the fallback", v, err)
	}
	v, err = Must(EnginePG).PacketCeiling(nil, nil)
	if err != nil || v != 1<<30 {
		t.Fatalf("pg ceiling = (%d,%v), want the 1 GiB protocol cap", v, err)
	}
	if Must(EnginePG).DriverStatementCap() != v {
		t.Fatal("pg has no separate client-side interpolation ceiling")
	}
	if Must(EngineMySQL).DriverStatementCap() != MySQLDriverInterpolationCap {
		t.Fatal("mysql driver ceiling drifted")
	}
}

// TestMetadataClassificationVocabulary pins the two type vocabularies the DEL,
// Parquet and import paths classify against, including the types that must NOT be
// treated as binary (uuid, json) and the ones that must be (bytea, bit).
func TestMetadataClassification(t *testing.T) {
	m := Must(EngineMySQL)
	if m.MetadataKind("VARCHAR") != common.KindUnknown {
		t.Fatal("mysql VARCHAR must stay text passthrough")
	}
	if m.MetadataKind("BIGINT") != common.KindNumber {
		t.Fatal("mysql BIGINT must classify as a number")
	}
	if !m.MetadataBinary("MEDIUMBLOB") {
		t.Fatal("mysql MEDIUMBLOB must be binary")
	}

	p := Must(EnginePG)
	// The driver's DatabaseTypeName ("INT4") and information_schema's data_type
	// ("integer") are different spellings of one type; both must land on the same
	// kind, and it must be the kind MySQL gives the equivalent type.
	for _, name := range []string{"INT4", "integer", "INT8", "BIGINT", "NUMERIC", "FLOAT4", "SERIAL"} {
		want := common.KindNumber
		if got := p.MetadataKind(name); got != want {
			t.Fatalf("pg integer type %q = %v, want %v", name, got, want)
		}
	}
	for name, want := range map[string]common.ColumnKind{
		"DATE":                     common.KindDate,
		"TIME":                     common.KindTime,
		"TIMESTAMP":                common.KindTimestamp,
		"TIMESTAMPTZ":              common.KindTimestamp,
		"timestamp with time zone": common.KindTimestamp,
		"BOOL":                     common.KindNumber,
		"UUID":                     common.KindUnknown,
		"JSON":                     common.KindUnknown,
		"text":                     common.KindUnknown,
		"name we have never seen":  common.KindUnknown,
	} {
		if got := p.MetadataKind(name); got != want {
			t.Errorf("pg type %q = %v, want %v", name, got, want)
		}
	}
	if !p.MetadataBinary("bytea") || !p.MetadataBinary("BIT") {
		t.Fatal("bytea and bit must be binary")
	}
	if p.MetadataBinary("uuid") || p.MetadataBinary("json") || p.MetadataBinary("text") {
		t.Fatal("uuid/json/text are printable and must not take the hex path")
	}
}
