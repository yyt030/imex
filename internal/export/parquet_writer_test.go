package export

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/format"

	"github.com/imex/internal/config"
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

	w, err := NewParquetWriter(f, []string{"id", "name"}, nil, config.ParquetCompressionNone)
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

// TestParquetWriterAppliesTheRequestedCodec pins what --parquet-compression
// actually writes: the codec is part of each column chunk's footer metadata, so
// this reads it back with parquet-go instead of trusting the constructor. The
// default must stay UNCOMPRESSED — switching it would change the bytes of every
// existing Parquet export.
func TestParquetWriterAppliesTheRequestedCodec(t *testing.T) {
	cases := []struct {
		name  string
		codec string
		want  format.CompressionCodec
	}{
		{"none is the default and means uncompressed", config.ParquetCompressionNone, format.Uncompressed},
		{"snappy", config.ParquetCompressionSnappy, format.Snappy},
		{"zstd", config.ParquetCompressionZstd, format.Zstd},
		{"empty behaves like none", "", format.Uncompressed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out.parquet")
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			w, err := NewParquetWriter(f, []string{"id", "s"}, nil, c.codec)
			if err != nil {
				t.Fatalf("NewParquetWriter(%q): %v", c.codec, err)
			}
			for i := 0; i < 200; i++ {
				if err := w.WriteRow([]interface{}{int64(i), "abcdefghijabcdefghij"}); err != nil {
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
			md := file.Metadata()
			if len(md.RowGroups) == 0 || len(md.RowGroups[0].Columns) == 0 {
				t.Fatal("no column chunks written")
			}
			for _, ch := range md.RowGroups[0].Columns {
				if got := ch.MetaData.Codec; got != c.want {
					t.Fatalf("codec = %s, want %s", got, c.want)
				}
			}
		})
	}
}

func TestParquetWriterRejectsUnknownCodec(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "out.parquet"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := NewParquetWriter(f, []string{"id"}, nil, "gzip"); err == nil {
		t.Fatal("NewParquetWriter accepted an unknown codec: a silent fall back to uncompressed would hide the typo")
	}
}
