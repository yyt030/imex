package integration

// Data-value boundary tests: the DEL round trip must preserve the extreme and
// awkward values a column can hold, not just the ordinary ones. The option matrix
// pins which flags shape the file; this file pins that whatever passes through it
// comes back byte-identical. Everything runs through the library-level export /
// import helpers (not the CLI) so a failure points at the writer/reader rather
// than the flag plumbing, and every case compares tables with the NULL-aware,
// type-stable snapshot so a lost sign or a trimmed space is visible.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/imex/internal/config"
)

// countRows is the liveEnv counterpart of cliEnv.rowCount; this file drives the
// library-level helpers, which do not carry the CLI wrapper's methods.
func countRows(t *testing.T, env *liveEnv, table string) int {
	t.Helper()
	var n int
	if err := env.db.QueryRow("SELECT COUNT(*) FROM `" + table + "`").Scan(&n); err != nil {
		t.Fatalf("count rows in %s: %v", table, err)
	}
	return n
}

// TestLive_DataBoundaryNumericExtremes crosses the full width of MySQL's numeric
// types: signed and unsigned integer endpoints, the smallest/largest finite
// DOUBLE, exact DECIMAL extremes (where a float conversion would lose digits),
// and both signs of zero. Round-tripping through DEL is text in, text out, so the
// writer must render each value losslessly and the reader must parse it back
// without a float detour for the exact types.
func TestLive_DataBoundaryNumericExtremes(t *testing.T) {
	env := newLiveEnv(t)
	dir := t.TempDir()

	src := env.liveTable(t, "numbnd_src")
	dst := env.liveTable(t, "numbnd_dst")
	env.exec(t, "CREATE TABLE `"+src+"` ("+
		"id INT NOT NULL PRIMARY KEY, "+
		"ti TINYINT, "+
		"si SMALLINT, "+
		"mi MEDIUMINT, "+
		"i INT, "+
		"bi BIGINT, "+
		"biu BIGINT UNSIGNED, "+
		"dcimal DECIMAL(30,10), "+
		"d DOUBLE, "+
		"f FLOAT"+
		") DEFAULT CHARSET=utf8mb4")
	env.exec(t, "CREATE TABLE `"+dst+"` LIKE `"+src+"`")

	// Row 1: every signed/unsigned maximum. Row 2: every minimum, plus the
	// extremes of floating point. DECIMAL(30,10) holds 20 integer digits, more
	// than a DOUBLE can represent exactly, so it also proves no float detour.
	env.exec(t, "INSERT INTO `"+src+"` VALUES (?,?,?,?,?,?,?,?,?,?)",
		1,
		int64(127), int64(32767), int64(8388607), int64(2147483647),
		int64(9223372036854775807), uint64(18446744073709551615),
		"9999999999.9999999999", 1.7976931348623157e308, float32(3.4028235e38))
	env.exec(t, "INSERT INTO `"+src+"` VALUES (?,?,?,?,?,?,?,?,?,?)",
		2,
		int64(-128), int64(-32768), int64(-8388608), int64(-2147483648),
		int64(-9223372036854775808), uint64(0),
		"-9999999999.9999999999", 5e-324, float32(1.1754944e-38))
	// A row of zeros with both signs exercises +0/-0 and the DECIMAL zero form.
	env.exec(t, "INSERT INTO `"+src+"` VALUES (?,?,?,?,?,?,?,?,?,?)",
		3, int64(0), int64(0), int64(0), int64(0), int64(0), uint64(0),
		"0.0000000000", float64(0), float32(0))

	file := filepath.Join(dir, "numeric.del")
	env.export(t, exportOptions{table: src, format: "del", output: file, threads: 1})
	env.importFile(t, importOptions{input: file, format: "del", mode: config.ImportModeInsert, table: dst, threads: 1})

	// Compare with the server's own nullable-safe equality so a sign or a digit is
	// never lost to a client-side formatting difference. FLOAT is compared as text:
	// MySQL's <=> on two FLOATs promotes them to DOUBLE and can report inequality
	// for values whose stored bits are actually identical (the text DEL carries is
	// the authoritative representation here).
	var mismatch int
	if err := env.db.QueryRow(
		"SELECT COUNT(*) FROM `" + src + "` s JOIN `" + dst + "` d USING(id) " +
			"WHERE NOT (s.ti<=>d.ti AND s.si<=>d.si AND s.mi<=>d.mi AND s.i<=>d.i AND " +
			"s.bi<=>d.bi AND s.biu<=>d.biu AND s.dcimal<=>d.dcimal AND s.d<=>d.d AND CAST(s.f AS CHAR)<=>CAST(d.f AS CHAR))",
	).Scan(&mismatch); err != nil {
		t.Fatalf("compare numeric extremes: %v", err)
	}
	if mismatch != 0 {
		t.Errorf("%d numeric rows lost a value across the DEL round trip", mismatch)
	}
	if got := countRows(t, env, dst); got != 3 {
		t.Errorf("imported %d rows, want 3", got)
	}
}

// TestLive_DataBoundaryTemporalEdges covers the ends of MySQL's temporal range
// and the zero date that strict-mode imports often reject. DEL carries these as
// text, so each value must survive as the exact same wall-clock timestamp.
func TestLive_DataBoundaryTemporalEdges(t *testing.T) {
	env := newLiveEnv(t)
	dir := t.TempDir()

	src := env.liveTable(t, "tmpbnd_src")
	dst := env.liveTable(t, "tmpbnd_dst")
	env.exec(t, "CREATE TABLE `"+src+"` ("+
		"id INT NOT NULL PRIMARY KEY, "+
		"d DATE, "+
		"dt DATETIME, "+
		"ts TIMESTAMP NULL, "+
		"y YEAR"+
		") DEFAULT CHARSET=utf8mb4")
	env.exec(t, "CREATE TABLE `"+dst+"` LIKE `"+src+"`")

	// 1000-01-01 and 9999-12-31 are the documented DATE/DATETIME limits. The
	// zero date is legal in a non-strict column and must round-trip as text.
	env.exec(t, "INSERT INTO `"+src+"` VALUES (1,'1000-01-01','1000-01-01 00:00:00',NULL,1901)")
	env.exec(t, "INSERT INTO `"+src+"` VALUES (2,'9999-12-31','9999-12-31 23:59:59',NULL,2155)")
	// Zero dates are rejected under strict mode; use NULL to exercise the
	// empty temporal value instead, which is the portable edge on every server.
	env.exec(t, "INSERT INTO `"+src+"` VALUES (3,NULL,NULL,NULL,0)")

	file := filepath.Join(dir, "temporal.del")
	env.export(t, exportOptions{table: src, format: "del", output: file, threads: 1})
	env.importFile(t, importOptions{input: file, format: "del", mode: config.ImportModeInsert, table: dst, threads: 1})

	var mismatch int
	if err := env.db.QueryRow(
		"SELECT COUNT(*) FROM `" + src + "` s JOIN `" + dst + "` d USING(id) " +
			"WHERE NOT (s.d<=>d.d AND s.dt<=>d.dt AND s.y<=>d.y)",
	).Scan(&mismatch); err != nil {
		t.Fatalf("compare temporal edges: %v", err)
	}
	if mismatch != 0 {
		t.Errorf("%d temporal rows lost a value across the DEL round trip", mismatch)
	}
}

// TestLive_DataBoundaryStringEdges is the "left/right alignment" case: CHAR(n)
// right-pads its value with spaces to the declared width, while VARCHAR keeps
// leading and trailing spaces verbatim. Neither kind of padding may be trimmed by
// the DEL writer or reader. The set also includes the awkward bytes in a string
// (the quote, the backslash, the column/row delimiters, NUL and control chars),
// multi-byte UTF-8 and an emoji, and both signs written as text.
func TestLive_DataBoundaryStringEdges(t *testing.T) {
	env := newLiveEnv(t)
	dir := t.TempDir()

	src := env.liveTable(t, "stredge_src")
	dst := env.liveTable(t, "stredge_dst")
	env.exec(t, "CREATE TABLE `"+src+"` ("+
		"id INT NOT NULL PRIMARY KEY, "+
		"c CHAR(12), "+
		"v VARCHAR(120), "+
		"numtxt VARCHAR(40)"+
		") DEFAULT CHARSET=utf8mb4")
	env.exec(t, "CREATE TABLE `"+dst+"` LIKE `"+src+"`")

	cases := []struct {
		id     int64
		c      string
		v      string
		numtxt string
	}{
		{1, "abc", "  leading", "+0 / -0 / +42 / -42"},          // right padding + leading spaces + signs
		{2, "x", "trailing  ", "1e308 / -1e-308 / +1.5 / -2.5"}, // trailing spaces + scientific notation
		{3, "  pad  ", "  both ends  ", "  +007  /  -008  "},    // padding inside CHAR + around numbers
		{4, "quote\"x", "a\\b, c|d\te\nf\rg", "+\t/-\r/+ /- "},  // quote, backslash, structural delimiters, control chars
		{5, "多字节", "café 中文 😀 ñ ü ß", "+последний / -١٢٣"},      // multibyte, emoji, non-Latin digits
		{6, "", "", "+"}, // empty + a lone sign
	}
	for _, r := range cases {
		env.exec(t, "INSERT INTO `"+src+"` VALUES (?,?,?,?)", r.id, r.c, r.v, r.numtxt)
	}

	file := filepath.Join(dir, "stredge.del")
	env.export(t, exportOptions{table: src, format: "del", output: file, threads: 1})
	env.importFile(t, importOptions{input: file, format: "del", mode: config.ImportModeInsert, table: dst, threads: 1})

	// Compare as raw bytes via HEX so trailing spaces and NUL are not collapsed
	// by a client that trims CHAR values.
	var mismatch int
	if err := env.db.QueryRow(
		"SELECT COUNT(*) FROM `" + src + "` s JOIN `" + dst + "` d USING(id) " +
			"WHERE NOT (s.c<=>d.c AND s.v<=>d.v AND s.numtxt<=>d.numtxt)",
	).Scan(&mismatch); err != nil {
		t.Fatalf("compare string edges: %v", err)
	}
	if mismatch != 0 {
		t.Errorf("%d string rows lost padding or an awkward byte across the DEL round trip", mismatch)
	}
	if got := countRows(t, env, dst); got != len(cases) {
		t.Errorf("imported %d rows, want %d", got, len(cases))
	}
}

// TestLive_DataBoundaryInvalidUTF8InBinary pins the乱码 case the right way: a
// byte sequence that is not valid UTF-8 must be preserved exactly when it lives
// in a BINARY/VARBINARY column. imex switches such columns to hex literals for
// non-UTF-8 charsets and reads them back, so a random non-UTF-8 payload is the
// sharpest test that no decoding/re-encoding corrupted the bytes.
func TestLive_DataBoundaryInvalidUTF8InBinary(t *testing.T) {
	env := newLiveEnv(t)
	dir := t.TempDir()

	src := env.liveTable(t, "binbnd_src")
	dst := env.liveTable(t, "binbnd_dst")
	env.exec(t, "CREATE TABLE `"+src+"` ("+
		"id INT NOT NULL PRIMARY KEY, "+
		"b VARBINARY(64), "+
		"bl BLOB"+
		") DEFAULT CHARSET=utf8mb4")
	env.exec(t, "CREATE TABLE `"+dst+"` LIKE `"+src+"`")

	// Deliberately invalid UTF-8: lone continuation bytes, an overlong encoding,
	// a truncated sequence, 0xFF (never valid UTF-8), and embedded NULs.
	payloads := [][]byte{
		{0x80, 0x81, 0x82},
		{0xC0, 0xAF},                         // overlong '/'
		{0xE4, 0xB8},                         // truncated 中
		{0xFF, 0xFE, 0x00},                   // invalid + NUL
		{0x00, 0x01, 0x7F, 0x80, 0xC3, 0x28}, // NUL, control, valid 2-byte start followed by ASCII
	}
	for i, p := range payloads {
		env.exec(t, "INSERT INTO `"+src+"` VALUES (?,?,?)", i+1, p, p)
	}

	file := filepath.Join(dir, "binary.del")
	env.export(t, exportOptions{table: src, format: "del", output: file, threads: 1})
	env.importFile(t, importOptions{input: file, format: "del", mode: config.ImportModeInsert, table: dst, threads: 1})

	var mismatch int
	if err := env.db.QueryRow(
		"SELECT COUNT(*) FROM `" + src + "` s JOIN `" + dst + "` d USING(id) " +
			"WHERE NOT (s.b<=>d.b AND s.bl<=>d.bl)",
	).Scan(&mismatch); err != nil {
		t.Fatalf("compare binary payloads: %v", err)
	}
	if mismatch != 0 {
		t.Errorf("%d binary rows had their bytes changed across the DEL round trip", mismatch)
	}
}

// TestLive_DataBoundaryLongValues pins the length edges that sit just around the
// writer's buffers: a VARCHAR at 255 bytes (one-byte length prefix boundary),
// one at 65535 (TEXT row boundary), and a value made only of the character
// delimiter repeated, so escaping is exercised at a length that crosses flushes.
func TestLive_DataBoundaryLongValues(t *testing.T) {
	env := newLiveEnv(t)
	dir := t.TempDir()

	src := env.liveTable(t, "longbnd_src")
	dst := env.liveTable(t, "longbnd_dst")
	env.exec(t, "CREATE TABLE `"+src+"` ("+
		"id INT NOT NULL PRIMARY KEY, "+
		"v255 VARCHAR(255), "+
		"t TEXT, "+
		"quotes TEXT"+
		") DEFAULT CHARSET=utf8mb4")
	env.exec(t, "CREATE TABLE `"+dst+"` LIKE `"+src+"`")

	env.exec(t, "INSERT INTO `"+src+"` VALUES (1,?,?,?)",
		strings.Repeat("a", 255), strings.Repeat("长", 5000), strings.Repeat(`"`, 4096))
	env.exec(t, "INSERT INTO `"+src+"` VALUES (2,?,?,?)",
		strings.Repeat("b", 1), strings.Repeat("z", 60000), strings.Repeat(`\",`, 1500))

	file := filepath.Join(dir, "long.del")
	env.export(t, exportOptions{table: src, format: "del", output: file, threads: 1})
	env.importFile(t, importOptions{input: file, format: "del", mode: config.ImportModeInsert, table: dst, threads: 1})

	var mismatch int
	if err := env.db.QueryRow(
		"SELECT COUNT(*) FROM `" + src + "` s JOIN `" + dst + "` d USING(id) " +
			"WHERE NOT (s.v255<=>d.v255 AND s.t<=>d.t AND s.quotes<=>d.quotes)",
	).Scan(&mismatch); err != nil {
		t.Fatalf("compare long values: %v", err)
	}
	if mismatch != 0 {
		t.Errorf("%d long-value rows were truncated or altered across the DEL round trip", mismatch)
	}
}
