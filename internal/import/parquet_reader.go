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
}

// NewParquetReader creates a new Parquet format reader.
// file is the input file (already opened by importer); column names are taken
// from the file's schema in schema order.
func NewParquetReader(file *os.File) (*ParquetReader, error) {
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

// ReadRow reads a single row from the Parquet file.
func (p *ParquetReader) ReadRow() ([]interface{}, error) {
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

	values := make([]interface{}, len(p.columns))
	for _, v := range row {
		idx := v.Column()
		if idx < 0 || idx >= len(values) {
			continue
		}
		if v.IsNull() {
			values[idx] = nil
			continue
		}
		values[idx] = parquetValueToInterface(v)
	}

	return values, nil
}

// parquetValueToInterface converts a single parquet.Value into a Go value that
// the importer can bind to a SQL placeholder.
func parquetValueToInterface(v parquet.Value) interface{} {
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
		// Return a []byte copy: byte-array values may carry raw non-UTF8
		// content (BLOB columns stored as plain BYTE_ARRAY), and the value's
		// backing buffer belongs to the row reader and is reused on the next
		// ReadRows call. []byte binds to binary and character columns alike.
		b := v.ByteArray()
		out := make([]byte, len(b))
		copy(out, b)
		return out
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
