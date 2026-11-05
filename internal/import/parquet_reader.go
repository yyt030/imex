package _import

import (
	"io"
	"os"

	"github.com/pkg/errors"

	"github.com/parquet-go/parquet-go"

	"github.com/imex/internal/common"
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

	// kinds marks, per file position, the target column's type kind. Only
	// KindTimestampTZ is acted on today — see SetColumnKinds.
	kinds []common.ColumnKind

	// binds holds the target engine's value adapter per file position, from
	// backend.BindFixups; nil entries (and every other engine) leave the value
	// exactly as it was read. See SetBindFixups.
	binds []func(interface{}) interface{}

	// rawCols marks, per file position, the columns whose own content the file
	// says is bytes rather than annotated text. See rawColumnsOf.
	rawCols []bool
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

	schema := reader.Schema()
	fields := schema.Fields()
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
		rawCols: rawColumnsOf(schema, columns),
		rowBuf:  nil,
		bufIdx:  0,
		bufSize: 1000,
	}, nil
}

// rawColumnsOf asks the file which of its byte-array columns carry content bytes
// rather than annotated text. imex writes a binary column as a plain BYTE_ARRAY
// leaf and a text column as parquet.String(), so the presence of a logical type
// is the file's own statement about its bytes — which no target table can say,
// because the same file may be loaded into a column of any type.
//
// A file whose leaves do not line up one per column (a nested schema) gets all
// false: nothing is known about its annotations from here, and reading it as
// text is what this reader has always done.
func rawColumnsOf(schema *parquet.Schema, columns []string) []bool {
	paths := schema.Columns()
	if len(paths) != len(columns) {
		return nil
	}
	raw := make([]bool, len(paths))
	for i, path := range paths {
		leaf, ok := schema.Lookup(path...)
		if !ok || leaf.ColumnIndex != i {
			return nil
		}
		t := leaf.Node.Type()
		if t.Kind() != parquet.ByteArray && t.Kind() != parquet.FixedLenByteArray {
			continue
		}
		// An annotated column says what its bytes mean (text, a decimal, a date);
		// a plain BYTE_ARRAY with no logical type at all is the file's way of
		// saying these are the column's content bytes. Measured on files this
		// tool writes: a text column carries LogicalType.UTF8 (and parquet-go
		// reconstructs the same one from a legacy ConvertedType UTF8), while a
		// BLOB, VARBINARY or BIT column has neither a logical type nor a
		// converted type.
		raw[i] = t.LogicalType() == nil
	}
	return raw
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

// SetColumnKinds marks which file columns are zone-bearing timestamps. A
// timestamp with a UTC offset is stored as the dump's own text, and Dameng's
// Oracle-mode parser only takes that text with a space before the offset; the
// same rule the DEL reader applies, in the same helper. See
// separateTimestampOffset.
//
// The kinds arrive aligned to the file's column order, from the same
// backend.MetadataKind classification the DEL reader uses.
func (p *ParquetReader) SetColumnKinds(kinds []common.ColumnKind) {
	p.kinds = kinds
}

// isTimestampTZColumn reports whether the file column at idx holds timestamp
// text with a UTC offset.
func (p *ParquetReader) isTimestampTZColumn(idx int) bool {
	return idx < len(p.kinds) && p.kinds[idx] == common.KindTimestampTZ
}

// SetBindFixups supplies the per-position value adapters the target engine needs
// between the reader and the driver placeholder. A nil at a position leaves the
// value exactly as it was read. See DelReader.SetBindFixups for why the question
// exists at all; the numbers are in internal/backend/bind_dameng.go.
func (p *ParquetReader) SetBindFixups(fixups []func(interface{}) interface{}) {
	p.binds = fixups
}

// isRawColumn reports whether the file's own schema says the bytes under this
// column are the column's content rather than its text.
func (p *ParquetReader) isRawColumn(idx int) bool {
	return idx < len(p.rawCols) && p.rawCols[idx]
}

// hasBindFixup reports whether the target engine wants a word about this column.
func (p *ParquetReader) hasBindFixup(idx int) bool {
	return idx < len(p.binds) && p.binds[idx] != nil
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
		// A column the file itself marks as content bytes is read as bytes only
		// where the target engine has something to do about that (see the note on
		// parquetValueToInterface); a text target's adapter turns the bytes back
		// into the string they used to be, so engines that answer nil for every
		// adapter — MySQL and PostgreSQL — read exactly what they always did.
		raw := p.isRawColumn(idx) && p.hasBindFixup(idx)
		value := parquetValueToInterface(v, p.isBinaryColumn(idx) || raw)
		if f := p.bindFixup(idx); f != nil {
			value = f(value)
		}
		if text, ok := value.(string); ok && p.isTimestampTZColumn(idx) {
			value = separateTimestampOffset(text)
		}
		values[idx] = value
	}

	return values, nil
}

// bindFixup returns the target engine's adapter for one file position, or nil.
func (p *ParquetReader) bindFixup(idx int) func(interface{}) interface{} {
	if idx >= len(p.binds) {
		return nil
	}
	return p.binds[idx]
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
