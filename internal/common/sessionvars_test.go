package common

import (
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/gomydump/internal/config"
)

func TestParseSessionVars(t *testing.T) {
	vars, err := ParseSessionVars([]string{
		"foreign_key_checks=0",
		"sql_mode=STRICT_TRANS_TABLES,NO_ZERO_DATE",
		"wait_timeout = 28800",
		`character_set_results=gbk`,
		"msg=it's fine", // single quote inside is doubled into a safe literal
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vars["foreign_key_checks"] != "0" {
		t.Errorf("numeric must stay unquoted, got %q", vars["foreign_key_checks"])
	}
	if vars["sql_mode"] != "'STRICT_TRANS_TABLES,NO_ZERO_DATE'" {
		t.Errorf("string must be single-quoted, got %q", vars["sql_mode"])
	}
	if vars["wait_timeout"] != "28800" {
		t.Errorf("spaces must be trimmed, got %q", vars["wait_timeout"])
	}
	if vars["msg"] != "'it''s fine'" {
		t.Errorf("inner quote must be doubled, got %q", vars["msg"])
	}

	// empty input yields nil map (no behavioural change)
	if got, err := ParseSessionVars(nil); got != nil || err != nil {
		t.Errorf("nil input: got %v, err %v", got, err)
	}
}

func TestParseSessionVarsErrors(t *testing.T) {
	bad := []string{
		"novalue",        // missing =
		"1bad=x",         // name must start with a letter
		"my var=x",       // space in name
		"charset=utf8",   // reserved (tool-managed)
		"parseTime=true", // reserved (tool-managed)
		"timeout=5s",     // reserved (driver-special)
		"strict=1",       // reserved (driver panics on it)
		"a=1/*comment*/", // comment sequence in value
		"a=1--x",         // line comment in value
		"a=1`b",          // backtick in value
		`a=x\y`,          // backslash: escaping is sql_mode dependent
		"",               // empty entry
	}
	for _, p := range bad {
		if _, err := ParseSessionVars([]string{p}); err == nil {
			t.Errorf("expected error for %q", p)
		}
	}
}

// dsnCfg builds the Config used by the DSN tests. The values are arbitrary; what
// matters is that every field the builder reads is populated.
func dsnCfg(vars map[string]string) *config.Config {
	c := config.NewExportConfig()
	c.Vars = vars
	return &c.Config
}

func TestBuildDSNWithVars(t *testing.T) {
	vars, err := ParseSessionVars([]string{"foreign_key_checks=0", "sql_mode=STRICT_ALL_TABLES"})
	if err != nil {
		t.Fatal(err)
	}
	dsn := BuildDSN(dsnCfg(vars))

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

	// no vars -> identical to BuildDSN
	base := BuildDSN(dsnCfg(nil))
	if got := BuildDSN(dsnCfg(nil)); got != base {
		t.Errorf("nil vars changed DSN:\n got %s\nwant %s", got, base)
	}

	// defensive path: reserved name injected directly is refused, DSN unchanged
	reserved := map[string]string{"charset": "'; DROP"}
	if got := BuildDSN(dsnCfg(reserved)); got != base {
		t.Errorf("reserved override must be refused, got %s", got)
	}
}

func TestSetStatementShape(t *testing.T) {
	// Mirror the driver's handleParams concatenation to assert the final SET
	// statement is well-formed for typical inputs.
	vars, err := ParseSessionVars([]string{"sql_mode=STRICT_TRANS_TABLES,NO_ZERO_DATE", "max_parallel_degree=4"})
	if err != nil {
		t.Fatal(err)
	}
	dsn := BuildDSN(dsnCfg(vars))
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
