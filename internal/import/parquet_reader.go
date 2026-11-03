package _import

import (
	"github.com/pkg/errors"
	"io"
	"os"

	"github.com/parquet-go/parquet-go"
)

// ParquetReader reads data in Parquet format.
//
// Rows are decoded through the low-level parquet.Row API so arbitrary schemas
// produced by other tools can be read without knowing their Go types.
type ParquetReader struct {
	file    *os.File
	reader  *parquet.Reader
	columns []string
	rowBuf  []parquet.Row
	bufIdx  int
	bufSize int

	// binaryCols marks, per file position, the columns that hold raw bytes.
	// Everything else is bound as text. See SetBinaryColumns.
	binaryCols []bool
}

// NewParquetReader creates a new Parquet format reader.
// file is the input file (already opened by importer); column names are taken
// from the file's schema in schema order.
func NewParquetReader(file *os.File) (rdr *ParquetReader, err error) {
	// parquet-go panics on structurally invalid input (bad magic header/footer,
	// truncated metadata) instead of returning an error. A corrupt input file
	// must be a clean runtime failure, not a stack dump, so convert any panic
	// into an error here.
	defer func() {
		if rec := recover(); rec != nil {
			rdr = nil
			err = errors.Errorf("invalid parquet file: %v", rec)
		}
	}()

	info, err := file.Stat()
	if err != nil {
		return nil, errors.Wrap(err, "failed to stat parquet file")
	}
	if info.Size() == 0 {
		return nil, errors.New("parquet file is empty")
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, errors.Wrap(err, "failed to seek parquet file")
	}

	reader := parquet.NewReader(file)

	fields := reader.Schema().Fields()
	columns := make([]string, len(fields))
	for i, field := range fields {
		columns[i] = field.Name()
	}
	if len(columns) == 0 {
		reader.Close()
		return nil, errors.New("parquet file has no columns")
	}

	return &ParquetReader{
		file:    file,
		reader:  reader,
		columns: columns,
		rowBuf:  nil,
		bufIdx:  0,
		bufSize: 1000,
	}, nil
}

// ReadHeader returns the column names of the Parquet file.
func (p *ParquetReader) ReadHeader() ([]string, error) {
	return p.columns, nil
}

// SetBinaryColumns marks which file columns carry raw bytes. The exporter writes
// a bytea or BLOB column as its stored bytes and every other column as that
// column's text, so the two need different Go bind types: a byte slice binds as
// a binary value, and on PostgreSQL that is the only thing a bytea column takes.
// Without the mark every byte-array cell goes out as a byte slice, which
// PostgreSQL hands to a uuid column as its own bytes and to an integer[] column
// as one array element per byte — measured on 14.24, both silently wrong and
// reported as a successful insert.
//
// The flags arrive aligned to the file's column order, from the same
// backend.MetadataBinary classification the DEL reader uses.
func (p *ParquetReader) SetBinaryColumns(flags []bool) {
	p.binaryCols = flags
}

// isBinaryColumn reports whether the file column at idx carries raw bytes.
func (p *ParquetReader) isBinaryColumn(idx int) bool {
	return idx < len(p.binaryCols) && p.binaryCols[idx]
}

// ReadRow reads a single row from the Parquet file.
func (p *ParquetReader) ReadRow() (values []interface{}, err error) {
	// Same reason as NewParquetReader: a damaged page can panic inside the
	// library rather than report an error.
	defer func() {
		if rec := recover(); rec != nil {
			values = nil
			err = errors.Errorf("invalid parquet file: %v", rec)
		}
	}()

	if p.bufIdx >= len(p.rowBuf) {
		p.rowBuf = make([]parquet.Row, p.bufSize)
		n, err := p.reader.ReadRows(p.rowBuf)
		if err != nil && err != io.EOF {
			return nil, errors.Wrap(err, "failed to read parquet rows")
		}
		if n == 0 {
			return nil, io.EOF
		}
		p.rowBuf = p.rowBuf[:n]
		p.bufIdx = 0
	}

	row := p.rowBuf[p.bufIdx]
	p.bufIdx++

	values = make([]interface{}, len(p.columns))
	for _, v := range row {
		idx := v.Column()
		if idx < 0 || idx >= len(values) {
			continue
		}
		if v.IsNull() {
			values[idx] = nil
			continue
		}
		values[idx] = parquetValueToInterface(v, p.isBinaryColumn(idx))
	}

	return values, nil
}

// parquetValueToInterface converts a single parquet.Value into a Go value that
// the importer can bind to a SQL placeholder.
//
// `binary` says the column's own content is bytes (a MySQL BLOB or a PostgreSQL
// bytea). The writers store every column as a BYTE_ARRAY — a non-binary column
// holds that column's text, a binary one holds its raw bytes — so the two cases
// cannot be told apart from the file alone, and they must be bound as different
// Go types: only a byte slice reaches a binary column as bytes, and a byte slice
// is what corrupts the columns that are not.
func parquetValueToInterface(v parquet.Value, binary bool) interface{} {
	switch v.Kind() {
	case parquet.Boolean:
		return v.Boolean()
	case parquet.Int32:
		return v.Int32()
	case parquet.Int64:
		return v.Int64()
	case parquet.Float:
		return v.Float()
	case parquet.Double:
		return v.Double()
	case parquet.ByteArray, parquet.FixedLenByteArray:
		// The value's backing buffer belongs to the row reader and is reused on
		// the next ReadRows call, so both branches copy out of it. An empty text
		// value has to stay an empty string rather than become NULL, which is
		// what the writer's []byte-on-write preserved on this side.
		b := v.ByteArray()
		if binary {
			out := make([]byte, len(b))
			copy(out, b)
			return out
		}
		return string(b)
	default:
		return v.String()
	}
}

// Close closes the Parquet reader and the underlying file.
func (p *ParquetReader) Close() error {
	if p.reader != nil {
		if err := p.reader.Close(); err != nil {
			return err
		}
	}
	return nil
}
