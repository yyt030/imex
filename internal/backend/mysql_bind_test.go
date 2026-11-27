package backend

import "testing"

// TestMySQLBindFixupsSelectsByTargetType pins which MySQL target columns get a bind
// adapter, from the DATA_TYPE words information_schema answers with. Three answers
// are possible and the adapter is named by what it does, not by a pointer
// comparison: a BIT target folds the text "1" to the bit byte, a numeric target
// folds pg's "t" to the integer text "1" and leaves "1" alone, and everything else
// gets nil so the value binds exactly as the reader produced it.
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
		// BIT, in every spelling the catalogue and the driver use.
		{"bit", "bit"},
		{"BIT", "bit"},
		{"bit(1)", "bit"},
		{"bit(8)", "bit"},
		{"BIT(64)", "bit"},
		{" bit(8) ", "bit"},

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
		got := "none"
		if f != nil {
			switch {
			case sameBind(f("1"), []byte{1}):
				got = "bit"
			case sameBind(f("t"), "1"):
				got = "num"
			default:
				got = "unknown adapter"
			}
		}
		if got != c.want {
			t.Errorf("BindFixups(%q) = %s, want %s", c.dt, got, c.want)
		}
	}
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
