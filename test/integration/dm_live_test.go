package integration

// Live Dameng-to-MySQL definition rebuild, driven through the real binary.
//
// This is the gate requirements 2.6.1 asked for. Round 194 measured the path
// (打印 → 建表 → 导出 → 导入 → 逐格比对 → 拒绝) with a shell script against whatever fixtures
// the bed happened to hold; here the same steps run from fixtures the case
// creates itself, so a clean DM8 instance answers the same way a year from now.
//
// Two gates must both be set, because the conversion has a source and a target:
//
//	IMEX_TEST_DM_DSN='dm://SYSDBA:<password>@127.0.0.1:5236/IMEXIT' \
//	IMEX_TEST_DSN='<user>:<password>@tcp(127.0.0.1:3306)/imex_test?parseTime=true' \
//	  go test -count=1 ./test/integration -run DMLive -v
//
// IMEX_TEST_DM_DSN is the DSN shape the tool writes for itself
// (internal/backend/dameng.go buildDamengDSN): the userinfo goes out verbatim
// because the Dameng driver never URL-decodes it, so a password holding a
// literal '?' is unrepresentable here for the same reason it is unrepresentable
// on a real command line. The path names the schema the fixtures are built in,
// which must be one the login can create tables in.
//
// The MySQL side needs one privilege beyond reading and writing rows. The
// rebuilt CREATE statement carries the source's schema qualification — that is
// the point of the file channel, the bytes have to be ones an operator can pipe
// straight into a client — so a database named after the DM schema has to exist.
// The case creates it when it can and says so plainly when it cannot.
//
// Fixture names are fixed rather than run-unique (GT194*), matching how the round
// was first measured, and they stay on the bed afterwards, the way every earlier
// DM round left its取证夹具. Each run drops and recreates them, so leftovers from
// a previous run cannot change the answer.

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/imex/internal/backend"
	"github.com/imex/internal/config"
)

// dmLive is one source DM8 bed plus one MySQL target bed.
type dmLive struct {
	dm     *sql.DB
	my     *sql.DB
	dir    string
	host   string
	port   string
	user   string
	pass   string
	schema string

	myHost string
	myPort string
	myUser string
	myPass string
}

func newDMLive(t *testing.T) *dmLive {
	t.Helper()

	dmDSN := os.Getenv("IMEX_TEST_DM_DSN")
	if dmDSN == "" {
		t.Skip("set IMEX_TEST_DM_DSN to run live Dameng integration tests")
	}
	if os.Getenv("IMEX_TEST_DSN") == "" {
		t.Skip("the dm→mysql rebuild needs a MySQL target too: set IMEX_TEST_DM_DSN and IMEX_TEST_DSN")
	}
	if cliBinary == "" {
		t.Fatal("TestMain did not build the CLI binary; it only does so when a live DSN is set")
	}

	user, pass, host, port, schema, err := parseDamengDSN(dmDSN)
	if err != nil {
		t.Fatalf("parse IMEX_TEST_DM_DSN: %v", err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("IMEX_TEST_DM_DSN port %q: %v", port, err)
	}

	// Opened through the same backend the command builds, so the DSN this case
	// dials with is the DSN the tool would have written for itself.
	cfg := &config.Config{
		Engine:             string(backend.EngineDM),
		Host:               host,
		Port:               number,
		User:               user,
		Password:           pass,
		Database:           schema,
		LogLevel:           config.LogLevelError,
		DefaultCharset:     config.DefaultCharset,
		ConnectTimeout:     config.DefaultConnectTimeout,
		SocketReadTimeout:  config.DefaultSocketTimeout,
		SocketWriteTimeout: config.DefaultSocketTimeout,
		ProgressInterval:   config.DefaultProgressInterval,
	}
	db, err := backend.Must(backend.EngineDM).Open(cfg)
	if err != nil {
		t.Fatalf("open dm: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping %s:%s: %v", host, port, err)
	}

	mc, err := mysql.ParseDSN(os.Getenv("IMEX_TEST_DSN"))
	if err != nil {
		t.Fatalf("parse IMEX_TEST_DSN: %v", err)
	}
	myHost, myPort, err := net.SplitHostPort(mc.Addr)
	if err != nil {
		t.Fatalf("IMEX_TEST_DSN must name a TCP address, got %q: %v", mc.Addr, err)
	}
	mydb, err := sql.Open("mysql", os.Getenv("IMEX_TEST_DSN"))
	if err != nil {
		t.Fatalf("open mysql: %v", err)
	}
	t.Cleanup(func() { mydb.Close() })
	if err := mydb.PingContext(ctx); err != nil {
		t.Fatalf("ping mysql %s: %v", mc.Addr, err)
	}

	return &dmLive{dm: db, my: mydb, dir: t.TempDir(),
		host: host, port: port, user: user, pass: pass, schema: schema,
		myHost: myHost, myPort: myPort, myUser: mc.User, myPass: mc.Passwd}
}

// parseDamengDSN reads `dm://user:password@host:port/schema` the way the Dameng
// driver does: userinfo at the last '@', user separated from password at the
// first ':', and no percent-decoding anywhere in the userinfo.
func parseDamengDSN(dsn string) (user, pass, host, port, schema string, err error) {
	rest, found := strings.CutPrefix(dsn, "dm://")
	if !found {
		return "", "", "", "", "", fmt.Errorf("must start with dm://, got %q", dsn)
	}
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return "", "", "", "", "", fmt.Errorf("no @ in %q", dsn)
	}
	userinfo, endpoint := rest[:at], rest[at+1:]
	colon := strings.Index(userinfo, ":")
	if colon < 0 {
		return "", "", "", "", "", fmt.Errorf("no : between user and password in %q", dsn)
	}
	user, pass = userinfo[:colon], userinfo[colon+1:]
	slash := strings.Index(endpoint, "/")
	if slash < 0 {
		return "", "", "", "", "", fmt.Errorf("no /<schema> in %q", dsn)
	}
	if host, port, err = net.SplitHostPort(endpoint[:slash]); err != nil {
		return "", "", "", "", "", err
	}
	if schema = endpoint[slash+1:]; schema == "" {
		return "", "", "", "", "", fmt.Errorf("%q names no schema after the '/'", dsn)
	}
	if strings.ContainsRune(pass, '?') {
		return "", "", "", "", "", fmt.Errorf("password holds a literal '?', which no dm:// DSN can express")
	}
	return user, pass, host, port, schema, nil
}

// dmFlags are the connection flags every DM command in this file runs with.
// Levels are left to the caller: the WARN half of this feature is only visible
// at warn, and the config record only at info.
func (e *dmLive) dmFlags(extra ...string) []string {
	base := []string{"--engine", "dm", "-h", e.host, "-P", e.port, "-u", e.user, "-p", e.pass,
		"-D", e.schema, "--log-level", "error"}
	return append(base, extra...)
}

// myFlags are the connection flags for the MySQL target bed. The DSN's own
// database is not used: every statement here names its target, and the one the
// rebuilt DDL names is the source schema.
func (e *dmLive) myFlags(extra ...string) []string {
	base := []string{"--engine", "mysql", "-h", e.myHost, "-P", e.myPort, "-u", e.myUser, "-p", e.myPass,
		"--log-level", "error"}
	return append(base, extra...)
}

// dmRun executes one statement as the login user, failing the case.
func (e *dmLive) dmRun(t *testing.T, stmt string) {
	t.Helper()
	if _, err := e.dm.Exec(stmt); err != nil {
		t.Fatalf("dm exec %q: %v", stmt, err)
	}
}

// dmDropIfExists removes a fixture when it is there and ignores the -2106 the
// server answers when it is not: Dameng has no DROP TABLE IF EXISTS.
func (e *dmLive) dmDropIfExists(t *testing.T, table string) {
	t.Helper()
	if _, err := e.dm.Exec("DROP TABLE " + e.schema + "." + table); err != nil {
		if !strings.Contains(err.Error(), "-2106") {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
}

func (e *dmLive) run(t *testing.T, args ...string) (string, int) {
	t.Helper()
	out, err := exec.Command(cliBinary, args...).CombinedOutput()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return strings.TrimRight(string(out), "\r\n"), code
}

// TestDMLive_DMToMySQLDefinitionRebuild walks the whole path round 194 shipped:
// rebuild a definition from the live catalogue, have a real MySQL server accept
// those bytes, move the rows across with the ordinary export/import pair, and
// prove both sides hold the same values. Every step is a command an operator
// would type, on the same binary the release ships.
func TestDMLive_DMToMySQLDefinitionRebuild(t *testing.T) {
	e := newDMLive(t)

	// The fixture is the union of everything the renderer can restate: the
	// identity, both string semantics (VARCHAR is byte-sized, NVARCHAR2
	// character-sized), a CHAR, a sized NUMBER, an explicit DECIMAL, the two
	// bit-spelled booleans, a VARBINARY, a CLOB, three date/time spellings, both
	// float words, a column comment holding a quote, and a table comment.
	const table = "GT194OK"

	e.dmDropIfExists(t, table)
	e.dmRun(t, `CREATE TABLE "`+e.schema+`"."`+table+`" (
 "ID" INT IDENTITY(1,1) NOT NULL,
 "CODE" VARCHAR(10) DEFAULT 'AB' NOT NULL,
 "NAME" NVARCHAR2(64),
 "FIXED" CHAR(4),
 "AMT" NUMBER(10,2) DEFAULT 3.14,
 "QTY" SMALLINT,
 "BIGV" BIGINT,
 "FLAG" BIT,
 "OKIND" BOOLEAN,
 "RAWV" VARBINARY(32),
 "DOC" CLOB,
 "CREATED" DATETIME(6) DEFAULT CURRENT_TIMESTAMP,
 "TS" TIMESTAMP(6),
 "D" DATE,
 "T" TIME(0),
 "FLT" FLOAT,
 "DBL" DOUBLE,
 "NUMB" DECIMAL(18,4),
 CONSTRAINT "PK_`+table+`" NOT CLUSTER PRIMARY KEY("ID"),
 CONSTRAINT "UK_`+table+`" UNIQUE("CODE"))`)
	e.dmRun(t, `CREATE INDEX "`+e.schema+`"."IDX_`+table+`" ON "`+e.schema+`"."`+table+`" ("NAME","QTY")`)
	e.dmRun(t, "COMMENT ON TABLE "+e.schema+"."+table+" IS 'gate round 194'")
	e.dmRun(t, "COMMENT ON COLUMN "+e.schema+"."+table+".CODE IS 'it''s the code'")
	for _, row := range []string{
		`('A1','alpha','ABCD',12.34,7,9007199254740993,1,TRUE,X'0102','doc text','2026-11-16 10:11:12.123456','2026-11-16','10:11:12',1.5,2.5,3.5)`,
		`('B2',NULL,NULL,NULL,NULL,NULL,0,FALSE,NULL,NULL,'2025-01-02 03:04:05','2025-01-02','03:04:05',NULL,NULL,NULL)`,
		`('C3','gamma','WXYZ',-9.99,1,1,1,TRUE,X'FF',NULL,'2024-06-07 08:09:10','2024-06-07','08:09:10',0.5,0.25,100.5)`,
	} {
		e.dmRun(t, "INSERT INTO "+e.schema+"."+table+
			" (CODE,NAME,FIXED,AMT,QTY,BIGV,FLAG,OKIND,RAWV,DOC,TS,D,T,FLT,DBL,NUMB) VALUES "+row)
	}

	ddl := e.render(t, table)

	t.Run("load", func(t *testing.T) {
		if _, err := e.my.Exec("CREATE DATABASE IF NOT EXISTS `" + e.schema + "` DEFAULT CHARACTER SET utf8mb4"); err != nil {
			t.Fatalf("the rebuilt CREATE qualifies the table with the source schema %q, so the MySQL "+
				"account needs CREATE on a database of that name: %v", e.schema, err)
		}
		if _, err := e.my.Exec("DROP TABLE IF EXISTS `" + e.schema + "`.`" + table + "`"); err != nil {
			t.Fatalf("drop target: %v", err)
		}
		path := filepath.Join(e.dir, table+".sql")
		if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
			t.Fatalf("write rebuilt definition: %v", err)
		}
		out, code := e.run(t, append([]string{"exec"}, append(e.myFlags(), "-D", e.schema, "-f", path)...)...)
		if code != 0 || strings.Contains(out, "ERROR") {
			t.Fatalf("a real MySQL server must accept the rebuilt definition: exit %d\n%s", code, out)
		}

		// The key lines are only worth pinning once the server has built them:
		// PRIMARY plus the unique constraint plus the plain index, and the
		// table's CLUSTER index deliberately absent from that count.
		var cols, keys int
		if err := e.my.QueryRow("SELECT COUNT(*) FROM information_schema.COLUMNS"+
			" WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?", e.schema, table).Scan(&cols); err != nil {
			t.Fatalf("count columns: %v", err)
		}
		if err := e.my.QueryRow("SELECT COUNT(DISTINCT INDEX_NAME) FROM information_schema.STATISTICS"+
			" WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?", e.schema, table).Scan(&keys); err != nil {
			t.Fatalf("count indexes: %v", err)
		}
		if cols != 18 {
			t.Errorf("server built %d columns, want 18", cols)
		}
		if keys != 3 {
			t.Errorf("server built %d indexes (PRIMARY + unique + key expected), want 3", keys)
		}
	})

	t.Run("migrate", func(t *testing.T) {
		src := filepath.Join(e.dir, table+".dm.del")
		dst := filepath.Join(e.dir, table+".mysql.del")
		// info level: the row counters this step is judged on are INFO records.
		out, code := e.run(t, append(append([]string{"export"}, e.dmFlags("--log-level", "info")...),
			"-o", src, "-f", "del", "--blob-encoding", "hex",
			"-q", "SELECT * FROM "+e.schema+"."+table)...)
		if code != 0 {
			t.Fatalf("export from dm: exit %d\n%s", code, out)
		}
		out, code = e.run(t, append(append([]string{"import"}, e.myFlags("--log-level", "info")...),
			"-D", e.schema, "-t", table, "-i", src, "-f", "del")...)
		if code != 0 {
			t.Fatalf("import into mysql: exit %d\n%s", code, out)
		}
		// Three rows in, none failed: the rebuilt types hold what the source held.
		if !strings.Contains(out, "inserted=3") || !strings.Contains(out, "failed=0") {
			t.Fatalf("import counters are not 3 inserted / 0 failed:\n%s", out)
		}
		out, code = e.run(t, append(append([]string{"export"}, e.myFlags("--log-level", "info")...),
			"-D", e.schema, "-o", dst, "-f", "del", "--blob-encoding", "hex",
			"-q", "SELECT * FROM "+table+" ORDER BY ID")...)
		if code != 0 {
			t.Fatalf("export back from mysql: exit %d\n%s", code, out)
		}
		cells, bad, detail := compareDELCells(t, src, dst)
		if bad != 0 {
			t.Errorf("%d of %d cells differ:\n%s", bad, cells, detail)
		}
		t.Logf("%d cells compared across %d rows, %d differed", cells, cells/18, bad)
	})
}

// render prints the table's definition in MySQL grammar and pins the words that
// carry the mapping decisions a unit test cannot prove against a real catalogue:
// which Dameng type word became which MySQL one.
func (e *dmLive) render(t *testing.T, table string) string {
	t.Helper()

	// warn level: the identity seed is a documented WARN, and a rebuild that went
	// silent about it would be hiding the one fact the catalogue never reported.
	args := append([]string{"show", "tab", e.schema + "." + table}, e.dmFlags("--log-level", "warn")...)
	out, code := e.run(t, append(args, "--sql-dialect", "mysql")...)
	if code != 0 {
		t.Fatalf("show --sql-dialect mysql: exit %d\n%s", code, out)
	}
	body, log := splitCLIBody(t, out)

	for _, want := range []string{
		"CREATE TABLE `" + e.schema + "`.`" + table + "` (",
		"`ID` int NOT NULL AUTO_INCREMENT",         // INFO2=1, and the identity is the whole key
		"`CODE` varchar(10) NOT NULL DEFAULT 'AB'", // byte-semantics VARCHAR keeps its number
		"COMMENT 'it''s the code'",                 // catalogue comment, quote doubled
		"`NAME` varchar(64)",                       // NVARCHAR2 is character-sized
		"`FIXED` char(4)",
		"`AMT` decimal(10,2) DEFAULT 3.14",
		"`BIGV` bigint",
		"`FLAG` tinyint",     // BIT
		"`OKIND` tinyint(1)", // BOOLEAN, the one MySQL boolean shape
		"`RAWV` varbinary(32)",
		"`DOC` longtext",                                  // CLOB
		"`CREATED` datetime(6) DEFAULT CURRENT_TIMESTAMP", // DATETIME(6)
		"`TS` datetime(6)",                                // TIMESTAMP(6)
		"`D` datetime",                                    // Dameng DATE is a date and a time
		"`T` time",
		"`FLT` double", // FLOAT
		"`DBL` double", // DOUBLE
		"`NUMB` decimal(18,4)",
		"PRIMARY KEY (`ID`)",
		"UNIQUE KEY `UK_" + table + "` (`CODE`)",
		"KEY `IDX_" + table + "` (`NAME`, `QTY`)",
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='gate round 194';",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rebuilt definition is missing %q\n%s", want, body)
		}
	}
	// Exactly one AUTO_INCREMENT: neither key may pick the identity up twice, and
	// the CLUSTER index the server stores for every table must drop out rather
	// than print a second key over the same columns.
	if got := strings.Count(body, "AUTO_INCREMENT"); got != 1 {
		t.Errorf("AUTO_INCREMENT printed %d times, want 1\n%s", got, body)
	}
	if strings.Contains(body, "CLUSTER") {
		t.Errorf("the table's clustered storage index leaked into the statement\n%s", body)
	}
	if strings.Count(log, "identity seed not read") != 1 {
		t.Errorf("want exactly one identity-seed WARN on the log channel\n%s", log)
	}

	// Face ②: the file channel has to hold exactly the bytes stdout holds, and
	// the config record has to say which grammar it printed — otherwise an
	// operator cannot tell a rebuild from a verbatim read.
	path := filepath.Join(e.dir, table+".mysql.sql")
	logPath := filepath.Join(e.dir, table+".mysql.log")
	fileArgs := append([]string{"show", "tab", e.schema + "." + table},
		e.dmFlags("--log-level", "info", "--sql-dialect", "mysql", "-o", path, "-m", logPath)...)
	fileOut, code := e.run(t, fileArgs...)
	if code != 0 {
		t.Fatalf("show to -o: exit %d\n%s", code, fileOut)
	}
	fileBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rebuilt file: %v", err)
	}
	if got := strings.TrimRight(string(fileBytes), "\r\n"); got != body {
		t.Errorf("stdout and -o differ: %d bytes vs %d", len(body), len(got))
	}
	rec, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read config log: %v", err)
	}
	if !strings.Contains(string(rec), "dialect=mysql") {
		t.Errorf("config record does not note dialect=mysql:\n%s", rec)
	}

	t.Logf("rebuilt definition %d bytes", len(fileBytes))
	return body
}

// splitCLIBody separates the definition written to stdout from the log written
// to stderr, because both arrive in one CombinedOutput.
func splitCLIBody(t *testing.T, out string) (body, log string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if isCLILogLine(line) {
			log += line + "\n"
			continue
		}
		body += line + "\n"
	}
	return strings.TrimRight(body, "\n"), strings.TrimRight(log, "\n")
}

// logLine recognises the CLI's own log records: a leading timestamp with the
// level tag a few fields later.
var logLine = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d+ `)

func isCLILogLine(line string) bool {
	return logLine.MatchString(line) && strings.Contains(line, "] ") &&
		(strings.Contains(line, " INFO ") || strings.Contains(line, " WARN ") ||
			strings.Contains(line, " ERROR ") || strings.Contains(line, " DEBUG "))
}

// TestDMLive_RebuildRefusesWhatItCannotPrint pins the fail-loud half of the
// feature against a live catalogue. Each shape below is one the source really
// has, and the answer must be a refusal that names the object — never a
// statement that quietly leaves the fact out.
func TestDMLive_RebuildRefusesWhatItCannotPrint(t *testing.T) {
	e := newDMLive(t)

	cases := []struct {
		name   string
		table  string
		create func(t *testing.T, table string)
		want   string
	}{
		{
			name:  "generated_column",
			table: "GT194GEN",
			create: func(t *testing.T, tbl string) {
				e.dmRun(t, `CREATE TABLE "`+e.schema+`"."`+tbl+`" ("ID" INT,"CODE" VARCHAR(10),`+
					`"FULLNAME" VARCHAR(40) GENERATED ALWAYS AS ("CODE" || '-' || "CODE"))`)
			},
			want: "is generated from",
		},
		{
			name:  "referential_constraint",
			table: "GT194FK",
			create: func(t *testing.T, tbl string) {
				const parent = "GT194P"
				e.dmDropIfExists(t, parent)
				e.dmRun(t, `CREATE TABLE "`+e.schema+`"."`+parent+`" ("ID" INT,`+
					`CONSTRAINT "PK_GT194P" NOT CLUSTER PRIMARY KEY("ID"))`)
				e.dmRun(t, `CREATE TABLE "`+e.schema+`"."`+tbl+`" ("ID" INT,"PID" INT,`+
					`CONSTRAINT "FK_GT194" FOREIGN KEY("PID") REFERENCES "`+e.schema+`"."`+parent+`"("ID"))`)
			},
			want: "carries a referential constraint",
		},
		{
			name:  "partitioned_table",
			table: "GT194PAR",
			create: func(t *testing.T, tbl string) {
				e.dmRun(t, `CREATE TABLE "`+e.schema+`"."`+tbl+`" ("ID" INT,"CREATED" DATE)`+
					` PARTITION BY RANGE("CREATED")`+
					`(PARTITION "P2024" VALUES LESS THAN(TO_DATE('2025-01-01','YYYY-MM-DD')),`+
					`PARTITION "P2025" VALUES LESS THAN(TO_DATE('2026-01-01','YYYY-MM-DD')),`+
					`PARTITION "PMAX" VALUES LESS THAN(MAXVALUE))`)
			},
			want: "is a partitioned table",
		},
		{
			name:  "identity_not_whole_key",
			table: "GT194IDA",
			create: func(t *testing.T, tbl string) {
				e.dmRun(t, `CREATE TABLE "`+e.schema+`"."`+tbl+`" ("A" INT IDENTITY(1,1),"B" INT,`+
					`CONSTRAINT "PK_GT194IDA" NOT CLUSTER PRIMARY KEY("B"))`)
			},
			want: "is not the whole primary key",
		},
		{
			name:  "type_with_offset",
			table: "GT194TZ",
			create: func(t *testing.T, tbl string) {
				e.dmRun(t, `CREATE TABLE "`+e.schema+`"."`+tbl+`" ("ID" INT,"TSTZ" DATETIME WITH TIME ZONE)`)
			},
			want: "has type DATETIME WITH TIME ZONE",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e.dmDropIfExists(t, tc.table)
			tc.create(t, tc.table)
			out, code := e.run(t, append([]string{"show", "tab", e.schema + "." + tc.table},
				e.dmFlags("--sql-dialect", "mysql")...)...)
			if code != 1 {
				t.Fatalf("exit %d, want 1: a refusal is a runtime error, not a usage error\n%s", code, out)
			}
			if !strings.Contains(out, "show tab failed") {
				t.Errorf("refusal is not shaped like the command's own error:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("refusal does not say %q:\n%s", tc.want, out)
			}
		})
	}
}

// compareDELCells reads two DEL files and compares them cell by cell under the
// three normalizations requirements 2.6.1 documents, each one a rendering choice
// rather than a value: numbers compare by value (Dameng "3.5" against MySQL
// "3.5000"), trailing zeros in a fraction are not a difference (Dameng
// "10:11:12" against MySQL DATETIME(6) "10:11:12.000000"), and an empty cell and
// NULL are the same absence. Everything else has to be byte-equal.
func compareDELCells(t *testing.T, dmPath, myPath string) (cells, bad int, detail string) {
	t.Helper()

	head, dmRows := readDEL(t, dmPath)
	myHead, myRows := readDEL(t, myPath)
	if strings.Join(head, "\x00") != strings.Join(myHead, "\x00") {
		t.Fatalf("the two files do not describe the same columns: %v vs %v", head, myHead)
	}
	if len(dmRows) != len(myRows) {
		t.Fatalf("row counts differ: %s has %d, %s has %d", dmPath, len(dmRows), myPath, len(myRows))
	}
	for i, dmRow := range dmRows {
		myRow := myRows[i]
		if len(dmRow) != len(head) || len(myRow) != len(head) {
			t.Fatalf("row %d width differs from the header", i)
		}
		for j, col := range head {
			cells++
			if !sameCell(dmRow[j], myRow[j]) {
				bad++
				detail += fmt.Sprintf("row %d column %s: dm=%q mysql=%q\n", i+1, col, dmRow[j], myRow[j])
			}
		}
	}
	return cells, bad, detail
}

var timeCell = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(\.\d+)?$|^\d{2}:\d{2}:\d{2}(\.\d+)?$`)

func sameCell(a, b string) bool {
	if a == "" || a == "NULL" {
		return b == "" || b == "NULL"
	}
	if ra, ok := new(big.Rat).SetString(a); ok {
		if rb, ok := new(big.Rat).SetString(b); ok {
			return ra.Cmp(rb) == 0
		}
	}
	if timeCell.MatchString(a) && timeCell.MatchString(b) {
		return trimTime(a) == trimTime(b)
	}
	return a == b
}

func trimTime(s string) string {
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		frac := strings.TrimRight(s[i+1:], "0")
		if frac == "" {
			s = s[:i]
		} else {
			s = s[:i] + "." + frac
		}
	}
	return strings.ReplaceAll(s, " ", "T")
}

func readDEL(t *testing.T, path string) ([]string, [][]string) {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer fh.Close()
	r := csv.NewReader(fh)
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(rows) < 1 {
		t.Fatalf("%s has no header row", path)
	}
	return rows[0], rows[1:]
}
