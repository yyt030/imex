package backend

import "testing"

// TestMySQLBindFixupsSelectsByTargetType pins which MySQL target columns get a bind
// adapter, from the type words information_schema answers with. Four answers are
// possible and the adapter is named by what it does, not by a pointer comparison: a
// one-bit target folds the text "1" to the bit byte, a wider BIT target leaves text
// alone and folds only a Go bool, a numeric target folds pg's "t" to the integer text
// "1", and everything else gets nil so the value binds exactly as the reader produced
// it.
//
// The list is the whole MySQL numeric vocabulary classifyColumnKind knows, the
// spellings a dump's binary columns arrive in, and the character, temporal and
// catch-up shapes that must stay nil — a VARCHAR target is where "t" is content and
// has to arrive verbatim.
func TestMySQLBindFixupsSelectsByTargetType(t *testing.T) {
	cases := []struct {
		dt   string
		want string
	}{
		// BIT, in every spelling the catalogue and the driver use. One bit folds the
		// boolean text; two bits and up must not, because that same text may be
		// MySQL's own payload byte quoted verbatim.
		{"bit", "bit1"},
		{"BIT", "bit1"},
		{"bit(1)", "bit1"},
		{"bit(8)", "bitwide"},
		{"BIT(64)", "bitwide"},
		{" bit(8) ", "bitwide"},

		// Numeric targets: the integer, decimal and floating-point words.
		{"tinyint", "num"},
		{"TINYINT(1)", "num"},
		{"smallint", "num"},
		{"mediumint", "num"},
		{"int", "num"},
		{"INT(11)", "num"},
		{"integer", "num"},
		{"bigint", "num"},
		{"UNSIGNED BIGINT", "num"},
		{"unsigned int", "num"},
		{"decimal", "num"},
		{"DECIMAL(20,5)", "num"},
		{"numeric(10,2)", "num"},
		{"float", "num"},
		{"double", "num"},
		{"real", "num"},
		{"serial", "num"},

		// Everything else: no adapter, no change at all.
		{"char", "none"},
		{"VARCHAR(64)", "none"},
		{"text", "none"},
		{"enum", "none"},
		{"set", "none"},
		{"json", "none"},
		{"date", "none"},
		{"datetime", "none"},
		{"TIMESTAMP", "none"},
		{"time", "none"},
		// YEAR is a number to the server but KindYear to this codebase's type
		// table, and no engine writes a boolean into it. Stated rather than
		// assumed, because the fold's scope is exactly this vocabulary.
		{"year", "none"},
		{"YEAR", "none"},
		{"blob", "none"},
		{"varbinary", "none"},
		{"geometry", "none"},
		{"", "none"},
		{"not_a_type", "none"},
	}

	for _, c := range cases {
		f := Must(EngineMySQL).BindFixups([]string{c.dt})[0]
		if got := bindLabel(f); got != c.want {
			t.Errorf("BindFixups(%q) = %s, want %s", c.dt, got, c.want)
		}
	}
}

// bindLabel names an adapter by what it does to the two probes that separate the
// four possible answers, so the assertion does not depend on function identity.
func bindLabel(f func(interface{}) interface{}) string {
	if f == nil {
		return "none"
	}
	switch {
	case sameBind(f("1"), []byte{1}):
		return "bit1"
	case sameBind(f("t"), "1"):
		return "num"
	case sameBind(f(true), []byte{1}) && sameBind(f("1"), "1"):
		return "bitwide"
	}
	return "unknown adapter"
}

// TestMySQLBindBoolTextFoldsPeerBooleanText is the F4 assertion: PostgreSQL's
// boolean text reaches a MySQL numeric target in four spellings (measured: "t" and
// "f" in its DEL and Parquet files, true and false in its SQL dumps), and through a
// placeholder MySQL parses none of them — `Error 1366 (22007): Incorrect integer
// value: 't' for column ...` took the whole batch down. Only those four move, and
// only into the integer text the column already accepts.
//
// The pass-through half matters as much as the fold: a value that was already
// usable must reach the driver with the same Go type it arrived with, because the
// driver quotes a []byte differently from a string, and a payload that is not one
// of the four spellings must not be re-typed on the way.
func TestMySQLBindBoolTextFoldsPeerBooleanText(t *testing.T) {
	num := Must(EngineMySQL).BindFixups([]string{"TINYINT(1)"})[0]
	if num == nil {
		t.Fatal("numeric target has no adapter")
	}

	for _, c := range []struct {
		in   interface{}
		want interface{}
		why  string
	}{
		{"t", "1", "pg DEL / Parquet true"},
		{"f", "0", "pg DEL / Parquet false"},
		{"T", "1", "upper-case letter"},
		{" F ", "0", "surrounding space, as a quoted DEL cell can carry"},
		{"true", "1", "pg SQL dump word"},
		{"TRUE", "1", "upper-case word"},
		{"false", "0", "pg SQL dump word"},
		{[]byte("t"), "1", "byte text folds too, to the integer text the column parses"},

		{"1", "1", "integer text is already right"},
		{"0", "0", "integer text is already right"},
		{"-3.5", "-3.5", "decimal text"},
		{"10110010", "10110010", "a bit-string-shaped value is not a boolean"},
		{"yes", "yes", "any other word is left for the server to judge"},
		{"tt", "tt", "only the exact spellings move"},
		{"", "", "empty text"},
		{nil, nil, "NULL never reaches the fold as these letters"},
		{int64(1), int64(1), "a typed value is not text"},
		{[]byte{0x30, 0x31}, []byte{0x30, 0x31}, "bytes that are not boolean text keep their type"},
	} {
		got := num(c.in)
		if !sameBind(got, c.want) {
			t.Errorf("numeric target: %s: %#v = %#v, want %#v", c.why, c.in, got, c.want)
		}
	}
}

// TestMySQLBindBitFoldsPeerBooleanText keeps the BIT route (the fix this seam was
// built for) intact beside the new numeric fold, including the two single-letter
// spellings that share the fold's vocabulary.
func TestMySQLBindBitFoldsPeerBooleanText(t *testing.T) {
	bit := Must(EngineMySQL).BindFixups([]string{"bit"})[0]
	if bit == nil {
		t.Fatal("BIT target has no adapter")
	}
	for _, c := range []struct {
		in   interface{}
		want interface{}
		why  string
	}{
		{"1", []byte{1}, "pg/dm text one"},
		{"0", []byte{0}, "pg/dm text zero"},
		{"true", []byte{1}, "word true"},
		{"false", []byte{0}, "word false"},
		{"t", []byte{1}, "pg single letter"},
		{"f", []byte{0}, "pg single letter"},
		{[]byte{0x30}, []byte{0x30}, "a real BIT(8) payload is not the text \"0\""},
		{"0x30", "0x30", "an undecoded token is left alone"},
		{true, []byte{1}, "a Go bool"},
		{false, []byte{0}, "a Go bool"},
	} {
		if !sameBind(bit(c.in), c.want) {
			t.Errorf("BIT target: %s: %#v = %#v, want %#v", c.why, c.in, bit(c.in), c.want)
		}
	}
}

// TestMySQLBindBitWideKeepsOwnPayloadText is the F5 assertion. MySQL writes its own
// bit(8) payload under --blob-encoding auto and raw as the storage byte inside the
// quotes (measured product bytes: `1,"0","<0x01>"` for B8=0x30 and B1=1), so on a
// column wider than one bit the text "0" is a payload the server reads back as 0x30 =
// 48 — folding it to the byte 0 is what turned 48 into 0 and 49 into 1 on a plain
// same-engine round trip, with exit code 0 and nothing in the log. Only a value that
// cannot be text keeps an adapter here.
func TestMySQLBindBitWideKeepsOwnPayloadText(t *testing.T) {
	wide := Must(EngineMySQL).BindFixups([]string{"bit(8)"})[0]
	if wide == nil {
		t.Fatal("bit(8) target has no adapter")
	}
	for _, c := range []struct {
		in   interface{}
		want interface{}
		why  string
	}{
		{"0", "0", "MySQL's own 0x30 payload, quoted as the character 0"},
		{"1", "1", "the same for 0x31 = 49"},
		{"t", "t", "0x74 is a legal bit(8) payload"},
		{"false", "false", "a word is payload bytes on a wide column"},
		{"AB", "AB", "a two-byte payload stays the text the server reads as bytes"},
		{"", "", "empty text"},
		{[]byte{0x30}, []byte{0x30}, "a decoded payload passes through"},
		{true, []byte{1}, "a Go bool is not a file spelling and still becomes a byte"},
		{false, []byte{0}, "a Go bool"},
		{nil, nil, "NULL"},
		{int64(7), int64(7), "a typed value keeps its type"},
	} {
		if !sameBind(wide(c.in), c.want) {
			t.Errorf("bit(8) target: %s: %#v = %#v, want %#v", c.why, c.in, wide(c.in), c.want)
		}
	}
}

// TestMySQLBitWidthReadsTheCatalogueSuffix pins the width key the two BIT adapters are
// chosen with. The bare word is MySQL's own default (bit(1)), so a missing or
// unparsable suffix folds — the safe direction, since a 1-bit column rejects the ASCII
// byte outright.
func TestMySQLBitWidthReadsTheCatalogueSuffix(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{"bit", 1},
		{"BIT", 1},
		{" bit(1) ", 1},
		{"bit(8)", 8},
		{"BIT(64)", 64},
		{"bit(16)", 16},
		{"bit(128)", 64}, // above the server's maximum, clamped
		{"bit()", 1},     // unparsable → the folding default
		{"bit(x)", 1},    // unparsable → the folding default
		{"bit(0)", 1},    // MySQL has no zero-width BIT; treat it as the minimum
	} {
		if got := mysqlBitWidth(c.in); got != c.want {
			t.Errorf("mysqlBitWidth(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestBinaryFlagSurvivesTheWidthSuffix is the one reason the catalogue had to be
// precise without moving the goalposts: a BIT column stays a binary column whether the
// word arrives bare from the driver or width-suffixed from information_schema, or the
// import path would stop treating it as raw payload.
func TestBinaryFlagSurvivesTheWidthSuffix(t *testing.T) {
	m := Must(EngineMySQL)
	for _, word := range []string{"bit", "BIT", "bit(1)", "bit(8)", "BLOB", "varbinary(16)", "mediumblob"} {
		if !m.MetadataBinary(word) {
			t.Errorf("MetadataBinary(%q) = false, want true", word)
		}
	}
	for _, word := range []string{"varchar(64)", "text", "tinyint(1)", "int", "date", ""} {
		if m.MetadataBinary(word) {
			t.Errorf("MetadataBinary(%q) = true, want false", word)
		}
	}
}

// TestBaseTypeNameIsTheCatalogueWord pins the one normalisation both adapters are
// keyed to: information_schema answers lowercase and without a length, the driver's
// own type words carry an UNSIGNED prefix, and a hand-built list may hold either.
// Width suffixes must not decide the class, because TINYINT(1) and BIGINT(20) are
// both numbers to the server.
func TestBaseTypeNameIsTheCatalogueWord(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"bit", "BIT"},
		{" bit(8) ", "BIT"},
		{"TINYINT(1)", "TINYINT"},
		{"bigint(20) unsigned", "BIGINT"},
		{"UNSIGNED BIGINT", "UNSIGNED BIGINT"},
		{"varchar(64)", "VARCHAR"},
		{"", ""},
	} {
		if got := baseTypeName(c.in); got != c.want {
			t.Errorf("baseTypeName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
