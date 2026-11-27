package export

import (
	"bufio"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

// DelWriter writes data in DEL (delimited ASCII) format
type DelWriter struct {
	bw         *bufio.Writer
	mod        *config.DelModifier
	writeCols  bool
	kinds      []common.ColumnKind // nil = unknown schema (legacy passthrough)
	timeFormat string
	dateFormat string
	// 缓存常用字段，减少间接寻址
	colDel  string
	charDel string
	escape  string
	// binaryFlags[i] marks column i as binary; hexBlob controls 0x encoding.
	binaryFlags []bool
	hexBlob     bool
	// numRisk says that at least one configured structural separator can appear
	// inside bare numeric text ([-+.0-9eE]), so the fields the writer sends bare
	// have to be checked before the row goes out. With the default ',' and '\n'
	// it is false and the check costs nothing.
	numRisk bool
	// hexBuf is the scratch the 0x encoder writes into. hex.EncodeToString would
	// allocate a new string per binary value; the buffer is reused across rows
	// because one writer is only ever used by one goroutine.
	hexBuf []byte
}

// NewDelWriter creates a new DEL format writer. kinds may be nil, in which
// case every value is treated as an opaque string (legacy behavior).
func NewDelWriter(w io.Writer, mod *config.DelModifier, writeColumns bool, kinds []common.ColumnKind) *DelWriter {
	return &DelWriter{
		bw:         bufio.NewWriterSize(w, 256*1024),
		mod:        mod,
		writeCols:  writeColumns,
		kinds:      kinds,
		timeFormat: convertTimeFormat(mod.TimestampFmt),
		dateFormat: convertTimeFormat(mod.DateFmt),
		// 缓存字段
		colDel:  mod.ColDel,
		charDel: mod.CharDel,
		escape:  mod.Escape,
		numRisk: separatorInNumericText(mod.ColDel) || separatorInNumericText(mod.RowDel),
	}
}

// separatorInNumericText reports whether a configured delimiter could occur
// inside the bare numeric text the writer emits for a numeric column. That text
// is digits, a sign, a decimal point and an exponent marker (a PostgreSQL or
// MySQL wire rendering such as 1e+308 carries all four), plus the two spellings
// a float8 column can actually hold — NaN and +Inf/-Inf, which pgx hands over as
// a native float and formatFloat writes verbatim. A delimiter drawn from that
// alphabet makes the record layout ambiguous, while the usual ',' or '\n' never
// does, so the default run keeps the check free.
func separatorInNumericText(delim string) bool {
	for i := 0; i < len(delim); i++ {
		switch delim[i] {
		case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9',
			'+', '-', '.', 'e', 'E', 'N', 'a', 'I', 'f':
			return true
		}
	}
	return false
}

// SetBinaryFormat configures binary column encoding. binaryFlags[i] marks
// column i as binary; when hexBlob is true those columns are written as 0x...
// hex literals (same representation as SQL format). Empty binary values are
// written as 0x (zero-length hex literal).
func (d *DelWriter) SetBinaryFormat(binaryFlags []bool, hexBlob bool) {
	d.binaryFlags = binaryFlags
	d.hexBlob = hexBlob
}

// WriteHeader writes column names as the first row
func (d *DelWriter) WriteHeader(columns []string) error {
	if !d.writeCols {
		return nil
	}
	// Column names are identifiers, never row data: the per-index binary/hex
	// treatment that writeRow applies must not touch them, otherwise a
	// --blob-encoding hex (or non-utf8mb4 charset) export writes header fields such as
	// 0x7662 instead of "vb" and the file can no longer be imported. Likewise the
	// per-column kinds must not apply: with the "number bare" default a header
	// name sitting over a numeric column would otherwise be written bare, so a
	// column literally named 123 would become indistinguishable from a value.
	// Identifiers always go through the quoted string path.
	hexBlob := d.hexBlob
	d.hexBlob = false
	kinds := d.kinds
	d.kinds = nil
	defer func() { d.hexBlob = hexBlob; d.kinds = kinds }()

	values := make([]interface{}, len(columns))
	for i, col := range columns {
		values[i] = col
	}
	if err := d.validateLiteralRow(values); err != nil {
		return err
	}
	return d.writeRow(values)
}

// WriteRow writes a single data row
func (d *DelWriter) WriteRow(values []interface{}) error {
	if err := d.validateLiteralRow(values); err != nil {
		return err
	}
	return d.writeRow(values)
}

// WriteRawRow writes a row directly from sql.RawBytes to avoid allocation
func (d *DelWriter) WriteRawRow(values []sql.RawBytes) error {
	if err := d.validateLiteralRaw(values); err != nil {
		return err
	}
	for i, val := range values {
		if i > 0 {
			d.bw.WriteString(d.colDel)
		}
		// Binary column with hex encoding: write as 0x... literal (unquoted).
		// Empty binary values write as 0x (zero-length hex literal).
		if d.hexBlob && i < len(d.binaryFlags) && d.binaryFlags[i] {
			if val != nil {
				d.bw.WriteString("0x")
				d.writeHex(val)
			}
			// nil (NULL) or empty: write nothing (NULL) or 0x (empty)
			continue
		}
		d.formatRawValue(val, i)
	}
	d.bw.WriteString(d.mod.RowDel)
	return nil
}

// WriteFooter flushes the buffered writer
func (d *DelWriter) WriteFooter() error {
	return d.bw.Flush()
}

// writeHex appends the hex encoding of val to the output, reusing the scratch
// buffer instead of allocating an encoded string per value.
func (d *DelWriter) writeHex(val []byte) {
	d.hexBuf = hex.AppendEncode(d.hexBuf[:0], val)
	d.bw.Write(d.hexBuf)
}

// Flush flushes the buffered writer (for testing)
func (d *DelWriter) Flush() error {
	return d.bw.Flush()
}

func (d *DelWriter) writeRow(values []interface{}) error {
	for i, val := range values {
		if i > 0 {
			d.bw.WriteString(d.colDel)
		}
		// Binary column with hex encoding: write as 0x... literal (unquoted).
		if d.hexBlob && i < len(d.binaryFlags) && d.binaryFlags[i] {
			if val != nil {
				d.bw.WriteString("0x")
				switch v := val.(type) {
				case []byte:
					d.writeHex(v)
				case string:
					d.writeHex([]byte(v))
				default:
					d.writeHex([]byte(fmt.Sprintf("%v", v)))
				}
			}
			continue
		}
		d.formatValue(val, i)
	}
	d.bw.WriteString(d.mod.RowDel)
	return nil
}

// kind returns the ColumnKind for a field index, or KindUnknown when the
// schema is not known or the index is out of range.
func (d *DelWriter) kind(i int) common.ColumnKind {
	if d.kinds == nil || i >= len(d.kinds) {
		return common.KindUnknown
	}
	return d.kinds[i]
}

// needsLiteralCheck reports whether the field at idx has to be looked at before
// it is written. A field that goes out quoted, with a working escape character,
// is safe by construction; what is left is anything the writer sends bare — every
// field in bare mode, and the numeric columns in any mode.
func (d *DelWriter) needsLiteralCheck(idx int) bool {
	return d.literalFields() || d.kind(idx) == common.KindNumber
}

// validateLiteralRow rejects values the writer cannot represent
// unambiguously. Validation runs before anything is written so a rejected row
// never leaves a half-written, corrupt record in the output.
func (d *DelWriter) validateLiteralRow(values []interface{}) error {
	if !d.literalFields() && !d.numRisk {
		return nil
	}
	for i, val := range values {
		if d.hexBlob && i < len(d.binaryFlags) && d.binaryFlags[i] {
			// Hex-encoded binary fields cannot contain a raw delimiter.
			continue
		}
		if val == nil {
			continue
		}
		var text string
		switch v := val.(type) {
		case string:
			text = v
		case []byte:
			text = string(v)
		case time.Time:
			text = v.Format(d.timeFormat)
		default:
			// Native numbers and booleans leave the writer bare whatever
			// --char-del says, so their text has to clear the separators too.
			if s, ok := numericFieldText(v); ok {
				if !d.numRisk {
					continue
				}
				if err := d.checkSeparators(s, i); err != nil {
					return err
				}
				continue
			}
			text = fmt.Sprintf("%v", v)
		}
		if !d.needsLiteralCheck(i) {
			continue
		}
		if err := d.checkLiteralText(text, i); err != nil {
			return err
		}
	}
	return nil
}

// validateLiteralRaw is validateLiteralRow for the RawBytes fast path.
func (d *DelWriter) validateLiteralRaw(values []sql.RawBytes) error {
	if !d.literalFields() && !d.numRisk {
		return nil
	}
	for i, val := range values {
		if d.hexBlob && i < len(d.binaryFlags) && d.binaryFlags[i] {
			continue
		}
		if !d.needsLiteralCheck(i) {
			continue
		}
		if err := d.checkLiteralText(bytesToString(val), i); err != nil {
			return err
		}
	}
	return nil
}

// bareFields reports whether fields go out without character delimiters, which
// is what an empty --char-del asks for. The DEL reader only honours escapes
// *inside* quoted fields, so a bare field is written verbatim whatever --escape
// says: it must not contain a structural delimiter either.
func (d *DelWriter) bareFields() bool {
	return d.charDel == ""
}

// literalFields reports whether values go into the file exactly as they arrived,
// either because there is no character delimiter to wrap them (empty
// --char-del) or because there is no escape character to protect a quote (empty
// --escape, the removed --no-escape flag). Only then can a value break the
// record layout, so only then is the row validated before it is written.
func (d *DelWriter) literalFields() bool {
	return d.bareFields() || d.escape == ""
}

// fieldBare reports whether the field at idx goes out without --char-del. Two
// things make a field bare: an empty --char-del, and the numeric column kind,
// which the writer renders bare whatever --char-del says.
func (d *DelWriter) fieldBare(idx int) bool {
	return d.bareFields() || d.kind(idx) == common.KindNumber
}

// checkLiteralText reports whether s can be written verbatim. A quoted field
// (character delimiter in use, no escape) can contain the column and row
// delimiters, but not the quote itself. A bare field — which includes every
// numeric column — has to avoid both structural separators.
func (d *DelWriter) checkLiteralText(s string, idx int) error {
	if s == "" {
		return nil
	}
	if !d.fieldBare(idx) {
		if strings.Contains(s, d.charDel) {
			return d.unrepresentableError(idx, "character delimiter", d.charDel)
		}
		return nil
	}
	return d.checkSeparators(s, idx)
}

// checkSeparators rejects a bare field whose text carries a structural
// separator: the reader would split the record where the value did not.
func (d *DelWriter) checkSeparators(s string, idx int) error {
	if s == "" {
		return nil
	}
	if d.colDel != "" && strings.Contains(s, d.colDel) {
		return d.unrepresentableError(idx, "column delimiter", d.colDel)
	}
	if d.mod.RowDel != "" && strings.Contains(s, d.mod.RowDel) {
		return d.unrepresentableError(idx, "row delimiter", d.mod.RowDel)
	}
	return nil
}

// numericFieldText renders a native numeric or boolean value exactly the way
// formatValue writes it, so validation judges the bytes that actually go out.
// The second result says whether the value is one of those types at all.
func numericFieldText(val interface{}) (string, bool) {
	switch v := val.(type) {
	case int:
		return strconv.FormatInt(int64(v), 10), true
	case int8:
		return strconv.FormatInt(int64(v), 10), true
	case int16:
		return strconv.FormatInt(int64(v), 10), true
	case int32:
		return strconv.FormatInt(int64(v), 10), true
	case int64:
		return strconv.FormatInt(v, 10), true
	case uint:
		return strconv.FormatUint(uint64(v), 10), true
	case uint8:
		return strconv.FormatUint(uint64(v), 10), true
	case uint16:
		return strconv.FormatUint(uint64(v), 10), true
	case uint32:
		return strconv.FormatUint(uint64(v), 10), true
	case uint64:
		return strconv.FormatUint(v, 10), true
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 64), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case bool:
		if v {
			return "1", true
		}
		return "0", true
	}
	return "", false
}

// unrepresentableError names the flag that made the field literal, so the user
// sees an actionable remedy in both empty --escape and empty --char-del mode.
func (d *DelWriter) unrepresentableError(idx int, kind, delim string) error {
	// Bare fields win the attribution: with no character delimiter the field is
	// written verbatim whatever --escape says, so naming --escape would send the
	// user to the wrong flag.
	flag := "--escape"
	remedy := "set --escape to a character so quotes are escaped, or choose delimiters absent from the data"
	if d.bareFields() {
		flag = "--char-del"
		remedy = "set --char-del to a character so fields are quoted, or choose delimiters absent from the data"
	}
	// A numeric column is bare even with a character delimiter in place, so the
	// separators themselves are the only knobs that can help. Reaching this in
	// quoted mode with a *separator* complaint can only mean such a field.
	if !d.bareFields() && kind != "character delimiter" {
		flag = "--col-del"
		if kind == "row delimiter" {
			flag = "--row-del"
		}
		remedy = fmt.Sprintf("numeric columns are written without a character delimiter, so choose a %s that cannot appear in numeric text (no digit, sign, point, exponent marker, and no letter of NaN/Inf; the default is a comma), or cast this column to a character type in the query", flag)
	}
	return fmt.Errorf("%s: column %d value contains the %s %q, which would corrupt the DEL record; %s", flag, idx+1, kind, delim, remedy)
}

func (d *DelWriter) formatValue(val interface{}, idx int) {
	if val == nil {
		return
	}

	switch v := val.(type) {
	case []byte:
		d.formatTypedString(string(v), idx)
	case string:
		d.formatTypedString(v, idx)
	case time.Time:
		d.formatTime(v)
	case int:
		d.bw.WriteString(strconv.FormatInt(int64(v), 10))
	case int8:
		d.bw.WriteString(strconv.FormatInt(int64(v), 10))
	case int16:
		d.bw.WriteString(strconv.FormatInt(int64(v), 10))
	case int32:
		d.bw.WriteString(strconv.FormatInt(int64(v), 10))
	case int64:
		d.bw.WriteString(strconv.FormatInt(v, 10))
	case uint:
		d.bw.WriteString(strconv.FormatUint(uint64(v), 10))
	case uint8:
		d.bw.WriteString(strconv.FormatUint(uint64(v), 10))
	case uint16:
		d.bw.WriteString(strconv.FormatUint(uint64(v), 10))
	case uint32:
		d.bw.WriteString(strconv.FormatUint(uint64(v), 10))
	case uint64:
		d.bw.WriteString(strconv.FormatUint(v, 10))
	case float32:
		d.formatFloat(float64(v))
	case float64:
		d.formatFloat(v)
	case bool:
		if v {
			d.bw.WriteByte('1')
		} else {
			d.bw.WriteByte('0')
		}
	default:
		d.formatTypedString(fmt.Sprintf("%v", v), idx)
	}
}

// formatRawValue formats sql.RawBytes directly without allocation, applying
// the column-kind transformations (--dec-pt, --strip-lzeros, --date-fmt,
// --timestamp-fmt) when the schema is known.
func (d *DelWriter) formatRawValue(val sql.RawBytes, idx int) {
	if val == nil {
		return
	}
	// 零拷贝转换：[]byte -> string，避免内存分配
	s := bytesToString(val)
	d.formatTypedString(s, idx)
}

// formatTypedString writes one wire-text field according to its column kind.
// Numeric columns are written BARE (no --char-del): the value is MySQL wire
// text whose decimal point is '.', with no leading-zero stripping, so it is a
// literal the import reader can bind directly. Every other kind — strings,
// enums, JSON (all KindUnknown) and the date/time families — is wrapped in
// --char-del as before; date/timestamp columns honor --date-fmt/--timestamp-fmt.
// This "number bare, everything else quoted" split is the default DEL layout.
func (d *DelWriter) formatTypedString(s string, idx int) {
	switch d.kind(idx) {
	case common.KindNumber:
		// Numeric columns: bare wire text, no character delimiter. Numeric wire
		// text only ever contains [-+.eE0-9], never a structural delimiter, so
		// writing it verbatim cannot corrupt the record layout.
		d.bw.WriteString(s)
	case common.KindDate:
		if d.dateFormat != "" && d.dateFormat != "2006-01-02" {
			if t, err := time.Parse("2006-01-02", s); err == nil {
				s = t.Format(d.dateFormat)
			}
		}
		d.formatString(s)
	case common.KindTimestamp:
		if d.timeFormat != "" && d.timeFormat != "2006-01-02 15:04:05" {
			if t, err := parseTimestampText(s); err == nil {
				s = t.Format(d.timeFormat)
			}
		}
		d.formatString(s)
	default:
		// KindUnknown / KindTime / KindYear: wire text passes through unchanged.
		d.formatString(s)
	}
}

// parseTimestampText parses canonical MySQL DATETIME/TIMESTAMP wire text,
// tolerating fractional seconds. Anything unparseable (zero dates, odd
// formats) is passed through untouched by the caller.
func parseTimestampText(s string) (time.Time, error) {
	for _, l := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04:05.999999"} {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable timestamp %q", s)
}

// bytesToString 零拷贝将 []byte 转换为 string
// 注意：调用者必须保证 byte slice 在使用期间不会被修改
func bytesToString(b []byte) string {
	// unsafe.String panics on an empty slice because &b[0] is out of range.
	// An empty (non-nil) RawBytes represents an empty string value; return ""
	// without touching the (possibly empty) backing array.
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(&b[0], len(b))
}

func (d *DelWriter) formatString(s string) {
	charDel := d.charDel
	if charDel == "" {
		d.bw.WriteString(s)
		return
	}

	d.bw.WriteString(charDel)

	if d.escape == charDel {
		// Double-write escaping
		d.writeEscapedDouble(s, charDel)
	} else {
		// An empty escape writes the value unescaped; writeEscapedBackslash
		// already returns it verbatim in that case.
		d.writeEscapedBackslash(s, charDel, d.escape)
	}

	d.bw.WriteString(charDel)
}

// writeEscapedDouble writes s with double-write escaping directly to buffer
func (d *DelWriter) writeEscapedDouble(s string, charDel string) {
	last := 0
	// 单字节字符分隔符优化：直接比较字节，避免 string(s[i]) 分配
	if len(charDel) == 1 {
		delim := charDel[0]
		for i := 0; i < len(s); i++ {
			if s[i] == delim {
				d.bw.WriteString(s[last:i])
				d.bw.WriteString(charDel)
				d.bw.WriteString(charDel)
				last = i + 1
			}
		}
		d.bw.WriteString(s[last:])
		return
	}
	// 多字节分隔符回退到字符串比较
	for i := 0; i < len(s); i++ {
		if strings.HasPrefix(s[i:], charDel) {
			d.bw.WriteString(s[last:i])
			d.bw.WriteString(charDel)
			d.bw.WriteString(charDel)
			i += len(charDel) - 1
			last = i + 1
		}
	}
	d.bw.WriteString(s[last:])
}

// writeEscapedBackslash writes s with backslash-style escaping directly to buffer.
// It matches whole delimiters (multi-byte and hex forms are supported), not just
// their first byte: escaping only `len(charDel) == 1` would leave a multi-byte
// delimiter inside the value and let the reader split the field apart.
func (d *DelWriter) writeEscapedBackslash(s string, charDel string, escape string) {
	if escape == "" {
		d.bw.WriteString(s)
		return
	}
	last := 0
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], escape):
			d.bw.WriteString(s[last:i])
			d.bw.WriteString(escape)
			d.bw.WriteString(escape)
			i += len(escape)
			last = i
		case strings.HasPrefix(s[i:], charDel):
			d.bw.WriteString(s[last:i])
			d.bw.WriteString(escape)
			d.bw.WriteString(charDel)
			i += len(charDel)
			last = i
		default:
			i++
		}
	}
	d.bw.WriteString(s[last:])
}

func (d *DelWriter) formatTime(t time.Time) {
	d.formatString(t.Format(d.timeFormat))
}

func (d *DelWriter) formatFloat(f float64) {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	d.bw.WriteString(s)
}

// convertTimeFormat converts DB2-style time format to Go time format
func convertTimeFormat(fmt string) string {
	replacer := strings.NewReplacer(
		"yyyy", "2006",
		"yy", "06",
		"MM", "01",
		"dd", "02",
		"HH", "15",
		"mm", "04",
		"ss", "05",
	)
	return replacer.Replace(fmt)
}
