package _import

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"testing"
)

// The three benchmarks below read the same logical dataset (rows x columns) in
// the three file layouts the SQL import path has to handle, so a change to the
// statement-level parsing can be compared on identical input:
//
//   - single-row: imex's own default since d1b1ee2, one INSERT per line
//   - extended:   --extended-insert / mysqldump style, many tuples per line
//   - wrapped:    a statement split across several physical lines, the layout
//     produced by other tools (and by --statement-size with long rows)
//
// Each row is 8 columns of mixed NULL / number / quoted text, which is what the
// exporter writes for a narrow table.

const (
	benchSQLRows     = 20000
	benchSQLCols     = 8
	benchSQLTuples   = 500 // tuples per multi-row statement
	benchSQLWrapCols = 4   // columns per physical line in the wrapped layout
)

func benchSQLRow(i int) string {
	cols := make([]string, 0, benchSQLCols)
	cols = append(cols, fmt.Sprintf("%d", i))
	cols = append(cols, fmt.Sprintf("'%s'", strings.Repeat("x", 20)))
	cols = append(cols, fmt.Sprintf("'name_%d'", i%997))
	cols = append(cols, "NULL")
	cols = append(cols, fmt.Sprintf("%d.%02d", i%100000, i%100))
	cols = append(cols, fmt.Sprintf("'%d-%02d-%02d 10:20:30'", 2000+i/60000, 1+i%12, 1+i%28))
	cols = append(cols, fmt.Sprintf("0x%08X", i))
	cols = append(cols, fmt.Sprintf("'%s'", strings.Repeat("y", 40)))
	return "(" + strings.Join(cols, ",") + ")"
}

// columnList is the fixed column list every generated statement shares.
const benchSQLColumns = "`id`,`pad`,`nm`,`opt`,`amount`,`ts`,`blob`,`note`"

func benchSQLHeader() string {
	return "CREATE TABLE `t` (`id` INT) ENGINE=InnoDB;\n" +
		"SET NAMES utf8mb4;\n" +
		"LOCK TABLES `t` WRITE;\n"
}

// buildSingleRowSQL: one INSERT statement per physical line.
func buildSingleRowSQL() string {
	var sb strings.Builder
	sb.WriteString(benchSQLHeader())
	for i := 0; i < benchSQLRows; i++ {
		fmt.Fprintf(&sb, "INSERT INTO `t` (%s) VALUES %s;\n", benchSQLColumns, benchSQLRow(i))
	}
	sb.WriteString("UNLOCK TABLES;\n")
	return sb.String()
}

// buildExtendedSQL: benchSQLTuples tuples per statement, one statement per line.
func buildExtendedSQL() string {
	var sb strings.Builder
	sb.WriteString(benchSQLHeader())
	for start := 0; start < benchSQLRows; start += benchSQLTuples {
		end := start + benchSQLTuples
		if end > benchSQLRows {
			end = benchSQLRows
		}
		tuples := make([]string, 0, end-start)
		for i := start; i < end; i++ {
			tuples = append(tuples, benchSQLRow(i))
		}
		fmt.Fprintf(&sb, "INSERT INTO `t` (%s) VALUES %s;\n", benchSQLColumns, strings.Join(tuples, ","))
	}
	sb.WriteString("UNLOCK TABLES;\n")
	return sb.String()
}

// buildWrappedSQL: every statement broken across ceil(benchSQLCols/benchSQLWrapCols)
// physical lines, splitting inside the VALUES list.
func buildWrappedSQL() string {
	var sb strings.Builder
	sb.WriteString(benchSQLHeader())
	for start := 0; start < benchSQLRows; start += benchSQLTuples {
		end := start + benchSQLTuples
		if end > benchSQLRows {
			end = benchSQLRows
		}
		sb.WriteString("INSERT INTO `t` (\n")
		cols := strings.Split(benchSQLColumns, ",")
		for i := 0; i < len(cols); i += benchSQLWrapCols {
			stop := i + benchSQLWrapCols
			if stop > len(cols) {
				stop = len(cols)
			}
			sep := ",\n"
			if stop == len(cols) {
				sep = "\n"
			}
			sb.WriteString(strings.Join(cols[i:stop], ","))
			sb.WriteString(sep)
		}
		sb.WriteString(")\nVALUES\n")
		for i := start; i < end; i++ {
			term := ",\n"
			if i == end-1 {
				term = ";\n"
			}
			sb.WriteString(benchSQLRow(i))
			sb.WriteString(term)
		}
	}
	sb.WriteString("UNLOCK TABLES;\n")
	return sb.String()
}

// drainSQLReader reads every row the reader produces and returns the count. It
// fails the benchmark if the row count or the column list is wrong, so a
// speed-up can never come from skipping work.
func drainSQLReader(b *testing.B, data string) {
	b.Helper()
	sr := NewSQLReader(bufio.NewReader(strings.NewReader(data)))
	cols, err := sr.ReadHeader()
	if err != nil {
		b.Fatalf("ReadHeader: %v", err)
	}
	if len(cols) != benchSQLCols {
		b.Fatalf("columns: got %d (%v), want %d", len(cols), cols, benchSQLCols)
	}
	rows := 0
	for {
		_, err := sr.ReadRow()
		if err == io.EOF {
			break
		}
		if err != nil {
			b.Fatalf("ReadRow: %v", err)
		}
		rows++
	}
	if rows != benchSQLRows {
		b.Fatalf("rows: got %d, want %d", rows, benchSQLRows)
	}
}

func BenchmarkSQLReaderSingleRow(b *testing.B) {
	data := buildSingleRowSQL()
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drainSQLReader(b, data)
	}
}

func BenchmarkSQLReaderExtended(b *testing.B) {
	data := buildExtendedSQL()
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drainSQLReader(b, data)
	}
}

func BenchmarkSQLReaderWrapped(b *testing.B) {
	data := buildWrappedSQL()
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drainSQLReader(b, data)
	}
}
