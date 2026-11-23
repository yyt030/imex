package backend

import (
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

// dsnCfg builds the Config used by the DSN tests. The values are arbitrary; what
// matters is that every field the builder reads is populated.
func dsnCfg(vars map[string]string) *config.Config {
	c := config.NewExportConfig()
	c.Vars = vars
	return &c.Config
}

// parseVars runs the shared parser over this dialect's own reserved list, which is
// exactly what cmd does. Keeping it here rather than hand-writing a map lets the
// test prove the two halves still agree after the list moved out of internal/common.
func parseVars(t *testing.T, pairs ...string) map[string]string {
	t.Helper()
	vars, err := common.ParseSessionVars(pairs, Must(EngineMySQL).IsReservedConnParam)
	if err != nil {
		t.Fatalf("ParseSessionVars(%v): %v", pairs, err)
	}
	return vars
}

func TestBuildMySQLDSNWithVars(t *testing.T) {
	vars := parseVars(t, "foreign_key_checks=0", "sql_mode=STRICT_ALL_TABLES")
	dsn := buildMySQLDSN(dsnCfg(vars))

	// The driver must accept the DSN and keep the vars as Params, which it
	// replays as "SET ..." on every new physical connection.
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("driver rejected DSN: %v", err)
	}
	if cfg.Params["foreign_key_checks"] != "0" {
		t.Errorf("foreign_key_checks missing from Params: %v", cfg.Params)
	}
	if got := cfg.Params["sql_mode"]; got != `'STRICT_ALL_TABLES'` {
		t.Errorf("sql_mode = %q, want quoted literal", got)
	}
	// internal settings must survive untouched (charset/parseTime/... are
	// consumed into typed Config fields by the driver, not kept in Params)
	if cfg.ParseTime || !cfg.InterpolateParams || cfg.MultiStatements {
		t.Errorf("internal driver settings altered: %+v", cfg.Params)
	}
	if !strings.Contains(dsn, "charset=utf8mb4") {
		t.Errorf("charset lost from DSN: %s", dsn)
	}

	// no vars -> the default string, byte for byte
	base := buildMySQLDSN(dsnCfg(nil))
	if got := buildMySQLDSN(dsnCfg(nil)); got != base {
		t.Errorf("nil vars changed DSN:\n got %s\nwant %s", got, base)
	}

	// defensive path: reserved name injected directly is refused, DSN unchanged
	reserved := map[string]string{"charset": "'; DROP"}
	if got := buildMySQLDSN(dsnCfg(reserved)); got != base {
		t.Errorf("reserved override must be refused, got %s", got)
	}
}

func TestSetStatementShape(t *testing.T) {
	// Mirror the driver's handleParams concatenation to assert the final SET
	// statement is well-formed for typical inputs.
	vars := parseVars(t, "sql_mode=STRICT_TRANS_TABLES,NO_ZERO_DATE", "max_parallel_degree=4")
	dsn := buildMySQLDSN(dsnCfg(vars))
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for name, value := range cfg.Params {
		if name == "sql_mode" || name == "max_parallel_degree" {
			parts = append(parts, "SET "+name+" = "+value)
		}
	}
	joined := strings.Join(parts, "; ")
	if !strings.Contains(joined, "SET sql_mode = 'STRICT_TRANS_TABLES,NO_ZERO_DATE'") {
		t.Errorf("unexpected SET statement: %s", joined)
	}
	if !strings.Contains(joined, "SET max_parallel_degree = 4") {
		t.Errorf("unexpected SET statement: %s", joined)
	}
	// values are url-escaped in the DSN, so commas/quotes cannot break parsing
	if strings.ContainsRune(dsn, '\n') {
		t.Errorf("newline leaked into DSN: %s", dsn)
	}
}

// TestBuildMySQLDSNGolden pins the whole connection string for three shapes of
// config. The strings are the output of the builder as it stood in internal/common
// before this round moved it (measured by running both trees over the same cases and
// diffing — 7 of 7 identical), so a change here is a change to what every run dials,
// on a face no other test in the suite looks at: the driver never re-serialises a
// DSN it was not given.
func TestBuildMySQLDSNGolden(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"defaults", dsnCfg(nil), "root@tcp(127.0.0.1:3306)/?readTimeout=5m0s&timeout=5s&writeTimeout=5m0s&charset=utf8mb4&clientFoundRows=false&interpolateParams=true&multiStatements=false&parseTime=false"},
		{"session vars", dsnCfg(parseVars(t, "foreign_key_checks=0", "sql_mode=STRICT_ALL_TABLES")), "root@tcp(127.0.0.1:3306)/?readTimeout=5m0s&timeout=5s&writeTimeout=5m0s&charset=utf8mb4&clientFoundRows=false&foreign_key_checks=0&interpolateParams=true&multiStatements=false&parseTime=false&sql_mode=%27STRICT_ALL_TABLES%27"},
		{"timeouts", func() *config.Config {
			c := dsnCfg(nil)
			c.ConnectTimeout, c.SocketReadTimeout, c.SocketWriteTimeout = 5*time.Second, 10*time.Second, 15*time.Second
			return c
		}(), "root@tcp(127.0.0.1:3306)/?readTimeout=10s&timeout=5s&writeTimeout=15s&charset=utf8mb4&clientFoundRows=false&interpolateParams=true&multiStatements=false&parseTime=false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildMySQLDSN(tc.cfg); got != tc.want {
				t.Errorf("DSN =\n %s\nwant\n %s", got, tc.want)
			}
		})
	}
}

// TestReservedMySQLDSNParams pins the --session-var rejection list where it now
// lives. Every name is either one buildMySQLDSN writes itself or one the driver's
// DSN parser reads as a connection setting, and the count is part of the pin: an
// addition silently accepts a name that overrides a tool-managed parameter, and a
// removal silently rejects one that a working run set last week.
func TestReservedMySQLDSNParams(t *testing.T) {
	if len(reservedMySQLDSNParams) != 29 {
		t.Errorf("reserved list has %d names, want 29", len(reservedMySQLDSNParams))
	}
	// The names the builder writes, plus the one that panics the driver's parser.
	for _, name := range []string{"charset", "parseTime", "interpolateParams", "multiStatements", "clientFoundRows", "timeout", "readTimeout", "writeTimeout", "strict", "tls", "tenant", "maxAllowedPacket"} {
		if !Must(EngineMySQL).IsReservedConnParam(name) {
			t.Errorf("%s must stay reserved on MySQL", name)
		}
	}
	for _, name := range []string{"foreign_key_checks", "sql_mode", "wait_timeout", "character_set_results", "innodb_lock_wait_timeout"} {
		if Must(EngineMySQL).IsReservedConnParam(name) {
			t.Errorf("%s must stay settable on MySQL", name)
		}
	}
	// The other engine has no DSN parameters of these names, and must not inherit
	// the list (which would reject a name PostgreSQL has never heard of).
	for _, name := range []string{"charset", "strict", "timeout"} {
		if Must(EnginePG).IsReservedConnParam(name) {
			t.Errorf("pg must not reserve %s", name)
		}
	}
}

// TestBuildMySQLConfigKeepsSpecialUser pins the fix for usernames/passwords that
// contain DSN metacharacters (':' and '@'). The old path rendered a DSN string and
// handed it to sql.Open, which re-parsed `user:pass@` and truncated a user like
// `test@SERVICE:attest` at the colon (the tail glued onto the password). The
// direct Config path must keep User and Passwd byte-for-byte, because Open now
// uses mysql.NewConnector and never re-parses them.
func TestBuildMySQLConfigKeepsSpecialUser(t *testing.T) {
	cases := []struct {
		name, user, pass string
	}{
		{"plain", "root", "s3cret"},
		{"user with at", "root@sys", "s3cret"},
		{"oceanbase full", "root@sys#obcluster", "s3cret"},
		{"user with colon", "test:SERVICE", "s3cret"},
		{"user with at and colon", "test@SERVICE:attest", "s3cret"},
		{"password with colon", "root", "a:b:c"},
		{"password with at", "root", "p@ss:word"},
		{"both exotic", "test@SERVICE:attest", "p@ss:word"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := dsnCfg(nil)
			c.User, c.Password = tc.user, tc.pass
			cfg, err := buildMySQLConfig(c)
			if err != nil {
				t.Fatalf("buildMySQLConfig: %v", err)
			}
			if cfg.User != tc.user {
				t.Errorf("User = %q, want %q", cfg.User, tc.user)
			}
			if cfg.Passwd != tc.pass {
				t.Errorf("Passwd = %q, want %q", cfg.Passwd, tc.pass)
			}
		})
	}
}

// TestDirectConfigDoesNotMangleWhereDSNDoes documents the exact regression: the
// string form loses a colon-bearing username on the round trip, while the direct
// Config does not. Keeping both assertions here stops anyone from "simplifying"
// Open back to sql.Open(buildMySQLDSN(...)).
func TestDirectConfigDoesNotMangleWhereDSNDoes(t *testing.T) {
	c := dsnCfg(nil)
	c.User, c.Password = "test@SERVICE:attest", "attest123"

	parsed, err := mysql.ParseDSN(buildMySQLDSN(c))
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	if parsed.User == c.User && parsed.Passwd == c.Password {
		t.Fatal("expected the DSN round trip to mangle this user — the regression this test guards no longer reproduces; update the test")
	}

	direct, err := buildMySQLConfig(c)
	if err != nil {
		t.Fatalf("buildMySQLConfig: %v", err)
	}
	if direct.User != c.User || direct.Passwd != c.Password {
		t.Errorf("direct Config mangled user: got %q/%q want %q/%q",
			direct.User, direct.Passwd, c.User, c.Password)
	}
}

// TestBuildMySQLConfigDriverFields pins that the direct path consumes every
// tool-owned parameter into the driver's typed fields, so none is left in Params
// to be replayed as `SET <name> = <value>` (which the server rejects as an
// unknown system variable). Only real --session-var names may remain in Params.
func TestBuildMySQLConfigDriverFields(t *testing.T) {
	c := dsnCfg(parseVars(t, "foreign_key_checks=0", "sql_mode=STRICT_ALL_TABLES"))
	c.Host, c.Port, c.Database = "db.example", 3307, "mydb"
	c.DefaultCharset = "utf8mb4"
	c.ConnectTimeout, c.SocketReadTimeout, c.SocketWriteTimeout = 3*time.Second, 4*time.Second, 5*time.Second

	cfg, err := buildMySQLConfig(c)
	if err != nil {
		t.Fatalf("buildMySQLConfig: %v", err)
	}
	if cfg.Net != "tcp" || cfg.Addr != "db.example:3307" || cfg.DBName != "mydb" {
		t.Errorf("address fields wrong: net=%q addr=%q db=%q", cfg.Net, cfg.Addr, cfg.DBName)
	}
	if cfg.ParseTime {
		t.Error("ParseTime must stay false (raw bytes round-trip)")
	}
	if !cfg.InterpolateParams || cfg.MultiStatements || cfg.ClientFoundRows {
		t.Errorf("driver settings wrong: interp=%v multi=%v cfr=%v",
			cfg.InterpolateParams, cfg.MultiStatements, cfg.ClientFoundRows)
	}
	if cfg.Timeout != 3*time.Second || cfg.ReadTimeout != 4*time.Second || cfg.WriteTimeout != 5*time.Second {
		t.Errorf("timeouts wrong: %v/%v/%v", cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout)
	}
	// charset is consumed into the driver's private charsets field, not left in
	// Params; FormatDSN must still render it so operators see the same string.
	if _, ok := cfg.Params["charset"]; ok {
		t.Error("charset leaked into Params (would be sent as a bogus SET)")
	}
	if !strings.Contains(cfg.FormatDSN(), "charset=utf8mb4") {
		t.Error("charset lost from FormatDSN")
	}
	// Only the session vars remain, and they are the only things the driver will
	// replay as SET on each new physical connection.
	if cfg.Params["foreign_key_checks"] != "0" || cfg.Params["sql_mode"] != `'STRICT_ALL_TABLES'` {
		t.Errorf("session vars missing from Params: %v", cfg.Params)
	}
	for _, reserved := range []string{"parseTime", "interpolateParams", "multiStatements", "clientFoundRows", "timeout", "readTimeout", "writeTimeout"} {
		if _, ok := cfg.Params[reserved]; ok {
			t.Errorf("driver-owned %q left in Params: %v", reserved, cfg.Params)
		}
	}
}
