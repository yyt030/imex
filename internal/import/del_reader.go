package _import

import (
	"bufio"
	"bytes"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/gomydump/internal/common"
	"github.com/gomydump/internal/config"
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
}

// SetColumnKinds supplies per-file-column target kinds so --dec-pt/--date-fmt/
// --timestamp-fmt reverse transforms only touch numeric/temporal columns.
func (d *DelReader) SetColumnKinds(kinds []common.ColumnKind) {
	d.kinds = kinds
}

// NewDelReader creates a new DEL format reader
func NewDelReader(r *bufio.Reader, mod *config.DelModifier, readColumns bool) *DelReader {
	d := &DelReader{
		mod:      mod,
		readCols: readColumns,
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
	rowDel := []byte(d.mod.RowDel)
	charDel := []byte(d.mod.CharDel)
	escape := []byte(d.mod.Escape)
	if d.mod.NoCharDel {
		charDel = nil
	}
	noEscape := d.mod.NoEscape

	inQuote := false
	pos := 0
	for pos < len(data) {
		if inQuote {
			if len(charDel) > 0 && bytes.HasPrefix(data[pos:], charDel) {
				// Double-write escape: charDel followed by charDel -> escaped, stay in quote
				if !noEscape && len(escape) > 0 && bytes.Equal(escape, charDel) &&
					bytes.HasPrefix(data[pos+len(charDel):], charDel) {
					pos += 2 * len(charDel)
					continue
				}
				// Closing quote
				inQuote = false
				pos += len(charDel)
				continue
			}
			// Backslash-style escape (escape distinct from charDel): skip escape + next char
			if !noEscape && len(escape) > 0 && !bytes.Equal(escape, charDel) &&
				bytes.HasPrefix(data[pos:], escape) {
				next := pos + len(escape)
				if next < len(data) {
					// Need one more byte to consume the escaped character
					pos = next + 1
				} else if atEOF {
					pos = next
				} else {
					return 0, nil, nil // wait for the escaped byte
				}
				continue
			}
			pos++
			continue
		}

		// Outside quotes: a row delimiter terminates the record
		if len(rowDel) > 0 && bytes.HasPrefix(data[pos:], rowDel) {
			return pos + len(rowDel), data[:pos], nil
		}
		// Opening quote
		if len(charDel) > 0 && bytes.HasPrefix(data[pos:], charDel) {
			inQuote = true
			pos += len(charDel)
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

	line := d.rd.Text()
	fields, _ := d.parseLine(line)
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

	line := d.rd.Text()
	fields, quoted := d.parseLine(line)

	values := make([]interface{}, len(fields))
	for i, f := range fields {
		if f == "" && !quoted[i] {
			values[i] = nil
		} else {
			values[i] = d.applyModifiers(f, i)
		}
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

// parseLine parses a delimited line into fields. It returns the field values
// and a parallel slice indicating whether each field was originally quoted.
// Quote-ness is what distinguishes an empty string from NULL.
func (d *DelReader) parseLine(line string) ([]string, []bool) {
	colDel := d.mod.ColDel
	charDel := d.mod.CharDel
	escape := d.mod.Escape
	noEscape := d.mod.NoEscape

	var fields []string
	var quoted []bool
	var current strings.Builder
	i := 0

	for i < len(line) {
		// Check if we're at a quoted field
		if !d.mod.NoCharDel && charDel != "" && strings.HasPrefix(line[i:], charDel) {
			// Start of a quoted field
			i += len(charDel)
			current.Reset()

			for i < len(line) {
				// Check for closing quote first
				if strings.HasPrefix(line[i:], charDel) {
					// Check if it's a double-write escape (two quotes in a row)
					if !noEscape && escape == charDel && i+len(charDel) < len(line) && strings.HasPrefix(line[i+len(charDel):], charDel) {
						// Double-write escape: "" -> "
						current.WriteString(charDel)
						i += 2 * len(charDel)
						continue
					}
					// End of quoted field
					i += len(charDel)
					break
				}

				// Check for backslash-style escape (only if escape != charDel)
				if !noEscape && escape != "" && escape != charDel && strings.HasPrefix(line[i:], escape) {
					afterEscape := i + len(escape)
					if afterEscape < len(line) {
						if strings.HasPrefix(line[afterEscape:], charDel) {
							// Escaped quote: \" -> "
							current.WriteString(charDel)
							i = afterEscape + len(charDel)
							continue
						} else if strings.HasPrefix(line[afterEscape:], escape) {
							// Double escape: \\ -> \
							current.WriteString(escape)
							i = afterEscape + len(escape)
							continue
						}
					}
					// Just a regular escape character
					current.WriteString(escape)
					i += len(escape)
					continue
				}

				current.WriteByte(line[i])
				i++
			}

			fields = append(fields, current.String())
			quoted = append(quoted, true)

			// Skip column delimiter after quoted field
			if i < len(line) && strings.HasPrefix(line[i:], colDel) {
				i += len(colDel)
			}
		} else if strings.HasPrefix(line[i:], colDel) {
			// Empty field between delimiters
			fields = append(fields, "")
			quoted = append(quoted, false)
			i += len(colDel)
		} else {
			// Unquoted field
			current.Reset()
			for i < len(line) && !strings.HasPrefix(line[i:], colDel) {
				current.WriteByte(line[i])
				i++
			}
			fields = append(fields, current.String())
			quoted = append(quoted, false)

			if i < len(line) && strings.HasPrefix(line[i:], colDel) {
				i += len(colDel)
			}
		}
	}

	// Handle trailing delimiter (empty last field)
	if len(line) > 0 && strings.HasSuffix(line, colDel) {
		fields = append(fields, "")
		quoted = append(quoted, false)
	}

	return fields, quoted
}
