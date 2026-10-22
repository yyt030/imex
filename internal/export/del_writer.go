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

	"github.com/gomydump/internal/common"
	"github.com/gomydump/internal/config"
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
	colDel    string
	charDel   string
	escape    string
	noEscape  bool
	noCharDel bool
	// binaryFlags[i] marks column i as binary; hexBlob controls 0x encoding.
	binaryFlags []bool
	hexBlob     bool
}

// NewDelWriter creates a new DEL format writer. kinds may be nil, in which
// case every value is treated as an opaque string (legacy behavior).
func NewDelWriter(w io.Writer, mod *config.DelModifier, writeColumns bool, kinds []common.ColumnKind) *DelWriter {
	// 预计算 escape 值
	escape := mod.Escape
	if escape == "" {
		escape = mod.CharDel
	}

	return &DelWriter{
		bw:         bufio.NewWriterSize(w, 256*1024),
		mod:        mod,
		writeCols:  writeColumns,
		kinds:      kinds,
		timeFormat: convertTimeFormat(mod.TimestampFmt),
		dateFormat: convertTimeFormat(mod.DateFmt),
		// 缓存字段
		colDel:    mod.ColDel,
		charDel:   mod.CharDel,
		escape:    escape,
		noEscape:  mod.NoEscape,
		noCharDel: mod.NoCharDel,
	}
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
	values := make([]interface{}, len(columns))
	for i, col := range columns {
		values[i] = col
	}
	return d.writeRow(values)
}

// WriteRow writes a single data row
func (d *DelWriter) WriteRow(values []interface{}) error {
	return d.writeRow(values)
}

// WriteRawRow writes a row directly from sql.RawBytes to avoid allocation
func (d *DelWriter) WriteRawRow(values []sql.RawBytes) error {
	for i, val := range values {
		if i > 0 {
			d.bw.WriteString(d.colDel)
		}
		// Binary column with hex encoding: write as 0x... literal (unquoted).
		// Empty binary values write as 0x (zero-length hex literal).
		if d.hexBlob && i < len(d.binaryFlags) && d.binaryFlags[i] {
			if val != nil {
				d.bw.WriteString("0x")
				d.bw.WriteString(hex.EncodeToString(val))
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
					d.bw.WriteString(hex.EncodeToString(v))
				case string:
					d.bw.WriteString(hex.EncodeToString([]byte(v)))
				default:
					d.bw.WriteString(hex.EncodeToString([]byte(fmt.Sprintf("%v", v))))
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
// Numeric columns always output MySQL wire text: decimal point is '.', no
// leading-zero stripping. Date/timestamp columns honor --date-fmt/--timestamp-fmt.
func (d *DelWriter) formatTypedString(s string, idx int) {
	switch d.kind(idx) {
	case common.KindNumber:
		// Numeric columns: wire text passthrough, decimal point always '.'
		d.formatString(s)
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
	if d.noCharDel {
		d.bw.WriteString(s)
		return
	}

	charDel := d.charDel
	if charDel == "" {
		d.bw.WriteString(s)
		return
	}

	d.bw.WriteString(charDel)

	if !d.noEscape {
		if d.escape == charDel {
			// Double-write escaping
			d.writeEscapedDouble(s, charDel)
		} else {
			d.writeEscapedBackslash(s, charDel, d.escape)
		}
	} else {
		d.bw.WriteString(s)
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

// writeEscapedBackslash writes s with backslash-style escaping directly to buffer
func (d *DelWriter) writeEscapedBackslash(s string, charDel string, escape string) {
	last := 0
	for i := 0; i < len(s); i++ {
		if s[i] == escape[0] {
			d.bw.WriteString(s[last:i])
			d.bw.WriteString(escape)
			d.bw.WriteString(escape)
			last = i + 1
		} else if len(charDel) == 1 && s[i] == charDel[0] {
			d.bw.WriteString(s[last:i])
			d.bw.WriteString(escape)
			d.bw.WriteString(charDel)
			last = i + 1
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
