package backend

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

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
		{"dameng", EngineDM, false},
		{"DM", EngineDM, false},
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
	if got := Engines(); len(got) != 3 || got[0] != "mysql" || got[1] != "pg" || got[2] != "dm" {
		t.Fatalf("Engines() = %v, want [mysql pg dm]: the order is printed in help and in the rejection", got)
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
	dm := Must(EngineDM)

	cases := []struct {
		rows, cols                int
		wantMySQL, wantPG, wantDM string
	}{
		{1, 1, "(?)", "($1)", "(?)"},
		{1, 3, "(?,?,?)", "($1,$2,$3)", "(?,?,?)"},
		{2, 2, "(?,?),(?,?)", "($1,$2),($3,$4)", "(?,?),(?,?)"},
		{3, 2, "(?,?),(?,?),(?,?)", "($1,$2),($3,$4),($5,$6)", "(?,?),(?,?),(?,?)"},
		{0, 3, "", "", ""},
		{3, 0, "", "", ""},
	}
	for _, c := range cases {
		if got := mysql.ValueGroups(c.rows, c.cols); got != c.wantMySQL {
			t.Errorf("mysql ValueGroups(%d,%d) = %q, want %q", c.rows, c.cols, got, c.wantMySQL)
		}
		if got := pg.ValueGroups(c.rows, c.cols); got != c.wantPG {
			t.Errorf("pg ValueGroups(%d,%d) = %q, want %q", c.rows, c.cols, got, c.wantPG)
		}
		if got := dm.ValueGroups(c.rows, c.cols); got != c.wantDM {
			t.Errorf("dm ValueGroups(%d,%d) = %q, want %q", c.rows, c.cols, got, c.wantDM)
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

	dm := Must(EngineDM)
	if got := dm.QuoteIdent(`a"b`); got != `"a""b"` {
		t.Fatalf("dm QuoteIdent = %q, want \"a\"\"b\"", got)
	}
	if got := dm.QuoteIdent("MixedCase"); got != `"MixedCase"` {
		t.Fatalf("dm must quote to preserve case, got %q", got)
	}
}

// TestBatchRowCap checks the two engines' ceilings land where their protocols do.
// Both are 65535, by different routes: MySQL's prepared-statement parameter count
// is a uint16, PostgreSQL's Bind message parameter count is an int16.
func TestBatchRowCap(t *testing.T) {
	for _, e := range []Engine{EngineMySQL, EnginePG, EngineDM} {
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

	dw := Must(EngineDM).BatchWords()
	for _, field := range []string{dw.PacketCap, dw.DriverCap, dw.PacketUnknown, dw.DriverOver, dw.BatchSizeClamp} {
		if contains(field, "max_allowed_packet") || contains(field, "MySQL") {
			t.Fatalf("Dameng record borrows MySQL vocabulary: %q", field)
		}
	}
	if !contains(dw.BatchSizeClamp, "Dameng") {
		t.Fatalf("the clamp record must name the engine that caused it: %q", dw.BatchSizeClamp)
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

	ds := Must(EngineDM).SnapshotSteps()
	if len(ds) != 1 || ds[0].SQL != "SET TRANSACTION ISOLATION LEVEL SERIALIZABLE" {
		t.Fatalf("Dameng snapshot steps = %+v", ds)
	}
}

// TestUpsertSuffix checks both engines' upsert clause, including the one case
// PostgreSQL answers with no clause at all: a table with no key to name.
func TestUpsertSuffix(t *testing.T) {
	cols := []string{"a", "b"}
	got, ret, err := Must(EngineMySQL).UpsertSuffix("t", cols, nil)
	if err != nil {
		t.Fatalf("mysql upsert: %v", err)
	}
	if want := " ON DUPLICATE KEY UPDATE `a` = VALUES(`a`), `b` = VALUES(`b`)"; got != want {
		t.Fatalf("mysql suffix = %q, want %q", got, want)
	}
	if ret != "" {
		t.Fatalf("mysql must not ask for rows back: 1, 2 and 0 affected rows already say it, got %q", ret)
	}
	if Must(EngineMySQL).UpsertNeedsTarget() {
		t.Fatal("MySQL must not be asked for a conflict target: it would cost a catalogue query per run")
	}

	// The guard is part of the pinned text, not an optimisation: without it a row
	// whose values already match the file is rewritten and reported as an update,
	// while MySQL counts the same row as unchanged.
	got, ret, err = Must(EnginePG).UpsertSuffix("t", cols, []string{"a"})
	if err != nil {
		t.Fatalf("pg upsert: %v", err)
	}
	want := ` ON CONFLICT ("a") DO UPDATE SET "a" = EXCLUDED."a", "b" = EXCLUDED."b"` +
		` WHERE "t"."a" IS DISTINCT FROM EXCLUDED."a" OR "t"."b" IS DISTINCT FROM EXCLUDED."b"`
	if got != want {
		t.Fatalf("pg suffix = %q, want %q", got, want)
	}
	if ret != " RETURNING (xmax = 0)" {
		t.Fatalf("pg returning = %q", ret)
	}
	// A composite target keeps its column order, which is what makes the index it
	// matches unambiguous.
	got, _, err = Must(EnginePG).UpsertSuffix("t", cols, []string{"b", "a"})
	if err != nil {
		t.Fatalf("pg composite upsert: %v", err)
	}
	want = ` ON CONFLICT ("b", "a") DO UPDATE SET "a" = EXCLUDED."a", "b" = EXCLUDED."b"` +
		` WHERE "t"."a" IS DISTINCT FROM EXCLUDED."a" OR "t"."b" IS DISTINCT FROM EXCLUDED."b"`
	if got != want {
		t.Fatalf("pg composite suffix = %q, want %q", got, want)
	}
	// A name the statement has to quote is qualified by that same spelling, or the
	// guard would point at a relation the INSERT never named.
	got, _, err = Must(EnginePG).UpsertSuffix("Mixed Case", []string{"id"}, []string{"id"})
	if err != nil {
		t.Fatalf("pg quoted-name upsert: %v", err)
	}
	if want := ` ON CONFLICT ("id") DO UPDATE SET "id" = EXCLUDED."id"` +
		` WHERE "Mixed Case"."id" IS DISTINCT FROM EXCLUDED."id"`; got != want {
		t.Fatalf("pg quoted-name suffix = %q, want %q", got, want)
	}
	if s, r, err := Must(EnginePG).UpsertSuffix("t", cols, nil); err != nil || s != "" || r != "" {
		t.Fatalf("pg suffix without a target = %q / %q, %v; want empty clause and no RETURNING", s, r, err)
	}
	if !Must(EnginePG).UpsertNeedsTarget() {
		t.Fatal("PostgreSQL's ON CONFLICT must be given a target")
	}
}

// TestFKCheckToggle pins the one session statement pair --mode replace wraps
// around the clear: MySQL's own two statements, byte for byte, and nothing at all
// where the engine has no such setting.
func TestFKCheckToggle(t *testing.T) {
	off, on := Must(EngineMySQL).FKCheckToggle()
	if off != "SET FOREIGN_KEY_CHECKS=0" || on != "SET FOREIGN_KEY_CHECKS=1" {
		t.Fatalf("mysql toggle = %q / %q", off, on)
	}
	if off, on := Must(EnginePG).FKCheckToggle(); off != "" || on != "" {
		t.Fatalf("pg toggle = %q / %q; PostgreSQL has no session switch for referential checks", off, on)
	}
	if off, on := Must(EngineDM).FKCheckToggle(); off != "" || on != "" {
		t.Fatalf("dm toggle = %q / %q; Dameng has no session switch for referential checks", off, on)
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
		{"unique_key_columns", func(b Backend) string { return b.UniqueKeyIndexColumnsSQL() }, 2},
		{"target_check", func(b Backend) string { return b.TargetColumnCheckSQL(3) }, 5},
		{"column_types", func(b Backend) string { return b.ColumnTypesSQL(3) }, 5},
	} {
		t.Run(c.name, func(t *testing.T) {
			must(t, c.fn(Must(EngineMySQL)), false, c.n)
			must(t, c.fn(Must(EnginePG)), true, c.n)
			must(t, c.fn(Must(EngineDM)), false, c.n)
		})
	}

	// PostgreSQL resolves the schema from the connection, not from --database,
	// which is already spent as the DSN database.
	pg := Must(EnginePG)
	for _, sql := range []string{pg.TableExistsSQL(), pg.ColumnListSQL(), pg.TargetColumnCheckSQL(2), pg.ColumnTypesSQL(2), pg.GeneratedColumnsSQL(), pg.UniqueKeyColumnsSQL(), pg.UniqueKeyIndexColumnsSQL()} {
		if !contains(sql, "current_schema()") {
			t.Fatalf("probe must scope to the connection's schema: %s", sql)
		}
	}

	// Dameng scopes by OWNER in the Oracle-mode catalogue.
	dm := Must(EngineDM)
	for _, sql := range []string{dm.TableExistsSQL(), dm.ColumnListSQL(), dm.TargetColumnCheckSQL(2), dm.ColumnTypesSQL(2), dm.GeneratedColumnsSQL(), dm.UniqueKeyColumnsSQL(), dm.UniqueKeyIndexColumnsSQL()} {
		if !contains(sql, "SYS.DBA_") {
			t.Fatalf("dm probe must read the Oracle-mode catalogue: %s", sql)
		}
	}

	// The conflict-target probe answers one row per key column, in key order, with
	// the primary key's rows first — the only shape that can name a composite
	// target. Each engine spells that in its own catalogue's vocabulary.
	if sql := Must(EngineMySQL).UniqueKeyIndexColumnsSQL(); !contains(sql, "SEQ_IN_INDEX") {
		t.Fatalf("mysql key-column probe does not order by key position: %s", sql)
	}
	if sql := Must(EnginePG).UniqueKeyIndexColumnsSQL(); !contains(sql, "WITH ORDINALITY") {
		t.Fatalf("pg key-column probe does not unfold the key in order: %s", sql)
	}
	if sql := Must(EngineDM).UniqueKeyIndexColumnsSQL(); !contains(sql, "DBA_CONS_COLUMNS") {
		t.Fatalf("dm key-column probe does not read DBA_CONS_COLUMNS: %s", sql)
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
	if got := Must(EngineDM).DefaultPort(); got != 5236 {
		t.Fatalf("dm default port = %d", got)
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

	v, err = Must(EngineDM).PacketCeiling(nil, nil)
	if err != nil || v != damengPacketCeilingFallback {
		t.Fatalf("dm nil-db ceiling = (%d,%v), want the fallback", v, err)
	}
	if Must(EngineDM).DriverStatementCap() != damengPacketCeilingFallback {
		t.Fatal("dm driver ceiling drifted")
	}
}

// TestPGConnectURLPinsTheConnectionParams closes the gap 11.2 named first: the four
// parameters Open() puts in the connection URL had no case holding their values, so
// dropping or re-spelling one stayed invisible. Both halves are pinned here — the
// text this tool emits, and what pgx makes of that text, because a parameter the
// driver does not read is worth nothing. StatementTimeout's min-of-two rule and the
// whole-second rounding of connect_timeout are the two places a future edit is most
// likely to change behaviour quietly.
func TestPGConnectURLPinsTheConnectionParams(t *testing.T) {
	base := &config.Config{
		Host: "127.0.0.1", Port: 5432, User: "pgprobe", Password: "s3cret",
		Database: "pgprobe", DefaultCharset: config.DefaultCharset,
	}
	s, err := connectURL(base)
	if err != nil {
		t.Fatalf("connectURL(defaults): %v", err)
	}
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("the emitted DSN does not parse: %v (%s)", err, s)
	}
	q := u.Query()
	if q.Get("sslmode") != "disable" {
		t.Errorf("sslmode = %q, want disable: a run the docs call plaintext must not negotiate TLS", q.Get("sslmode"))
	}
	if q.Get("client_encoding") != "UTF8" {
		t.Errorf("client_encoding = %q, want UTF8 for the shipped --default-charset", q.Get("client_encoding"))
	}
	if _, ok := q["statement_timeout"]; ok {
		t.Errorf("statement_timeout = %q with no socket timeout set; the option was not given", q.Get("statement_timeout"))
	}
	if _, ok := q["connect_timeout"]; ok {
		t.Errorf("connect_timeout = %q with no --connect-timeout set", q.Get("connect_timeout"))
	}
	if u.Scheme != "postgres" || u.Host != "127.0.0.1:5432" || u.Path != "/pgprobe" {
		t.Errorf("DSN identity = %s://%s%s, want postgres://127.0.0.1:5432/pgprobe", u.Scheme, u.Host, u.Path)
	}

	// What pgx reads out of that DSN: no TLS config, and the encoding as a session
	// parameter it will actually send.
	pc, err := pgconn.ParseConfig(s)
	if err != nil {
		t.Fatalf("pgx rejected the DSN this tool emits: %v", err)
	}
	if pc.TLSConfig != nil {
		t.Error("pgx built a TLS config from sslmode=disable")
	}
	if pc.RuntimeParams["client_encoding"] != "UTF8" {
		t.Errorf("pgx client_encoding = %q, want UTF8", pc.RuntimeParams["client_encoding"])
	}
	if pc.Host != "127.0.0.1" || pc.Port != 5432 || pc.Database != "pgprobe" || pc.User != "pgprobe" || pc.Password != "s3cret" {
		t.Errorf("pgx read the identity as host=%q port=%d db=%q user=%q pw=%q", pc.Host, pc.Port, pc.Database, pc.User, pc.Password)
	}

	got := connectURLMust(t, &config.Config{
		Host: "127.0.0.1", Port: 5432, User: "u", Password: "p@ss:w/ord?=&%",
		Database: "d", DefaultCharset: "gbk",
		SocketReadTimeout: 90 * time.Second, SocketWriteTimeout: 30 * time.Second,
		ConnectTimeout: 1500 * time.Millisecond,
	})
	q = urlQuery(t, got)
	// The charset maps, the smaller of the two socket deadlines wins, and a
	// sub-second connect deadline rounds up rather than dialling with zero budget.
	if q.Get("client_encoding") != "GBK" {
		t.Errorf("client_encoding for --default-charset gbk = %q", q.Get("client_encoding"))
	}
	if q.Get("statement_timeout") != "30000ms" {
		t.Errorf("statement_timeout = %q, want the 30 s socket deadline of 30000ms", q.Get("statement_timeout"))
	}
	if q.Get("connect_timeout") != "2" {
		t.Errorf("connect_timeout = %q, want 1.5 s rounded up to 2", q.Get("connect_timeout"))
	}
	pc, err = pgconn.ParseConfig(got)
	if err != nil {
		t.Fatalf("pgx rejected a DSN built from a reserved-character password: %v", err)
	}
	if pc.Password != "p@ss:w/ord?=&%" {
		t.Errorf("pgx recovered %q from the URL, want the password unchanged", pc.Password)
	}
	if pc.ConnectTimeout != 2*time.Second {
		t.Errorf("pgx ConnectTimeout = %v, want 2s", pc.ConnectTimeout)
	}
	if pc.RuntimeParams["statement_timeout"] != "30000ms" {
		t.Errorf("pgx statement_timeout = %q, want 300000ms", pc.RuntimeParams["statement_timeout"])
	}

	// A rejected charset has to fail here, not reach the server as a nonsense
	// parameter: the CLI's own check runs earlier, but the backend must not lie.
	if _, err := connectURL(&config.Config{DefaultCharset: "big5"}); err == nil {
		t.Error("connectURL accepted a charset PostgreSQL has no encoding for")
	}
}

func connectURLMust(t *testing.T, cfg *config.Config) string {
	t.Helper()
	s, err := connectURL(cfg)
	if err != nil {
		t.Fatalf("connectURL: %v", err)
	}
	return s
}

func urlQuery(t *testing.T, s string) url.Values {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return u.Query()
}

// TestDamengDSNPinsConnectionParams checks the dm:// DSN the backend builds:
// the connection parameters it always sets, and a userinfo the driver can read
// back unchanged. The second half is the reason the DSN is assembled by hand
// instead of through net/url — see parseDamengDSN.
func TestDamengDSNPinsConnectionParams(t *testing.T) {
	base := &config.Config{
		Host: "127.0.0.1", Port: 5236, User: "SYSDBA", Password: "s3cret",
		Database: "DMDB", DefaultCharset: config.DefaultCharset,
	}
	s, err := buildDamengDSN(base)
	if err != nil {
		t.Fatalf("buildDamengDSN: %v", err)
	}
	want := "dm://SYSDBA:s3cret@127.0.0.1:5236?compatibleMode=oracle&schema=DMDB"
	if s != want {
		t.Errorf("DSN = %s, want %s", s, want)
	}
	user, password, host, q := parseDamengDSN(t, s)
	if user != "SYSDBA" || password != "s3cret" {
		t.Errorf("identity = %q/%q, want SYSDBA/s3cret", user, password)
	}
	if host != "127.0.0.1:5236" {
		t.Errorf("host = %q, want 127.0.0.1:5236", host)
	}
	if q["compatibleMode"] != "oracle" {
		t.Errorf("compatibleMode = %q, want oracle", q["compatibleMode"])
	}
	if q["schema"] != "DMDB" {
		t.Errorf("schema = %q, want DMDB", q["schema"])
	}

	// Every character the driver's hand-rolled parser is not built around has to
	// survive to the server verbatim, because the parser never URL-decodes.
	// Percent-encoding any of them (what net/url does to userinfo) is how a real
	// DM8 answered -2501 "Invalid username or password" for a password as plain
	// as `pw#41`.
	for _, password := range []string{
		"pw#41", "pw%41", "pw@ss", "pw/rd", "pw=1&2", "pw:sep", "p w", "空格#",
	} {
		s = buildDamengDSNUnchecked(t, &config.Config{
			Host: "127.0.0.1", Port: 5236, User: "IMEXIT", Password: password,
			Database: "IMEXIT", DefaultCharset: config.DefaultCharset,
		})
		_, got, _, _ := parseDamengDSN(t, s)
		if got != password {
			t.Errorf("password %q arrived as %q in %s", password, got, s)
		}
	}

	s = buildDamengDSNUnchecked(t, &config.Config{
		Host: "127.0.0.1", Port: 5236, User: "u", Password: "p@ss:w/ord&=%",
		Database: "d", DefaultCharset: config.DefaultCharset,
		ConnectTimeout: 1500 * time.Millisecond, SocketReadTimeout: 90 * time.Second,
	})
	_, _, host, q = parseDamengDSN(t, s)
	if host != "127.0.0.1:5236" {
		t.Errorf("host = %q with a punctuation-heavy password, want 127.0.0.1:5236", host)
	}
	if q["connectTimeout"] != "1500" {
		t.Errorf("connectTimeout = %q, want 1500 ms", q["connectTimeout"])
	}
	if q["socketTimeout"] != "90" {
		t.Errorf("socketTimeout = %q, want 90 s", q["socketTimeout"])
	}
	if q["compatibleMode"] != "oracle" || q["schema"] != "d" {
		t.Errorf("query = %v, want compatibleMode=oracle and schema=d", q)
	}

	// No --database means no schema parameter at all.
	s = buildDamengDSNUnchecked(t, &config.Config{
		Host: "127.0.0.1", Port: 5236, User: "u", Password: "p",
		DefaultCharset: config.DefaultCharset,
	})
	_, _, _, q = parseDamengDSN(t, s)
	if _, ok := q["schema"]; ok {
		t.Errorf("schema = %q without --database, want the parameter absent", q["schema"])
	}

	if _, err := buildDamengDSN(&config.Config{User: ""}); err == nil {
		t.Error("buildDamengDSN accepted an empty user")
	}
}

// parseDamengDSN splits a dm:// DSN the way the Dameng driver's own parser does
// (n.go `parseDSN` of gitee.com/chunanyong/dm v1.8.23): the query is everything
// after the last '?', split on '&' then the first '='; the userinfo is what
// precedes the last '@', split from the host; user and password divide at the
// first ':'. Nothing is URL-decoded on the way through, and the driver exports
// no way to run its parser, so the shape is mirrored here. This is the contract
// the emitted string has to satisfy.
func parseDamengDSN(t *testing.T, dsn string) (user, password, host string, query map[string]string) {
	t.Helper()
	if !strings.HasPrefix(dsn, "dm://") {
		t.Fatalf("DSN %q does not start with dm://", dsn)
	}
	rest := strings.TrimPrefix(dsn, "dm://")
	query = map[string]string{}
	if i := strings.LastIndex(rest, "?"); i > 0 {
		for _, kvString := range strings.Split(rest[i+1:], "&") {
			kv := strings.SplitN(kvString, "=", 2)
			if len(kv) == 2 {
				query[kv[0]] = kv[1]
			}
		}
		rest = rest[:i]
	}
	i := strings.LastIndex(rest, "@")
	if i < 0 {
		t.Fatalf("DSN %q has no user@host section", dsn)
	}
	kv := strings.SplitN(rest[:i], ":", 2)
	if len(kv) != 2 {
		t.Fatalf("DSN %q has no user:password section", dsn)
	}
	return kv[0], kv[1], rest[i+1:], query
}

func buildDamengDSNUnchecked(t *testing.T, cfg *config.Config) string {
	t.Helper()
	s, err := buildDamengDSN(cfg)
	if err != nil {
		t.Fatalf("buildDamengDSN: %v", err)
	}
	return s
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
	if !p.MetadataBinary("bytea") {
		t.Fatal("bytea must be binary")
	}
	// pgx hands back the ASCII bit string, not a packed byte string, so the hex
	// rule would write 0x3130313030313031 for a value the server prints as 8
	// characters. Measured on 14.24.
	if p.MetadataBinary("BIT") || p.MetadataBinary("VARBIT") || p.MetadataBinary("bit varying") {
		t.Fatal("bit and varbit arrive as printable text and must not take the hex path")
	}
	if p.MetadataBinary("uuid") || p.MetadataBinary("json") || p.MetadataBinary("text") {
		t.Fatal("uuid/json/text are printable and must not take the hex path")
	}

	d := Must(EngineDM)
	for _, name := range []string{"NUMBER", "NUMBER(10,2)", "INTEGER", "FLOAT", "BINARY_DOUBLE"} {
		if got := d.MetadataKind(name); got != common.KindNumber {
			t.Fatalf("dm numeric type %q = %v, want %v", name, got, common.KindNumber)
		}
	}
	for name, want := range map[string]common.ColumnKind{
		"DATE":         common.KindTimestamp,
		"TIMESTAMP":    common.KindTimestamp,
		"TIMESTAMP(6)": common.KindTimestamp,
		// The zone-bearing types land on their own kind: their text ends in an
		// offset, which --timestamp-fmt must not re-lay out, and which Dameng's
		// own parser only accepts with a space before it.
		"DATETIME WITH TIME ZONE":        common.KindTimestampTZ,
		"TIMESTAMP WITH TIME ZONE":       common.KindTimestampTZ,
		"TIMESTAMP(6) WITH TIME ZONE":    common.KindTimestampTZ,
		"TIMESTAMP WITH LOCAL TIME ZONE": common.KindTimestampTZ,
		"VARCHAR2":                       common.KindUnknown,
		"CLOB":                           common.KindUnknown,
		"name we have never seen":        common.KindUnknown,
	} {
		if got := d.MetadataKind(name); got != want {
			t.Errorf("dm type %q = %v, want %v", name, got, want)
		}
	}
	if !d.MetadataBinary("BLOB") || !d.MetadataBinary("RAW") || !d.MetadataBinary("LONG RAW") {
		t.Fatal("Dameng binary types must take the hex path")
	}
}

// TestHexBlobMandatory pins the fact that moved out of internal/export: the
// engine, not a charset rule, decides whether a dump must carry binary columns as
// 0x hex. MySQL leaves the decision to the charset; PostgreSQL does not have one.
func TestHexBlobMandatory(t *testing.T) {
	if Must(EngineMySQL).HexBlobMandatory() {
		t.Fatal("mysql must not force hex: --default-charset decides there")
	}
	if !Must(EnginePG).HexBlobMandatory() {
		t.Fatal("pg must force hex: a NUL in a text value is SQLSTATE 22021 whatever the client encoding")
	}
	if !Must(EngineDM).HexBlobMandatory() {
		t.Fatal("dm must force hex: Oracle-mode DM rejects a NUL byte inside a text value")
	}
}

// sameBind compares one bind-adapter result against an expected value, treating a
// []byte as its content (so the byte-level BIT payload can be pinned) and every
// other value by identity.
func sameBind(got, want interface{}) bool {
	gb, gok := got.([]byte)
	wb, wok := want.([]byte)
	if gok || wok {
		return gok && wok && len(gb) == len(wb) && string(gb) == string(wb)
	}
	return got == want
}

// TestBindFixupsIsPerEngineAndPosition pins the third contract face of the import
// path: one adapter per target column, in the order the reader's positions come
// in, and the statement a nil carries. PostgreSQL answers nil for every column —
// its bit input already takes the file's own text — while MySQL answers nil for
// every column except a BIT target: a pg/Dameng dump writes a one-bit column as
// the quoted text "1"/"0", which the driver reads into BIT(1) as the ASCII digit
// byte 0x31 and the server rejects with Error 1406, so only that text is folded
// back to the bit byte. Dameng answers per type name and leaves a binary or
// unknown column alone.
//
// The numbers are the defects this seam exists for, asserted through the
// interface rather than through the helper: text that the driver's 64-bit parse
// moved, a 41-digit value that wrapped into a negative number, and a BIT column
// that read "0x01" as false (Dameng) or 0x31 as too-long (MySQL).
func TestBindFixupsIsPerEngineAndPosition(t *testing.T) {
	types := []string{"VARCHAR2", "NUMBER", "BIGINT", "FLOAT", "BIT", "BLOB", "DATE", "", "NOT_A_TYPE"}

	// PostgreSQL: nil for every column, the file's text binds as it is.
	if got := Must(EnginePG).BindFixups(types); len(got) != len(types) {
		t.Fatalf("pg: %d adapters for %d columns", len(got), len(types))
	} else {
		for i, f := range got {
			if f != nil {
				t.Errorf("pg: column %d (%q) has an adapter; that engine binds the file's text as it is", i, types[i])
			}
		}
		if got := Must(EnginePG).BindFixups(nil); len(got) != 0 {
			t.Errorf("pg: no columns must give no adapters, got %d", len(got))
		}
	}

	// MySQL: nil everywhere except the BIT column (position 4), and the BIT
	// adapter folds the boolean text to a byte while leaving the payload bytes
	// that MySQL's own 0x form and Parquet hand over untouched — the width-safety
	// boundary that keeps a real BIT(8) 0x30 from being re-read as the text "0".
	if got := Must(EngineMySQL).BindFixups(types); len(got) != len(types) {
		t.Fatalf("mysql: %d adapters for %d columns", len(got), len(types))
	} else {
		for i, f := range got {
			want := i == 4
			if (f != nil) != want {
				t.Errorf("mysql: column %d (%q) adapter = %v, want present=%v", i, types[i], f != nil, want)
			}
		}
		if got := Must(EngineMySQL).BindFixups(nil); len(got) != 0 {
			t.Errorf("mysql: no columns must give no adapters, got %d", len(got))
		}
		bit := Must(EngineMySQL).BindFixups(types)[4]
		// bindRender collapses every []byte to "@bytes", so the BIT adapter is
		// checked on its concrete result: the boolean text becomes the exact bit
		// byte, and every value that must not move (the payload bytes MySQL's own
		// 0x form and Parquet hand over, and unrelated text) stays identical.
		for _, c := range []struct {
			in   interface{}
			want interface{}
			why  string
		}{
			{"1", []byte{1}, "boolean text 1 becomes the bit byte"},
			{"0", []byte{0}, "boolean text 0 becomes the bit byte"},
			{"true", []byte{1}, "the word true becomes the bit byte"},
			{"false", []byte{0}, "the word false becomes the bit byte"},
			{[]byte{0x30}, []byte{0x30}, "a real payload byte is NOT re-read as text"},
			{[]byte{0x01}, []byte{0x01}, "MySQL's own decoded payload passes through"},
			{"2", "2", "any other text is left as the file wrote it"},
		} {
			if !sameBind(bit(c.in), c.want) {
				t.Errorf("mysql BIT: %s: %#v = %#v, want %#v", c.why, c.in, bit(c.in), c.want)
			}
		}
	}

	f := Must(EngineDM).BindFixups(types)
	if len(f) != len(types) {
		t.Fatalf("dm: %d adapters for %d columns", len(f), len(types))
	}
	// A binary target already gets bytes, and a name the type table does not
	// know must not guess: both are nil, at their own position.
	for _, i := range []int{5, 7, 8} {
		if f[i] != nil {
			t.Errorf("dm: column %d (%q) must have no adapter", i, types[i])
		}
	}
	for _, i := range []int{0, 1, 2, 3, 4, 6} {
		if f[i] == nil {
			t.Errorf("dm: column %d (%q) must have an adapter", i, types[i])
		}
	}
	for _, c := range []struct {
		col  int
		in   interface{}
		want string
		why  string
	}{
		{1, "99999999999999999999", "#99999999999999999999", "D1: the text path drifted it"},
		{1, "899999999999999.99999", "#899999999999999.99999", "D1: the text path rounded it"},
		{2, "9999999999999999999999999999999999999999999", "#" + strings.Repeat("9", 40), "D1: overflow must not wrap"},
		{4, "0xaa", "1", "D2: the text path landed 0"},
		{4, "170", "1", "D2: the text path landed 0"},
		{3, []byte{0xaa}, "170", "D3: a payload into FLOAT"},
		{0, []byte("abc"), "abc", "text column receives the text"},
		{6, "2024-05-06 07:08:09", "2024-05-06 07:08:09", "a date keeps its own text"},
	} {
		if got := bindRender(f[c.col](c.in)); got != c.want {
			t.Errorf("dm: %s: column %d (%q) with %#v = %s, want %s", c.why, c.col, types[c.col], c.in, got, c.want)
		}
	}
}

// TestLookupAppliesTheDefaultEngine pins the non-failing resolver the format
// decisions use: an exact engine spelling wins, and anything else — above all the
// empty string a hand-built config carries — is MySQL, so no library caller that
// never set Engine can start hitting PostgreSQL's dialect by accident.
func TestNormalizeAppliesTheSameRuleAsParse(t *testing.T) {
	cases := []struct {
		in   string
		want Engine
	}{
		{"", EngineMySQL},
		{"mysql", EngineMySQL},
		{"MYSQL", EngineMySQL},
		{" Mysql ", EngineMySQL},
		{"pg", EnginePG},
		{"PG", EnginePG},
		{"  Pg  ", EnginePG},
		{"dm", EngineDM},
		{"DM", EngineDM},
		{"dameng", EngineDM},
		{"  Dameng  ", EngineDM},
		// Values cmd rejects with exit 2 never reach a decision point, but the
		// accessors must still answer with the default rather than panic.
		{"postgres", EngineMySQL},
		{"oracle", EngineMySQL},
		{"  ", EngineMySQL},
	}
	for _, c := range cases {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
		if e, err := Parse(c.in); err == nil && e != Normalize(c.in) {
			t.Errorf("Normalize(%q) = %q disagrees with Parse, which returns %q", c.in, Normalize(c.in), e)
		}
	}
}

func TestLookupAppliesTheDefaultEngine(t *testing.T) {
	for _, raw := range []string{"pg", "PG", "  Pg  ", "dm", "DM", "dameng", "  Dameng  "} {
		want := EnginePG
		if strings.ToLower(strings.TrimSpace(raw)) == "dm" || strings.ToLower(strings.TrimSpace(raw)) == "dameng" {
			want = EngineDM
		}
		if got := Lookup(raw).Engine(); got != want {
			t.Errorf("Lookup(%q).Engine() = %q, want %q", raw, got, want)
		}
	}
	for _, raw := range []string{"", "mysql", "oracle", "  "} {
		if got := Lookup(raw).Engine(); got != EngineMySQL {
			t.Errorf("Lookup(%q).Engine() = %q, want mysql", raw, got)
		}
	}
}
