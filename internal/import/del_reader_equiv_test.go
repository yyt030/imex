package _import

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"

	"github.com/imex/internal/config"
)

// The reader's hot path was rewritten from "per-position HasPrefix over a
// freshly built substring, byte-by-byte into a strings.Builder" to "cached
// delimiter specs, byte compares, fields sliced out of the record". The
// references below are the previous implementations, kept verbatim, so the new
// code has to agree with them on every input instead of only on the cases
// someone thought to write down.

func parseLineRef(line string, mod *config.DelModifier) ([]string, []bool) {
	colDel := mod.ColDel
	charDel := mod.CharDel
	escape := mod.Escape
	noEscape := mod.NoEscape

	var fields []string
	var quoted []bool
	var current strings.Builder
	i := 0

	for i < len(line) {
		if !mod.NoCharDel && charDel != "" && strings.HasPrefix(line[i:], charDel) {
			i += len(charDel)
			current.Reset()

			for i < len(line) {
				if strings.HasPrefix(line[i:], charDel) {
					if !noEscape && escape == charDel && i+len(charDel) < len(line) && strings.HasPrefix(line[i+len(charDel):], charDel) {
						current.WriteString(charDel)
						i += 2 * len(charDel)
						continue
					}
					i += len(charDel)
					break
				}

				if !noEscape && escape != "" && escape != charDel && strings.HasPrefix(line[i:], escape) {
					afterEscape := i + len(escape)
					if afterEscape < len(line) {
						if strings.HasPrefix(line[afterEscape:], charDel) {
							current.WriteString(charDel)
							i = afterEscape + len(charDel)
							continue
						} else if strings.HasPrefix(line[afterEscape:], escape) {
							current.WriteString(escape)
							i = afterEscape + len(escape)
							continue
						}
					}
					current.WriteString(escape)
					i += len(escape)
					continue
				}

				current.WriteByte(line[i])
				i++
			}

			fields = append(fields, current.String())
			quoted = append(quoted, true)

			if i < len(line) && strings.HasPrefix(line[i:], colDel) {
				i += len(colDel)
			}
		} else if strings.HasPrefix(line[i:], colDel) {
			fields = append(fields, "")
			quoted = append(quoted, false)
			i += len(colDel)
		} else {
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

	if len(line) > 0 && strings.HasSuffix(line, colDel) {
		fields = append(fields, "")
		quoted = append(quoted, false)
	}

	return fields, quoted
}

func scanRowsRef(d *DelReader) bufioSplitFunc {
	return func(data []byte, atEOF bool) (advance int, token []byte, err error) {
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
					if !noEscape && len(escape) > 0 && bytes.Equal(escape, charDel) &&
						bytes.HasPrefix(data[pos+len(charDel):], charDel) {
						pos += 2 * len(charDel)
						continue
					}
					inQuote = false
					pos += len(charDel)
					continue
				}
				if !noEscape && len(escape) > 0 && !bytes.Equal(escape, charDel) &&
					bytes.HasPrefix(data[pos:], escape) {
					next := pos + len(escape)
					switch {
					case len(charDel) > 0 && bytes.HasPrefix(data[next:], charDel):
						pos = next + len(charDel)
					case bytes.HasPrefix(data[next:], escape):
						pos = next + len(escape)
					case next < len(data):
						pos = next + 1
					case atEOF:
						pos = next
					default:
						return 0, nil, nil
					}
					continue
				}
				pos++
				continue
			}

			if len(rowDel) > 0 && bytes.HasPrefix(data[pos:], rowDel) {
				return pos + len(rowDel), data[:pos], nil
			}
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
		return 0, nil, nil
	}
}

type bufioSplitFunc = func(data []byte, atEOF bool) (advance int, token []byte, err error)

// delModVariants are the delimiter configurations the differential tests run
// over: the defaults, the --no-escape / --no-char-del forms, multi-byte
// delimiters, and hex-style multi-byte escapes.
func delModVariants() []*config.DelModifier {
	return []*config.DelModifier{
		{CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `"`},
		{CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `\`},
		{CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `"`, NoEscape: true},
		{CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `"`, NoCharDel: true},
		{CharDel: "'", ColDel: "||", RowDel: "\r\n", Escape: "''"},
		{CharDel: `"`, ColDel: ";", RowDel: "\n", Escape: "\\"},
	}
}

func TestDelReader_ParseLineMatchesReference(t *testing.T) {
	cases := []string{
		``,
		`"a"`,
		`"a","b"`,
		`a,b`,
		`"a",b,"c"`,
		`,,`,
		`"a",,`,
		`,"a"`,
		`""`,
		`"",""`,
		`"Alice ""The Great""","Bob"`,
		`"Alice \"The Great\"","Bob"`,
		`"unterminated,bob`,
		`"ends with escape\`,
		`"ends with escape"`,
		`trailing,`,
		`"a"""`,
		`"""a"`,
		`"""","""`,
		`a"b`,
		`"a\`,
		`x,"y""z",`,
		`"tab	within","nl` + "\n" + `within"`,
		`"\x","y"`,
		`"a""`,
		`"a""b""c"`,
		`"a\\"`,
		`"'quoted,inside'",x`,
	}

	for _, mod := range delModVariants() {
		m := mod
		for _, line := range cases {
			t.Run(m.ColDel+"/"+strings.TrimSpace(line), func(t *testing.T) {
				d := NewDelReader(nil, m, false)
				gotF, gotQ := d.parseLine([]byte(line), nil, nil)
				wantF, wantQ := parseLineRef(line, m)
				if len(gotF) != len(wantF) {
					t.Fatalf("field count = %d (%q), want %d (%q)", len(gotF), gotF, len(wantF), wantF)
				}
				for i := range wantF {
					if gotF[i] != wantF[i] {
						t.Errorf("field[%d] = %q, want %q (line %q)", i, gotF[i], wantF[i], line)
					}
				}
				for i := range wantQ {
					if gotQ[i] != wantQ[i] {
						t.Errorf("quoted[%d] = %v, want %v (line %q)", i, gotQ[i], wantQ[i], line)
					}
				}
			})
		}
	}
}

// TestDelReader_ParseLineRandom feeds generated records to both
// implementations. The alphabet is deliberately dense in delimiters.
func TestDelReader_ParseLineRandom(t *testing.T) {
	alphabet := []string{`"`, `'`, ",", ";", "||", "\\", `\`, "a", "bc", "", "\r\n", " "}
	rng := rand.New(rand.NewSource(20261023))
	for _, mod := range delModVariants() {
		m := mod
		t.Run(m.ColDel, func(t *testing.T) {
			d := NewDelReader(nil, m, false)
			for n := 0; n < 4000; n++ {
				var b strings.Builder
				k := rng.Intn(24)
				for i := 0; i < k; i++ {
					b.WriteString(alphabet[rng.Intn(len(alphabet))])
				}
				line := b.String()
				gotF, gotQ := d.parseLine([]byte(line), nil, nil)
				wantF, wantQ := parseLineRef(line, m)
				if len(gotF) != len(wantF) {
					t.Fatalf("line %q: field count = %d (%q), want %d (%q)", line, len(gotF), gotF, len(wantF), wantF)
				}
				for i := range wantF {
					if gotF[i] != wantF[i] || gotQ[i] != wantQ[i] {
						t.Fatalf("line %q: field[%d] = %q/%v, want %q/%v", line, i, gotF[i], gotQ[i], wantF[i], wantQ[i])
					}
				}
			}
		})
	}
}

// TestDelReader_ScanRowsMatchesReference compares record framing on generated
// documents, byte-for-byte, including where each record starts and ends.
func TestDelReader_ScanRowsMatchesReference(t *testing.T) {
	chunks := []string{`"`, `'`, ",", "\n", "\r\n", "\\", "x", "yz", "  ", "''", "||", `""`}
	rng := rand.New(rand.NewSource(991177))
	for _, mod := range delModVariants() {
		m := mod
		t.Run(m.RowDel, func(t *testing.T) {
			d := NewDelReader(nil, m, false)
			ref := NewDelReader(nil, m, false)
			splitRef := scanRowsRef(ref)
			for n := 0; n < 400; n++ {
				var b bytes.Buffer
				k := rng.Intn(40)
				for i := 0; i < k; i++ {
					b.WriteString(chunks[rng.Intn(len(chunks))])
				}
				doc := b.Bytes()
				got := collectRecords(d.scanRows, doc)
				want := collectRecords(splitRef, doc)
				if len(got) != len(want) {
					t.Fatalf("doc %q: %d records %q, want %d %q", doc, len(got), got, len(want), want)
				}
				for i := range want {
					if !bytes.Equal(got[i], want[i]) {
						t.Fatalf("doc %q: record[%d] = %q, want %q", doc, i, got[i], want[i])
					}
				}
			}
		})
	}
}

// collectRecords drives a SplitFunc the way bufio/recordReader does: it keeps
// consuming from the front of the window until the splitter stops advancing.
func collectRecords(split bufioSplitFunc, doc []byte) [][]byte {
	var out [][]byte
	pos := 0
	for pos <= len(doc) {
		advance, token, err := split(doc[pos:], true)
		if err != nil {
			panic(err)
		}
		if advance == 0 {
			break
		}
		out = append(out, append([]byte(nil), token...))
		pos += advance
	}
	return out
}

// FuzzDelReaderParse keeps the differential property check running under
// clustering: any input that makes the fast parser disagree with the
// reference is a bug, and the empty-field/quote-ness signal must survive.
func FuzzDelReaderParse(f *testing.F) {
	for _, s := range []string{`"a","b"`, `"a""b"`, `"a\`, `,,`, `x,"y",`, `"||"`, `''''`} {
		f.Add([]byte(s))
	}
	mods := delModVariants()
	f.Fuzz(func(t *testing.T, line []byte) {
		for _, m := range mods {
			mod := m
			d := NewDelReader(nil, mod, false)
			gotF, gotQ := d.parseLine(line, nil, nil)
			wantF, wantQ := parseLineRef(string(line), mod)
			if len(gotF) != len(wantF) {
				t.Fatalf("line %q (col %q): field count = %d (%q), want %d (%q)", line, mod.ColDel, len(gotF), gotF, len(wantF), wantF)
			}
			for i := range wantF {
				if gotF[i] != wantF[i] || gotQ[i] != wantQ[i] {
					t.Fatalf("line %q (col %q): field[%d] = %q/%v, want %q/%v", line, mod.ColDel, i, gotF[i], gotQ[i], wantF[i], wantQ[i])
				}
			}
		}
	})
}
