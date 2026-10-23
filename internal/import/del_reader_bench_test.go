package _import

import (
	"bufio"
	"bytes"
	"strconv"
	"testing"

	"github.com/gomydump/internal/common"
	"github.com/gomydump/internal/config"
)

// Corpus for the DEL reader benchmarks. Shapes mirror what the exporter
// actually writes: every non-NULL value quoted, an empty *quoted* field for ”
// (an unquoted empty field is NULL), and the hard cases a real dump contains —
// doubled quotes inside a value, an unquoted 0x… token in a binary column, and
// a value carrying the column delimiter inside quotes.
const benchColumns = 11

func benchModifier() *config.DelModifier {
	return &config.DelModifier{
		CharDel:      `"`,
		ColDel:       ",",
		RowDel:       "\n",
		Escape:       `"`,
		TimestampFmt: "yyyy-MM-dd HH:mm:ss",
		DateFmt:      "yyyy-MM-dd",
	}
}

func benchKinds() []common.ColumnKind {
	return []common.ColumnKind{
		common.KindNumber,    // id
		common.KindUnknown,   // name
		common.KindUnknown,   // descr
		common.KindNumber,    // n1
		common.KindNumber,    // ratio
		common.KindTimestamp, // ts
		common.KindDate,      // dt
		common.KindUnknown,   // note
		common.KindUnknown,   // empty ('' or NULL)
		common.KindUnknown,   // tag
		common.KindUnknown,   // vb (binary, 0x token)
	}
}

func benchBinaries() []bool {
	// Only the last column is binary in the benchmark schema; the 0x token in
	// the other columns must stay verbatim text.
	bin := make([]bool, benchColumns)
	bin[10] = true
	return bin
}

// benchCorpus builds a deterministic DEL document of n data rows plus a header.
// Layout (mirrors what the exporter writes): every non-NULL value is quoted, an
// empty *quoted* field is the empty string, an unquoted empty field is NULL, and
// the binary column carries an unquoted 0x… token.
func benchCorpus(n int) []byte {
	var buf bytes.Buffer
	buf.Grow(n * 180)
	buf.WriteString(`"id","name","descr","n1","ratio","ts","dt","note","empty","tag","vb"`)
	buf.WriteByte('\n')
	q := func(s string) string { return `"` + s + `"` }
	for i := 1; i <= n; i++ {
		id := strconv.Itoa(i)
		tag := q("tag_" + id)
		if i%97 == 0 {
			// Doubled quote inside a quoted value: needs un-escaping.
			tag = q(`he said ""hi"" ` + id)
		}
		descr := repeatWords(i, "desc1-desc2-")
		if i%53 == 0 {
			// Column delimiter inside a quoted value.
			descr = q("a,b;" + id)
		}
		empty := `""`
		if i%11 == 0 {
			// NULL: unquoted empty field.
			empty = ""
		}
		buf.WriteString(q(id))
		buf.WriteByte(',')
		buf.WriteString(q("name_" + id))
		buf.WriteByte(',')
		buf.WriteString(descr)
		buf.WriteByte(',')
		buf.WriteString(q(id))
		buf.WriteByte(',')
		buf.WriteString(q(strconv.FormatFloat(float64(i%100)/3, 'f', 2, 64)))
		buf.WriteByte(',')
		buf.WriteString(q("2020-09-13 12:26:" + pad(i%60)))
		buf.WriteByte(',')
		buf.WriteString(q("2020-01-" + pad(i%28+1)))
		buf.WriteByte(',')
		buf.WriteString(q("note text payload " + repeat(i, 'x')))
		buf.WriteByte(',')
		buf.WriteString(empty)
		buf.WriteByte(',')
		buf.WriteString(tag)
		buf.WriteByte(',')
		buf.WriteString("0x" + hexBytes(i))
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

func repeat(n int, c byte) string {
	if n%5 == 0 {
		n *= 2
	}
	b := make([]byte, n%24+1)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

func repeatWords(n int, word string) string {
	out := make([]byte, 0, len(word)*3)
	for i := 0; i <= n%3; i++ {
		out = append(out, word...)
	}
	out = append(out, strconv.Itoa(n%997)...)
	return `"` + string(out) + `"`
}

func pad(i int) string {
	if i < 10 {
		return "0" + strconv.Itoa(i)
	}
	return strconv.Itoa(i)
}

func hexBytes(n int) string {
	const hexdigits = "0123456789ABCDEF"
	out := make([]byte, 0, 16)
	for i := 0; i < 8; i++ {
		v := byte((n + i*37) % 256)
		out = append(out, hexdigits[v>>4], hexdigits[v&0x0f])
	}
	return string(out)
}

// BenchmarkDelReaderReadRow measures the full row path: record framing, field
// splitting and per-value conversion. This is the cost the import pipeline pays
// per row before a batch reaches the database.
func BenchmarkDelReaderReadRow(b *testing.B) {
	corpus := benchCorpus(20000)
	b.SetBytes(int64(len(corpus)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rd := NewDelReader(bufio.NewReader(bytes.NewReader(corpus)), benchModifier(), true)
		rd.SetColumnKinds(benchKinds())
		rd.SetBinaryColumns(benchBinaries())
		if _, err := rd.ReadHeader(); err != nil {
			b.Fatal(err)
		}
		rows := 0
		for {
			_, err := rd.ReadRow()
			if err != nil {
				break
			}
			rows++
		}
		if rows != 20000 {
			b.Fatalf("rows = %d, want 20000", rows)
		}
	}
}

// BenchmarkDelReaderScan isolates record framing (scanRows + recordReader) from
// field parsing, so a regression in the splitter stays visible on its own.
func BenchmarkDelReaderScan(b *testing.B) {
	corpus := benchCorpus(20000)
	b.SetBytes(int64(len(corpus)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rd := NewDelReader(bufio.NewReader(bytes.NewReader(corpus)), benchModifier(), true)
		if _, err := rd.ReadHeader(); err != nil {
			b.Fatal(err)
		}
		rows := 0
		for rd.rd.Scan() {
			_ = rd.rd.Bytes()
			rows++
		}
		if rows != 20000 {
			b.Fatalf("rows = %d, want 20000", rows)
		}
		if err := rd.rd.Err(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDelReaderParseOnly measures field parsing over pre-framed rows,
// with no recordReader or bufio in the loop.
func BenchmarkDelReaderParseOnly(b *testing.B) {
	corpus := benchCorpus(20000)
	rd := NewDelReader(bufio.NewReader(bytes.NewReader(corpus)), benchModifier(), true)
	if _, err := rd.ReadHeader(); err != nil {
		b.Fatal(err)
	}
	lines := make([][]byte, 0, 20000)
	for rd.rd.Scan() {
		lines = append(lines, append([]byte(nil), rd.rd.Bytes()...))
	}
	if len(lines) != 20000 {
		b.Fatalf("lines = %d, want 20000", len(lines))
	}
	total := 0
	for _, l := range lines {
		total += len(l)
	}
	b.SetBytes(int64(total))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := NewDelReader(nil, benchModifier(), false)
		for _, line := range lines {
			p.parseLine(line, nil, nil)
		}
	}
}
