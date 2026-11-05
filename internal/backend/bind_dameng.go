package backend

import (
	"math/big"
	"strconv"
	"strings"

	"gitee.com/chunanyong/dm"
)

// This file answers one question for the import path: what Go value has to reach
// the Dameng driver's placeholder for a value to land in a column as the number
// the file carries.
//
// The reason it exists is that the driver's text path is not exact. `fromString`
// in zi.go (gitee.com/chunanyong/dm v1.8.23) hands a NUMBER, DECIMAL, NUMERIC,
// REAL or DOUBLE target to `new(big.Float).SetString(val)` — 64 bits of
// significand, about 19 decimal digits — and an integer target to that same
// parse followed by `f.Int()`, which truncates toward zero and then *wraps*
// silently when the result does not fit an int64. Measured on a live DM8 against
// NUMBER and NUMERIC(38,10), binding the file's text directly:
//
//	899999999999999.99999  lands 900000000000000
//	999999999999999.99999  lands 1000000000000000
//	999999999999990.12345  lands 999999999999990.12350
//	199999999999999.99999  lands 199999999999999.99998
//	99999999999999999999   into BIGINT lands 7766279631452241920
//
// The same values bound as a `dm.DmDecimal` land exactly (each asserted with
// `CASE WHEN N = <the literal> THEN 'EQ' END`, because TO_CHAR only prints 20
// significant digits of a NUMBER). The three rules below are what that measures
// into code, and each guard in them is a number that was observed, not guessed.
//
// A raw byte payload arrives here too. Parquet stores every column as a
// BYTE_ARRAY, and a MySQL BIT(n) column's payload is its stored bytes, so the
// reader can only learn "these bytes are content, not text" from the file's own
// schema (no String logical type) and needs the target type to say what to do
// with them. Measured binding the raw bytes of MySQL BIT(8) = 0xAA: a DM
// SMALLINT, BIGINT and NUMERIC target all answer `Error 6007: Data convertion
// error`, while the unsigned big-endian integer *text* of the same payload
// lands 170 on all three (aa→170, ff→255, 55→85, 01→1, 00→0, 0001→1, and
// 0x0102030405060708→72623859790382856).
const (
	// dmMaxBindDigits is the significant-digit limit past which a decimal
	// target keeps the value as it arrived. Measured digit by digit into
	// NUMERIC(38,0): the text path drifts at 36 and 37 digits and fails at 38,
	// the exact path holds 36, 37 and 38, and both answer the server's own
	// overflow error at 39 and 40 — DM's NUMBER takes 38 digits of precision,
	// so nothing past the guard could have been stored anyway, and passing the
	// text through reproduces the identical server outcome.
	dmMaxBindDigits = 38

	// dmMaxCoeffDigits is the number of significant digits the driver will
	// actually build a decimal from: checkPrec() in o.go rejects more than
	// XDEC_MAX_PREC (40) outright. Past it an integer target gets
	// dmOverflowDigits instead of the original text, because the original
	// would take the driver's wrapping path.
	dmMaxCoeffDigits = 40

	// dmMaxBindBits caps the binary precision the fractional case asks
	// big.Float for — 4 bits per decimal digit plus 64, measured to print back
	// the exact decimal it was given. 1600 bits is the highest value the probe
	// ran through; a longer literal is pathological input and keeps the text.
	dmMaxBindBits = 1600

	// dmMaxRawBytes is the longest byte payload read as an unsigned integer.
	// MySQL's BIT tops out at 64 bits, and a longer run of bytes is not a
	// number anything stores — a BLOB or RAW column arrives as bytes for a
	// binary target, which is a different question entirely.
	dmMaxRawBytes = 8

	// dmOverflowDigits is 40 nines: the largest coefficient the driver will
	// build, which overflows every integer type DM has (BIGINT holds 19 digits).
	// It is what an unbuildable integer gets clamped to so the server reports
	// `Error -6102 Digital overflow` instead of the driver wrapping the value.
	dmOverflowDigits = "9999999999999999999999999999999999999999"
)

// damengBindFixup returns the value adapter one Dameng target column needs, or
// nil when the value must reach the driver exactly as the reader produced it.
// The classification itself lives in the Dameng type table, so a type cannot be
// known to the dump and unknown to the import.
func damengBindFixup(name string) func(interface{}) interface{} {
	bind, ok := damengBindOf(name)
	if !ok {
		return nil
	}
	switch bind {
	case bindExactNumber:
		return dmBindExactNumber
	case bindInteger:
		return dmBindInteger
	case bindFloatNumber:
		return dmBindFloatNumber
	case bindBit:
		return dmBindBit
	case bindText:
		return dmBindText
	case bindRaw:
		// Bytes are what these columns store, and bytes are what the readers
		// already hand over — so this is the one classification with nothing to
		// do, and returning nil keeps LOB cells out of a call per value.
		return nil
	}
	return nil
}

// dmBindExactNumber binds a NUMBER, DECIMAL, DEC or NUMERIC target exactly.
func dmBindExactNumber(v interface{}) interface{} {
	text, ok := dmBindNumberText(v)
	if !ok {
		return v
	}
	coeff, fracLen, ok := dmSplitDecimal(text)
	if !ok {
		return v
	}
	d, ok := dmExactDecimal(coeff, fracLen)
	if !ok {
		return v
	}
	return d
}

// dmBindInteger binds a TINYINT, SMALLINT, INT, INTEGER or BIGINT target.
//
// The rounding first is not cosmetic: text bound into these columns is rounded
// half away from zero by the driver (measured into BIGINT — 2.5→3, -2.5→-3,
// 0.5→1, -0.5→-1, 3.5→4, -3.5→-4, 1.5→2, -1.5→-2, 2.4→2, 2.6→3), while a raw
// DmDecimal is truncated (2.5→2). Rounding the coefficient the same way keeps
// every in-range value landing where it lands today and changes only the
// out-of-range ones, which then fail with the server's own overflow error
// instead of wrapping. Measured: 99999999999999999999 into BIGINT now answers
// `Error -6102 Digital overflow` where the text path stored
// 7766279631452241920.
func dmBindInteger(v interface{}) interface{} {
	text, ok := dmBindNumberText(v)
	if !ok {
		return v
	}
	coeff, fracLen, ok := dmSplitDecimal(text)
	if !ok {
		return v
	}
	n := dmRoundHalfAway(coeff, fracLen)
	if dmSigDigits(n) > dmMaxCoeffDigits {
		// Unbuildable, and far outside any integer column's range: hand the
		// server the largest coefficient it will build so it reports the
		// overflow instead of the driver wrapping the value.
		overflow, _ := new(big.Int).SetString(dmOverflowDigits, 10)
		if coeff.Sign() < 0 {
			overflow.Neg(overflow)
		}
		n = overflow
	}
	d, err := dm.NewDecimalFromBigInt(n)
	if err != nil {
		return v
	}
	return *d
}

// dmBindFloatNumber binds a FLOAT, REAL, DOUBLE or BINARY_DOUBLE target.
//
// Only the payload form is corrected here. A binary-float column has nowhere to
// put a twentieth significant digit, so the driver's 64-bit parse of the file's
// text is already the right answer — measured, the two paths differ nowhere in
// this family. A raw payload becomes unsigned integer text first, which lands
// 1.7E2, 2.55E2 and 8.5E1 on FLOAT for aa, ff and 55.
func dmBindFloatNumber(v interface{}) interface{} {
	switch x := v.(type) {
	case []byte:
		if t, ok := dmRawUintText(x); ok {
			return t
		}
		return string(x)
	case string:
		if t, ok := dmHexTokenText(x); ok {
			return t
		}
	}
	return v
}

// dmBindBit binds a BIT or BOOLEAN target.
//
// The driver reads text for these columns with `toBool`, which recognises "0",
// "1", "true" and "false" and answers false for everything else — measured,
// "0x01" lands 0 and "170" lands 0, which is the whole of defect D2. DM's own
// rule for a number going into a BIT column is nonzero→1 (measured with the
// literals 2, 170 and -1, all 1), so the adapters reproduces that rule for
// every form a file can carry and hands back one canonical digit.
//
// The forms a file carries, measured on this round's harness: a Parquet file
// gives the payload as bytes; a DEL dump written with --hex-blob gives an 0x…
// token; a DEL dump written without it gives the stored bytes inside the field's
// own quotes, because MySQL only forces hex when the dump's charset cannot carry
// the bytes (HexBlobMandatory is false). The third form is a string, and it is
// what BIT(1) arrives as — 0x00 and 0x01, which no reader can call text — so a
// run of bytes with no printable byte in it is read as the payload it is. A run
// that does contain printable bytes may be the column's own text, and guessing
// would put a wrong flag in the column instead of the 0 the driver answers, so
// that value keeps the path it has always had: see the note in
// docs/requirements.md 2.1.12 on what a cross-engine DEL dump has to be written
// with.
func dmBindBit(v interface{}) interface{} {
	switch x := v.(type) {
	case bool:
		if x {
			return "1"
		}
		return "0"
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true":
			return "1"
		case "false":
			return "0"
		}
		if t, ok := dmBitOfText(x); ok {
			return t
		}
		if dmIsPayloadText(x) {
			return dmBitOfBytes([]byte(x))
		}
	case []byte:
		if len(x) == 0 {
			return "0"
		}
		if len(x) <= dmMaxRawBytes {
			return dmBitOfBytes(x)
		}
	case int64:
		return dmBitOfInt(x != 0)
	case int32:
		return dmBitOfInt(x != 0)
	case int:
		return dmBitOfInt(x != 0)
	case uint64:
		return dmBitOfInt(x != 0)
	case float64:
		return dmBitOfInt(x != 0)
	case float32:
		return dmBitOfInt(x != 0)
	}
	return v
}

// dmIsPayloadText reports the strings that are bytes rather than words: at least
// one byte, at most the widest MySQL BIT (eight), and nothing in the printable
// ASCII range. Measured on the raw-byte DEL dump this round's harness exports:
// BIT(1) 1 is "\x01", BIT(8) 170 is "\xaa", BIT(16) 65535 is "\xff\xff", and each
// of them is a value the driver's toBool silently lands 0.
func dmIsPayloadText(s string) bool {
	if s == "" || len(s) > dmMaxRawBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x20 && s[i] < 0x7f {
			return false
		}
	}
	return true
}

// dmBindText is a character or date/time target: text is what belongs there, and
// a raw payload becomes the text it was stored as.
func dmBindText(v interface{}) interface{} {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}

// dmBindNumberText turns one value into the decimal text a numeric target should
// be given, without binding anything: bytes become the unsigned integer they
// carry, a 0x token becomes its own number, and text is text. It is the shared
// first step of the two decimal adapters.
func dmBindNumberText(v interface{}) (string, bool) {
	switch x := v.(type) {
	case string:
		if t, ok := dmHexTokenText(x); ok {
			return t, true
		}
		return x, true
	case []byte:
		return dmRawUintText(x)
	case bool:
		return dmBindBit(x).(string), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case int32:
		return strconv.FormatInt(int64(x), 10), true
	case int:
		return strconv.FormatInt(int64(x), 10), true
	case uint64:
		return strconv.FormatUint(x, 10), true
	}
	return "", false
}

// dmSplitDecimal splits a plain decimal literal into a signed coefficient and
// the number of digits that belong after the point. Anything an exact decimal
// cannot be built from — an exponent, an infinity, a stray letter, a comma — is
// refused, and the caller leaves the value alone. Measured: 1.7976931348623157E+308
// is refused by the server either way (`Error 6092 The data is too large`), so
// rejecting the form costs nothing and keeps a float parse out of the path.
//
// A point with digits on either side is a decimal literal: ".5" is 0.5 and "5."
// is 5 with nothing after the point, which is the form the driver itself prints
// such a value in (its String() lays the point after the digits and then strips
// one trailing zero). A bare "." carries no digits and is refused.
func dmSplitDecimal(s string) (*big.Int, int, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil, 0, false
	}
	neg := false
	if t[0] == '+' || t[0] == '-' {
		neg = t[0] == '-'
		t = t[1:]
	}
	if i := strings.IndexByte(t, '.'); i >= 0 {
		if strings.IndexByte(t[i+1:], '.') >= 0 {
			return nil, 0, false
		}
		whole, frac := t[:i], t[i+1:]
		if whole == "" && frac == "" {
			return nil, 0, false
		}
		if whole == "" {
			whole = "0"
		}
		if !dmAllDigits(whole) || !dmDigitsOrEmpty(frac) {
			return nil, 0, false
		}
		coeff, ok := new(big.Int).SetString(whole+frac, 10)
		if !ok {
			return nil, 0, false
		}
		if neg {
			coeff.Neg(coeff)
		}
		return coeff, len(frac), true
	}
	if !dmAllDigits(t) {
		return nil, 0, false
	}
	coeff, ok := new(big.Int).SetString(t, 10)
	if !ok {
		return nil, 0, false
	}
	if neg {
		coeff.Neg(coeff)
	}
	return coeff, 0, true
}

func dmAllDigits(s string) bool {
	if s == "" {
		return false
	}
	return dmDigitsOrEmpty(s)
}

// dmDigitsOrEmpty is dmAllDigits with the empty run allowed, which is what a
// trailing point ("5.") and a leading one (".5", whose whole part is filled with
// "0") both need. At least one digit is still required by the caller that checks
// the other half.
func dmDigitsOrEmpty(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// dmSigDigits counts the significant digits of a coefficient: the digits that
// are left once the sign and the leading zeros are gone. Zero has none.
func dmSigDigits(n *big.Int) int {
	return len(strings.TrimLeft(n.String(), "-0"))
}

// dmRoundHalfAway rounds a scaled coefficient to an integer, halves away from
// zero — n = (2·|c| + 10^s) / (2·10^s), then the sign is put back. The absolute
// value is what makes it away-from-zero: dividing the signed coefficient
// directly rounds -2.5 to -2, which the measured text path never does.
func dmRoundHalfAway(coeff *big.Int, scale int) *big.Int {
	if scale == 0 {
		return new(big.Int).Set(coeff)
	}
	denom := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	n := new(big.Int).Abs(coeff)
	n.Lsh(n, 1)
	n.Add(n, denom)
	denom.Lsh(denom, 1)
	n.Div(n, denom)
	if coeff.Sign() < 0 {
		n.Neg(n)
	}
	return n
}

// dmExactDecimal builds the driver's own decimal for a coefficient and scale.
//
// The integer case is a direct big.Int, which is exact by construction. The
// fractional case goes through a big.Float carrying enough bits that the
// shortest accurate decimal of the quotient is still the literal that came in
// (4 bits per digit plus 64, measured against the list in this file's header),
// because DmDecimal's scale field is unexported and NewDecimalFromBigFloat is
// the only constructor that can express a fraction.
func dmExactDecimal(coeff *big.Int, scale int) (interface{}, bool) {
	digits := dmSigDigits(coeff)
	if digits > dmMaxBindDigits {
		return nil, false
	}
	if scale == 0 {
		d, err := dm.NewDecimalFromBigInt(coeff)
		if err != nil {
			return nil, false
		}
		return *d, true
	}
	bits := 4*(digits+scale) + 64
	if bits > dmMaxBindBits {
		return nil, false
	}
	f := big.NewFloat(0).SetPrec(uint(bits))
	if _, ok := f.SetString(coeff.String()); !ok {
		return nil, false
	}
	denom := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	f.Quo(f, new(big.Float).SetPrec(uint(bits)).SetInt(denom))
	d, err := dm.NewDecimalFromBigFloat(f)
	if err != nil {
		return nil, false
	}
	return *d, true
}

// dmRawUintText renders a raw byte payload as the unsigned big-endian integer it
// carries, which is the number a MySQL BIT column of that width stores.
func dmRawUintText(b []byte) (string, bool) {
	if len(b) == 0 || len(b) > dmMaxRawBytes {
		return "", false
	}
	acc, mul := big.NewInt(0), big.NewInt(256)
	for _, c := range b {
		acc.Mul(acc, mul)
		acc.Add(acc, big.NewInt(int64(c)))
	}
	return acc.String(), true
}

// dmHexTokenText turns an unquoted 0x token — the form the DEL writer uses for a
// binary column — into the decimal text of the number it holds, and refuses
// anything that is not a short hex literal. The grammar is DelReader's own
// decoder (decodeHexField in sql_reader.go: "0x" or "0X", an even number of hex
// digits, at least one), so a token the reader would have decoded for a binary
// column is the same token a numeric column now reads as a number.
//
// A token longer than the eight bytes MySQL's widest BIT can hold stays text: it
// is not a bit field, and leaving it alone is exactly what happened before this
// existed.
func dmHexTokenText(s string) (string, bool) {
	if len(s) < 4 || s[0] != '0' || (s[1] != 'x' && s[1] != 'X') {
		return "", false
	}
	hex := s[2:]
	if len(hex) > dmMaxRawBytes*2 || len(hex)%2 != 0 {
		return "", false
	}
	v, ok := new(big.Int).SetString(hex, 16)
	if !ok {
		return "", false
	}
	return v.String(), true
}

// dmBitOfText reduces a plain decimal (or hex) literal to DM's own BIT answer:
// one if any digit is nonzero, zero otherwise. Exponent forms are refused, so
// such a value keeps the text path it always had.
func dmBitOfText(s string) (string, bool) {
	if t, ok := dmHexTokenText(s); ok {
		s = t
	}
	seen, nonzero := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '+' || c == '-' || c == '.':
		case c >= '0' && c <= '9':
			seen = true
			if c != '0' {
				nonzero = true
			}
		default:
			return "", false
		}
	}
	if !seen {
		return "", false
	}
	return dmBitOfInt(nonzero), true
}

func dmBitOfBytes(b []byte) string {
	for _, c := range b {
		if c != 0 {
			return "1"
		}
	}
	return "0"
}

func dmBitOfInt(nonzero bool) string {
	if nonzero {
		return "1"
	}
	return "0"
}
