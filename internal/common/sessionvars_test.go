package common

import (
	"testing"
)

// reservedFixture is the shape of the predicate production callers pass: the real
// list lives with the dialect that owns it (internal/backend/dsn_mysql.go), and
// common cannot import backend. Copying the shipped list here would let the table
// and this suite drift together while the CLI still rejected something else;
// internal/backend/dsn_mysql_test.go pins the names themselves.
var reservedFixture = map[string]bool{
	"charset":   true,
	"parseTime": true,
	"timeout":   true,
	"strict":    true,
}

func fixtureReserved(name string) bool { return reservedFixture[name] }

func TestParseSessionVars(t *testing.T) {
	vars, err := ParseSessionVars([]string{
		"foreign_key_checks=0",
		"sql_mode=STRICT_TRANS_TABLES,NO_ZERO_DATE",
		"wait_timeout = 28800",
		`character_set_results=gbk`,
		"msg=it's fine", // single quote inside is doubled into a safe literal
	}, fixtureReserved)
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
	if got, err := ParseSessionVars(nil, fixtureReserved); got != nil || err != nil {
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
		if _, err := ParseSessionVars([]string{p}, fixtureReserved); err == nil {
			t.Errorf("expected error for %q", p)
		}
	}
}

// TestParseSessionVarsReservedNameIsTheEnginesOwn pins that the rejection comes from
// the injected predicate and not from a table in here: a name the caller's engine
// does not reserve is accepted, and a nil predicate reserves nothing. cmd passes the
// resolved backend either way, so no CLI run sees the nil case.
func TestParseSessionVarsReservedNameIsTheEnginesOwn(t *testing.T) {
	if _, err := ParseSessionVars([]string{"charset=utf8"}, fixtureReserved); err == nil {
		t.Error("a reserved name must be rejected")
	}
	vars, err := ParseSessionVars([]string{"charset=utf8"}, func(string) bool { return false })
	if err != nil {
		t.Fatalf("an engine that reserves nothing must accept the name: %v", err)
	}
	if vars["charset"] != "'utf8'" {
		t.Errorf("charset = %q, want the quoted value", vars["charset"])
	}
	if vars, err := ParseSessionVars([]string{"charset=utf8"}, nil); err != nil || vars["charset"] != "'utf8'" {
		t.Errorf("nil predicate: vars=%v err=%v", vars, err)
	}
}
