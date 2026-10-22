package export

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/parquet-go/parquet-go"
)

// parquetRowsPerRowGroup is the tunable behind the writer's row-group cap. A
// test that overrides it must restore it; the production default is asserted
// separately so an accidental revert to parquet-go's unbounded default
// (math.MaxInt64, which buffers the whole dataset in RAM) is caught.
func TestParquetWriter_RowGroupCapWiring(t *testing.T) {
	saved := parquetRowsPerRowGroup
	parquetRowsPerRowGroup = 1000
	defer func() { parquetRowsPerRowGroup = saved }()

	path := filepath.Join(t.TempDir(), "out.parquet")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}

	w, err := NewParquetWriter(f, []string{"id", "name"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2500; i++ {
		if err := w.WriteRow([]interface{}{int64(i), "value"}); err != nil {
			t.Fatalf("write row %d: %v", i, err)
		}
	}
	if err := w.WriteFooter(); err != nil {
		t.Fatalf("WriteFooter: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	pf, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	file, err := parquet.OpenFile(pf, st.Size())
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if got := len(file.RowGroups()); got != 3 {
		t.Fatalf("row groups = %d, want 3 (2500 rows capped at %d per group)", got, parquetRowsPerRowGroup)
	}
}

func TestParquetDefaultRowGroupCapIsBounded(t *testing.T) {
	if DefaultParquetRowsPerRowGroup <= 0 || DefaultParquetRowsPerRowGroup > 1_000_000 {
		t.Fatalf("DefaultParquetRowsPerRowGroup = %d: must be a bounded row count, not parquet-go's unbounded default", DefaultParquetRowsPerRowGroup)
	}
}
