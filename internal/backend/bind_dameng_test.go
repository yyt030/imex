package backend

import (
	"fmt"
	"math/big"
	"strings"
	"testing"

	"gitee.com/chunanyong/dm"
)

// bindRender renders what one adapter hands to the driver, as the text a test can
// compare on. A dm.DmDecimal prints its own exact decimal form (String() lays
// digits and weight back out) behind a "#", because several inputs would print the
// same text whether the adapter built a decimal or left the string alone — ".5"
// is the proof. Everything else goes through its Go rendering, and a value the
// adapter handed over as it found it is reported by its own text (a string) or as
// "@<type>", since half of these cases exist precisely to pin that the original
// reached the driver.
func bindRender(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return "<nil>"
	case dm.DmDecimal:
		return "#" + x.String()
	case string:
		return x
	case []byte:
		return "@bytes"
	default:
		return "@" + fmt.Sprintf("%T", v)
	}
}

// TestDamengBindExactNumber pins the decimal adapter. The input side is the
// three shapes a reader can hand over — the file's text, a 0x token, a raw
// payload — and the expected side is the exact decimal the driver must be given,
// or the value itself where the rules say to stay out of the way.
//
// The five drifting cases are measured text binds against a live DM8 (see
// bind_dameng.go for the table); every one of them lands exact through this
// adapter, which is what the live test in test/integration re-asserts.
func TestDamengBindExactNumber(t *testing.T) {
	for _, c := range []struct {
		in   interface{}
		want string
	}{
		// Exact where the driver's 64-bit parse is not.
		{"899999999999999.99999", "#899999999999999.99999"},
		{"999999999999999.99999", "#999999999999999.99999"},
		{"999999999999990.12345", "#999999999999990.12345"},
		{"199999999999999.99999", "#199999999999999.99999"},
		{"-899999999999999.99999", "#-899999999999999.99999"},
		// Exact both ways, kept so the fix is not paid for by a change.
		{"123456789012345.12345", "#123456789012345.12345"},
		{"0.00001", "#0.00001"},
		{"-1.234555", "#-1.234555"},
		{"9223372036854775807", "#9223372036854775807"},
		{"-9223372036854775808", "#-9223372036854775808"},
		{"2.5", "#2.5"},
		// Zero in all the shapes a dump carries it in.
		{"0", "#0"},
		{"0.00000", "#0"},
		{"-0.0", "#0"},
		{"00123.4500", "#123.45"},
		{".5", "#.5"},
		{"5.", "#5"},
		// The "#" rows are the driver's own rendering, and it is not Go's: a
		// value below one prints with no leading zero, and a point with nothing
		// after it is folded away. Both are exact text for the number the
		// server is handed, which is what these adapters are for. A bare point
		// has no digits at all and stays the file's own text.
		{".", "."},
		// 38 significant digits is the widest DM's NUMBER holds, and the exact
		// path holds it where the text path drifts at 36 and fails at 38.
		{"12345678901234567890123456789012345678", "#12345678901234567890123456789012345678"},
		{"99999999999999999999", "#99999999999999999999"},
		// Past the guard the text is handed over unchanged, which the server
		// rejects the same way it rejects the exact value.
		{"123456789012345678901234567890123456789", "123456789012345678901234567890123456789"},
		// Forms an exact decimal cannot be built from stay text.
		{"1e3", "1e3"},
		{"1.7976931348623157E+308", "1.7976931348623157E+308"},
		{"+Inf", "+Inf"},
		{"NaN", "NaN"},
		{"1,234", "1,234"},
		{"", ""},
		{"not a number", "not a number"},
		// The two import-side shapes that carry no decimal text at all.
		{[]byte{0xaa}, "#170"},
		{[]byte{0x00}, "#0"},
		{[]byte{0x00, 0x01}, "#1"},
		{[]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, "#18446744073709551615"},
		{[]byte("0123456789"), "@bytes"},
		{"0x01", "#1"},
		{"0xaa", "#170"},
		{"0XFF", "#255"},
		{"0x0102030405060708", "#72623859790382856"},
		{"0x1234567890abcdef", "#1311768467294899695"},
		// Eight bytes is the widest MySQL BIT holds, and a longer run of hex is
		// not a number, so it keeps the text the file has.
		{"0x001234567890abcdef", "0x001234567890abcdef"},
		{"0x001234567890abcdef01", "0x001234567890abcdef01"},
		{"0xyz", "0xyz"},
		// Go's own numbers (a Parquet file written by another tool).
		{int64(42), "#42"},
		{true, "#1"},
	} {
		got := dmBindExactNumber(c.in)
		if s := bindRender(got); s != c.want {
			t.Errorf("dmBindExactNumber(%#v) = %s, want %s", c.in, s, c.want)
		}
	}
}

// TestDamengBindInteger pins the integer adapter, whose two jobs are to keep the
// rounding the measured text path produces and to make an out-of-range value
// fail instead of wrapping.
func TestDamengBindInteger(t *testing.T) {
	for _, c := range []struct {
		in   interface{}
		want string
	}{
		// Measured against a live DM8 as the text path's own answers.
		{"2.5", "#3"},
		{"-2.5", "#-3"},
		{"0.5", "#1"},
		{"-0.5", "#-1"},
		{"1.5", "#2"},
		{"-1.5", "#-2"},
		{"3.5", "#4"},
		{"-3.5", "#-4"},
		{"2.4", "#2"},
		{"2.6", "#3"},
		{"0.4", "#0"},
		{"0.0", "#0"},
		{"-0.4", "#0"},
		// A point with nothing on one side is still a decimal literal: ".5" is
		// the measured half-up answer 1, "5." is the integer it spells.
		{".5", "#1"},
		{"5.", "#5"},
		{"9223372036854775807", "#9223372036854775807"},
		{"-9223372036854775808", "#-9223372036854775808"},
		// The wraparound defect: the value goes to the server as itself, and
		// the server's own range check is what rejects it.
		{"99999999999999999999", "#99999999999999999999"},
		{"-99999999999999999999", "#-99999999999999999999"},
		// Past what the driver will build at all, the magnitude is clamped to
		// the widest coefficient it accepts — 40 nines, which overflows every
		// integer type DM has, so the row fails loudly instead of silently.
		{"9999999999999999999999999999999999999999999", "#" + strings.Repeat("9", 40)},
		{"-9999999999999999999999999999999999999999999", "#-" + strings.Repeat("9", 40)},
		// Payloads and tokens become their own unsigned value first.
		{[]byte{0xaa}, "#170"},
		{[]byte{0x7f}, "#127"},
		{"0x7fffffffffffffff", "#9223372036854775807"},
		// Go's own numbers (a Parquet file written by another tool).
		{int64(7), "#7"},
		{true, "#1"},
		// And what is not a number is not touched.
		{"1e3", "1e3"},
		{"abc", "abc"},
	} {
		got := dmBindInteger(c.in)
		if s := bindRender(got); s != c.want {
			t.Errorf("dmBindInteger(%#v) = %s, want %s", c.in, s, c.want)
		}
	}
}

// TestDamengBindBit pins the BIT and BOOLEAN adapter. The measured failure is
// that the driver's text path answers 0 for anything it cannot name: "0x01" and
// "170" both landed 0 in a BIT column while the server's own rule for a number
// going into one is nonzero→1.
func TestDamengBindBit(t *testing.T) {
	for _, c := range []struct {
		in   interface{}
		want string
	}{
		{"0", "0"},
		{"1", "1"},
		{"true", "1"},
		{"TRUE", "1"},
		{"false", "0"},
		{"2", "1"},
		{"170", "1"},
		{"-1", "1"},
		{"0.5", "1"},
		{"0.0", "0"},
		{"000", "0"},
		{"0x01", "1"},
		{"0x00", "0"},
		{"0xaa", "1"},
		{[]byte{0xaa}, "1"},
		{[]byte{0x00}, "0"},
		{[]byte{0x00, 0x00}, "0"},
		{[]byte{0x00, 0x01}, "1"},
		{true, "1"},
		{false, "0"},
		{int64(0), "0"},
		{int64(7), "1"},
		// A MySQL DEL dump written without --hex-blob carries the BIT column's
		// stored bytes inside the field's quotes, so the adapter is handed a
		// string of them. No printable byte in the run means it is not a word.
		{"\x01", "1"},
		{"\x00", "0"},
		{"\xaa", "1"},
		{"\xff\xff", "1"},
		{"\x00\x00", "0"},
		// A run that does contain printable bytes may be the column's text, and
		// a too-long run is not a BIT payload either: both keep the path they had.
		{"A", "A"},
		{" \x01", " \x01"},
		{strings.Repeat("\x01", 9), strings.Repeat("\x01", 9)},
		// A word the driver would have read as false anyway stays its text.
		{"yes", "yes"},
		{"", ""},
		{"1e3", "1e3"},
	} {
		got := dmBindBit(c.in)
		if s := bindRender(got); s != c.want {
			t.Errorf("dmBindBit(%#v) = %s, want %s", c.in, s, c.want)
		}
	}
}

// TestDamengBindTextAndFloat pins the two adapters whose only job is the raw
// payload: a text column must receive the text it received before, and a float
// column must receive the number the payload carries.
func TestDamengBindTextAndFloat(t *testing.T) {
	if got := bindRender(dmBindText([]byte("abc"))); got != "abc" {
		t.Errorf("dmBindText bytes = %q, want abc", got)
	}
	if got := bindRender(dmBindText("abc")); got != "abc" {
		t.Errorf("dmBindText text = %q, want abc", got)
	}
	if got := bindRender(dmBindText(nil)); got != "<nil>" {
		t.Errorf("dmBindText nil = %q, want <nil>", got)
	}
	for _, c := range []struct {
		in   interface{}
		want string
	}{
		// A payload of up to eight bytes is the unsigned number it carries, and
		// nothing else: bytes only reach an adapter from a column the file says
		// is raw, so "12.5" in four bytes of ASCII is 825372213, not a decimal.
		// Longer than MySQL's widest BIT, the run of bytes stays its own text.
		{[]byte{0xaa}, "170"},
		{[]byte{0x55}, "85"},
		{[]byte("12.5"), "825372213"},
		{[]byte("0123456789"), "0123456789"},
		{"1.7976931348623157E+308", "1.7976931348623157E+308"},
		{"0xaa", "170"},
		{"0x1234567890abcdef", "1311768467294899695"},
		{"12.5", "12.5"},
		{int64(5), "@int64"},
	} {
		got := bindRender(dmBindFloatNumber(c.in))
		if got != c.want {
			t.Errorf("dmBindFloatNumber(%#v) = %s, want %s", c.in, got, c.want)
		}
	}
	// A binary target is the one classification with nothing to do: the readers
	// already hand over bytes.
	if f := damengBindFixup("BLOB"); f != nil {
		t.Error("BLOB must not have a bind adapter")
	}
	if f := damengBindFixup("WEIRD_TYPE"); f != nil {
		t.Error("an unknown type name must not have a bind adapter")
	}
}

// TestDamengBindHelpers pins the pure parts, including the two shapes that were
// measured as traps: a signed division rounds -2.5 to -2, and a big.Int literal
// past int64 comes back non-nil with wrapped digits.
func TestDamengBindHelpers(t *testing.T) {
	for _, c := range []struct {
		text  string
		coeff string
		scale int
		ok    bool
	}{
		{"123.45", "12345", 2, true},
		{"-123.45", "-12345", 2, true},
		{".5", "5", 1, true},
		{"5.", "5", 0, true},
		{".", "", 0, false},
		{"0", "0", 0, true},
		{"000", "0", 0, true},
		{"1e3", "", 0, false},
		{"1.2.3", "", 0, false},
		{" 7 ", "7", 0, true},
		{"", "", 0, false},
		{"0x11", "", 0, false},
	} {
		coeff, scale, ok := dmSplitDecimal(c.text)
		if ok != c.ok {
			t.Errorf("dmSplitDecimal(%q) ok = %v, want %v", c.text, ok, c.ok)
			continue
		}
		if ok && coeff.String() != c.coeff {
			t.Errorf("dmSplitDecimal(%q) coeff = %s, want %s", c.text, coeff, c.coeff)
		}
		if ok && scale != c.scale {
			t.Errorf("dmSplitDecimal(%q) scale = %d, want %d", c.text, scale, c.scale)
		}
	}

	// The pairs are the measured text path's own answers, written as the
	// coefficient and scale dmSplitDecimal produces: 2.5 becomes (25, 1) and
	// the server's parse rounds it to 3, while 0.12345 becomes (12345, 5) and
	// rounds down to 0.
	for _, c := range []struct {
		coeff string
		scale int
		want  string
	}{
		{"5", 1, "1"},
		{"-5", 1, "-1"},
		{"25", 1, "3"},
		{"-25", 1, "-3"},
		{"24", 1, "2"},
		{"26", 1, "3"},
		{"4999", 3, "5"},
		{"5001", 3, "5"},
		{"49999999999999999999", 19, "5"},
		{"12345", 5, "0"},
		{"99999", 5, "1"},
	} {
		in, _ := new(big.Int).SetString(c.coeff, 10)
		got := dmRoundHalfAway(in, c.scale)
		if got.String() != c.want {
			t.Errorf("dmRoundHalfAway(%s, %d) = %s, want %s", c.coeff, c.scale, got, c.want)
		}
	}

	for _, c := range []struct {
		in   []byte
		want string
		ok   bool
	}{
		{[]byte{0xaa}, "170", true},
		{[]byte{0x00}, "0", true},
		{[]byte{0x00, 0x01}, "1", true},
		{[]byte{0xff, 0xff}, "65535", true},
		{[]byte{}, "", false},
		{[]byte{1, 2, 3, 4, 5, 6, 7, 8, 9}, "", false},
	} {
		got, ok := dmRawUintText(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("dmRawUintText(%v) = %q/%v, want %q/%v", c.in, got, ok, c.want, c.ok)
		}
	}

	for _, c := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"0x01", "1", true},
		{"0xff", "255", true},
		{"0XFF", "255", true},
		{"0x1234567890abcdef", "1311768467294899695", true},
		// Eight bytes is the cap, so 16 hex digits is in and 18 is out.
		{"0x001234567890abcdef", "", false},
		{"0x001234567890abcdef01", "", false},
		{"0xf", "", false},
		{"0x", "", false},
		{"0xzz", "", false},
		{"1abc", "", false},
	} {
		got, ok := dmHexTokenText(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("dmHexTokenText(%q) = %q/%v, want %q/%v", c.in, got, ok, c.want, c.ok)
		}
	}

	for _, c := range []struct {
		in   string
		want int
	}{
		{"0", 0},
		{"0.000", 0},
		{"-0", 0},
		{"00123", 3},
		// Trailing zeros count: this is the number of digits the driver has to
		// carry for the coefficient, not the significant figures a mathematician
		// would read — and the 38-digit guard is about the digits carried.
		{"1000", 4},
		{"-12345678901234567890123456789012345678", 38},
		{"123456789012345678901234567890123456789", 39},
	} {
		coeff, _, ok := dmSplitDecimal(c.in)
		if !ok {
			t.Fatalf("dmSplitDecimal(%q) refused a plain literal", c.in)
		}
		if got := dmSigDigits(coeff); got != c.want {
			t.Errorf("dmSigDigits(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
