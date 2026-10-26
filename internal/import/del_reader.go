package _import

import (
	"bufio"
	"bytes"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

// Reader interface for different import formats
type Reader interface {
	ReadHeader() ([]string, error)
	ReadRow() ([]interface{}, error)
}

// DelReader reads data in DEL (delimited ASCII) format
type DelReader struct {
	rd       *recordReader
	mod      *config.DelModifier
	readCols bool
	columns  []string
	colCount int
	// Precomputed canonical layouts for reverse modifier application.
	dateLayout string // non-empty when --date-fmt differs from ISO
	tsLayout   string // non-empty when --timestamp-fmt differs from ISO
	// kinds maps file-column position to the target table column kind.
	// Nil until the importer supplies it; without kinds no text modifier is
	// applied (raw file values pass through), which is safe for string data.
	kinds []common.ColumnKind
	// binaryCols marks file-column positions whose target column is a raw
	// binary type. The exporter writes those as unquoted 0x literals when
	// hex encoding is on, so the reader has to decode them back to bytes; for
	// any other column a 0x token is ordinary text.
	binaryCols []bool

	// Delimiter layout, resolved once. scanRows is a bufio.SplitFunc that runs
	// for every record and parseLine tests delimiters at every byte position,
	// so converting the configured strings there would repeat the same work
	// row after row. delimSpec also carries the first byte, which turns the
	// default single-byte delimiters into byte compares instead of prefix
	// calls over a freshly built substring.
	rowD, charD, colD, escD delimSpec
	quote                   bool // string delimiters are in use
	doubleWrite             bool // escape == charDel: quotes escape by doubling
	backslashEsc            bool // escape is set and differs from charDel

	// Reused per-reader scratch for the row path. Field values themselves are
	// always fresh strings, because the pipeline hands rows to another
	// goroutine and recordReader reuses its buffer; only the slices holding
	// them are recycled here.
	fields  []string
	quoted  []bool
	scratch []byte
}

// delimSpec is a configured delimiter pre-sliced for byte-level scanning.
type delimSpec struct {
	b   []byte
	n   int
	one byte // b[0]; meaningful only when n == 1
}

func newDelimSpec(s string) delimSpec {
	d := delimSpec{n: len(s)}
	if len(s) > 0 {
		d.b = []byte(s)
		d.one = s[0]
	}
	return d
}

// at reports whether the delimiter starts at s[i:]. An empty delimiter matches
// everywhere, which is strings.HasPrefix(s[i:], "") behaviour.
func (d delimSpec) at(s []byte, i int) bool {
	if d.n == 1 {
		return i < len(s) && s[i] == d.one
	}
	return i+d.n <= len(s) && bytes.Equal(s[i:i+d.n], d.b)
}

// SetColumnKinds supplies per-file-column target kinds so --dec-pt/--date-fmt/
// --timestamp-fmt reverse transforms only touch numeric/temporal columns.
func (d *DelReader) SetColumnKinds(kinds []common.ColumnKind) {
	d.kinds = kinds
}

// SetBinaryColumns marks which file columns hold raw binary data. Combined with
// an unquoted 0x… token this is what distinguishes a hex-encoded BLOB from a
// text value that merely starts with "0x".
func (d *DelReader) SetBinaryColumns(flags []bool) {
	d.binaryCols = flags
}

// isBinaryColumn reports whether the field at idx targets a binary column.
func (d *DelReader) isBinaryColumn(idx int) bool {
	return idx < len(d.binaryCols) && d.binaryCols[idx]
}

// NewDelReader creates a new DEL format reader
func NewDelReader(r *bufio.Reader, mod *config.DelModifier, readColumns bool) *DelReader {
	d := &DelReader{
		mod:      mod,
		readCols: readColumns,
		rowD:     newDelimSpec(mod.RowDel),
		charD:    newDelimSpec(mod.CharDel),
		colD:     newDelimSpec(mod.ColDel),
		escD:     newDelimSpec(mod.Escape),
	}
	// Quote handling is off for --no-char-del and for an empty char delimiter,
	// exactly as the per-position checks below require.
	d.quote = !mod.NoCharDel && mod.CharDel != ""
	if !mod.NoEscape {
		d.doubleWrite = d.quote && mod.Escape == mod.CharDel
		d.backslashEsc = mod.Escape != "" && mod.Escape != mod.CharDel
	}
	// --date-fmt / --timestamp-fmt: re-parse configured layouts back to the
	// canonical MySQL text the server expects.
	if df := convertImportFormat(mod.DateFmt); df != "" && df != "2006-01-02" && strings.Contains(df, "2006") {
		d.dateLayout = df
	}
	if tf := convertImportFormat(mod.TimestampFmt); tf != "" && tf != "2006-01-02 15:04:05" && strings.Contains(tf, "2006") {
		d.tsLayout = tf
	}

	// Use a quote-aware splitter so a row delimiter (newline) inside a quoted
	// field is not treated as a row boundary.
	d.rd = newRecordReader(r, d.scanRows)
	return d
}

// scanRows is a bufio.SplitFunc that yields complete records, treating the
// row delimiter as a boundary only when it occurs outside a quoted field.
func (d *DelReader) scanRows(data []byte, atEOF bool) (advance int, token []byte, err error) {
	inQuote := false
	pos := 0
	for pos < len(data) {
		if inQuote {
			if d.quote && d.charD.at(data, pos) {
				// Double-write escape: charDel followed by charDel -> escaped,
				// stay in quote.
				if d.doubleWrite && d.charD.at(data, pos+d.charD.n) {
					pos += 2 * d.charD.n
					continue
				}
				// Closing quote
				inQuote = false
				pos += d.charD.n
				continue
			}
			// Backslash-style escape (escape distinct from charDel): skip the
			// whole escape sequence. A multi-byte escape may be followed by a
			// multi-byte delimiter, so consuming a single byte would leave the
			// splitter inside the delimiter and corrupt the quote state.
			if d.backslashEsc && d.escD.at(data, pos) {
				next := pos + d.escD.n
				switch {
				case d.quote && d.charD.at(data, next):
					pos = next + d.charD.n
				case d.escD.at(data, next):
					pos = next + d.escD.n
				case next < len(data):
					pos = next + 1
				case atEOF:
					pos = next
				default:
					// The escaped byte has not arrived yet.
					return 0, nil, nil
				}
				continue
			}
			pos++
			continue
		}

		// Outside quotes: a row delimiter terminates the record
		if d.rowD.n > 0 && d.rowD.at(data, pos) {
			return pos + d.rowD.n, data[:pos], nil
		}
		// Opening quote
		if d.quote && d.charD.at(data, pos) {
			inQuote = true
			pos += d.charD.n
			continue
		}
		pos++
	}

	if atEOF {
		if len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	}
	// Incomplete record; request more data
	return 0, nil, nil
}

// ReadHeader reads the column names from the first row
func (d *DelReader) ReadHeader() ([]string, error) {
	if !d.readCols {
		return nil, nil
	}

	if !d.rd.Scan() {
		if err := d.rd.Err(); err != nil {
			return nil, errors.Wrap(err, "failed to read header")
		}
		return nil, errors.New("EOF")
	}

	line := d.rd.Bytes()
	fields, _ := d.parseLine(line, nil, nil)
	d.columns = fields
	d.colCount = len(fields)
	return fields, nil
}

// ReadRow reads a single data row. Empty *unquoted* fields are NULL; empty
// *quoted* fields ("") are the empty string, so quote-ness must be preserved.
func (d *DelReader) ReadRow() ([]interface{}, error) {
	if !d.rd.Scan() {
		if err := d.rd.Err(); err != nil {
			return nil, errors.Wrap(err, "failed to read row")
		}
		return nil, errors.New("EOF")
	}

	line := d.rd.Bytes()
	fields, quoted := d.parseLine(line, d.fields, d.quoted)
	d.fields, d.quoted = fields, quoted

	values := make([]interface{}, len(fields))
	for i, f := range fields {
		if f == "" && !quoted[i] {
			values[i] = nil
			continue
		}
		// A hex-encoded binary column arrives unquoted as 0x…, so only an
		// unquoted token in a binary column is decoded; quoted values are text,
		// and non-binary columns keep "0x…" verbatim.
		if !quoted[i] && d.isBinaryColumn(i) {
			if b, ok := decodeHexField(f); ok {
				values[i] = b
				continue
			}
		}
		values[i] = d.applyModifiers(f, i)
	}

	return values, nil
}

// applyModifiers reverses the file-side text modifiers (--dec-pt, --date-fmt,
// --timestamp-fmt) for a field at file-column position idx, so values arrive
// at the server in canonical MySQL text. Transforms are applied only to the
// column kinds they belong to; when the target schema/kind is unknown the raw
// value passes through untouched (string data is never rewritten).
func (d *DelReader) applyModifiers(f string, idx int) string {
	if d.kinds == nil || idx >= len(d.kinds) {
		return f
	}
	switch d.kinds[idx] {
	case common.KindDate:
		if d.dateLayout != "" {
			if t, err := time.Parse(d.dateLayout, f); err == nil {
				return t.Format("2006-01-02")
			}
		}
	case common.KindTimestamp:
		if d.tsLayout != "" {
			for _, frac := range []string{"", ".999999"} {
				if t, err := time.Parse(d.tsLayout+frac, f); err == nil {
					return t.Format("2006-01-02 15:04:05")
				}
			}
		}
	}
	return f
}

// convertImportFormat converts a DB2-style format string to a Go layout,
// mirroring the exporter's convertTimeFormat.
func convertImportFormat(fmt string) string {
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

// parseLine splits one record into field values plus a parallel slice marking
// which fields were originally quoted — quote-ness is what distinguishes an
// empty string from NULL.
//
// Callers pass back the slices they want reused (`fields[:0]`/`quoted[:0]`) so
// a long import allocates those headers once instead of per row; the field
// *values* are always fresh strings because the pipeline hands rows to another
// goroutine while recordReader recycles its own buffer. A field that needs no
// un-escaping is therefore a single conversion of its own bytes, and only
// fields that really contain an escape sequence are rebuilt in scratch space.
func (d *DelReader) parseLine(line []byte, fields []string, quoted []bool) ([]string, []bool) {
	fields = fields[:0]
	quoted = quoted[:0]
	i, n := 0, len(line)
	// An empty column delimiter would match at every position and never
	// advance; the CLI rejects empty delimiters, so treat it as "no column
	// separation" and let each record parse as one field.
	hasCol := d.colD.n > 0

	for i < n {
		switch {
		case d.quote && d.charD.at(line, i):
			// Quoted field.
			start := i + d.charD.n
			i = start
			clean := start
			dirty := false
			closed := false
			for i < n {
				if d.charD.at(line, i) {
					// Double-write escape ("" -> ") or end of the field.
					if d.doubleWrite && d.charD.at(line, i+d.charD.n) {
						d.scratch = append(d.scratch, line[clean:i]...)
						dirty = true
						d.scratch = append(d.scratch, d.charD.b...)
						i += 2 * d.charD.n
						clean = i
						continue
					}
					i += d.charD.n
					closed = true
					break
				}
				if d.backslashEsc && d.escD.at(line, i) {
					d.scratch = append(d.scratch, line[clean:i]...)
					dirty = true
					after := i + d.escD.n
					switch {
					case d.charD.at(line, after):
						// Escaped quote: \" -> "
						d.scratch = append(d.scratch, d.charD.b...)
						i = after + d.charD.n
					case d.escD.at(line, after):
						// Double escape: \\ -> \
						d.scratch = append(d.scratch, d.escD.b...)
						i = after + d.escD.n
					case after < n:
						// Escape byte followed by ordinary data.
						d.scratch = append(d.scratch, d.escD.b...)
						d.scratch = append(d.scratch, line[after])
						i = after + 1
					default:
						// Trailing escape byte at end of record.
						d.scratch = append(d.scratch, d.escD.b...)
						i = after
					}
					clean = i
					continue
				}
				i++
			}
			// Content stops before the closing quote; an unterminated field runs
			// to the end of the record, exactly as the byte-by-byte writer did.
			end := i
			if closed {
				end = i - d.charD.n
			}
			if dirty {
				fields = append(fields, string(append(d.scratch, line[clean:end]...)))
				d.scratch = d.scratch[:0]
			} else {
				fields = append(fields, string(line[start:end]))
			}
			quoted = append(quoted, true)

			// Skip the column delimiter that ends the field, if present.
			if hasCol && d.colD.at(line, i) {
				i += d.colD.n
			}
		case hasCol && d.colD.at(line, i):
			// Empty field between delimiters.
			fields = append(fields, "")
			quoted = append(quoted, false)
			i += d.colD.n
		default:
			// Unquoted field: written verbatim, no escape handling applies.
			start := i
			for i < n && !(hasCol && d.colD.at(line, i)) {
				i++
			}
			fields = append(fields, string(line[start:i]))
			quoted = append(quoted, false)
			if hasCol && d.colD.at(line, i) {
				i += d.colD.n
			}
		}
	}

	// Handle trailing delimiter (empty last field)
	if n > 0 && hasCol && d.colD.n <= n && bytes.Equal(line[n-d.colD.n:], d.colD.b) {
		fields = append(fields, "")
		quoted = append(quoted, false)
	}

	return fields, quoted
}
