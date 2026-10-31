package list

import (
	"io"
	"strings"
	"unicode/utf8"
)

// nullText is what table prints for SQL NULL. A string whose value is the four
// letters NULL is therefore indistinguishable from NULL.
const nullText = "NULL"

// escaper makes control characters visible: a backslash in the data becomes
// two, while the backslash the replacement introduces is not escaped again.
var escaper = strings.NewReplacer(
	`\`, `\\`,
	"\t", `\t`,
	"\r", `\r`,
	"\n", `\n`,
)

func newRenderer(columns []string, header bool, out io.Writer) *tableRenderer {
	return &tableRenderer{out: out, columns: columns, header: header}
}

func cellText(v *string) string {
	if v == nil {
		return nullText
	}
	return escaper.Replace(*v)
}

// tableRenderer buffers rows because column widths are only known after the
// last row, then aligns every column by rune width with two spaces between
// columns. Buffering is the reason table is the only renderer left; it trades
// memory for alignment, which is its only reason to exist.
type tableRenderer struct {
	out     io.Writer
	columns []string
	header  bool
	rows    [][]string
}

func (r *tableRenderer) WriteRow(row []*string) error {
	cells := make([]string, len(row))
	for i, v := range row {
		cells[i] = cellText(v)
	}
	r.rows = append(r.rows, cells)
	return nil
}

func (r *tableRenderer) Close() error {
	widths := make([]int, len(r.columns))
	if r.header {
		for i, name := range r.columns {
			widths[i] = utf8.RuneCountInString(name)
		}
	}
	for _, row := range r.rows {
		for i, cell := range row {
			if n := utf8.RuneCountInString(cell); n > widths[i] {
				widths[i] = n
			}
		}
	}

	var b strings.Builder
	writeLine := func(cells []string) {
		for i, cell := range cells {
			if i > 0 {
				b.WriteString("  ")
			}
			b.WriteString(cell)
			if i < len(cells)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(cell)))
			}
		}
		b.WriteByte('\n')
	}
	if r.header {
		writeLine(r.columns)
	}
	for _, row := range r.rows {
		writeLine(row)
	}
	_, err := io.WriteString(r.out, b.String())
	return err
}

// Render is a convenience wrapper used by tests: it feeds every row through the
// same renderer the command uses.
func Render(out io.Writer, columns []string, header bool, rows [][]*string) error {
	r := newRenderer(columns, header, out)
	for _, row := range rows {
		if err := r.WriteRow(row); err != nil {
			return err
		}
	}
	return r.Close()
}
