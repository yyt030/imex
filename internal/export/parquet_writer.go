package export

import (
	"database/sql"
	"fmt"
	"io"

	"github.com/pkg/errors"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress"
	"github.com/parquet-go/parquet-go/compress/snappy"
	"github.com/parquet-go/parquet-go/compress/zstd"

	"github.com/imex/internal/config"
)

// ParquetWriter writes data in Parquet format.
//
// All MySQL columns are stored as optional UTF-8 strings. This keeps the
// export lossless (the same textual representation used by the DEL format)
// and avoids guessing incompatible physical types for arbitrary result sets.
type ParquetWriter struct {
	writer  *parquet.GenericWriter[map[string]interface{}]
	columns []string
	// pending buffers rows so the underlying generic writer is invoked in
	// batches rather than per-row (a single-row Write pays full call overhead).
	pending []map[string]interface{}
}

// parquetWriteBatch is how many rows to accumulate before handing them to the
// underlying parquet writer.
const parquetWriteBatch = 1000

// DefaultParquetRowsPerRowGroup caps how many rows one row group accumulates
// before it is compressed and flushed. parquet-go's own default is
// math.MaxInt64, which makes the writer buffer the *entire* result set in
// memory: peak RSS grows linearly with the export (~270 B/row measured on a
// 10-column table), so a multi-million-row export costs hundreds of MB and an
// oversized one can exhaust memory. Capping the row group keeps writer memory
// flat while growing the file by well under 0.1% (more, smaller compressed
// blocks).
const DefaultParquetRowsPerRowGroup int64 = 100_000

// parquetRowsPerRowGroup is the value actually applied; it is a variable only
// so tests can exercise the flushing behaviour with a small row count.
var parquetRowsPerRowGroup int64 = DefaultParquetRowsPerRowGroup

// NewParquetWriter creates a new Parquet format writer.
// w is the output writer (typically *os.File), columns defines the schema.
// binaryFlags (optional, may be nil or shorter than columns) marks columns
// whose values are raw bytes: those become plain optional BYTE_ARRAY leaves
// (no UTF-8 logical annotation) so non-UTF8 BLOB content round-trips intact.
// compression names the page codec (none, snappy or zstd); an unknown name is an
// error rather than a silent fall back to an uncompressed file, because the
// caller asked for a smaller file and would have no way to notice it did not get
// one.
func NewParquetWriter(w io.Writer, columns []string, binaryFlags []bool, compression string) (*ParquetWriter, error) {
	if len(columns) == 0 {
		return nil, errors.New("parquet schema requires at least one column")
	}

	codec, err := parquetCodec(compression)
	if err != nil {
		return nil, err
	}

	schema, err := BuildParquetSchema(columns, binaryFlags)
	if err != nil {
		return nil, err
	}

	writer := parquet.NewGenericWriter[map[string]interface{}](
		w,
		&parquet.WriterConfig{Schema: schema, MaxRowsPerRowGroup: parquetRowsPerRowGroup, Compression: codec},
	)

	return &ParquetWriter{
		writer:  writer,
		columns: columns,
		pending: make([]map[string]interface{}, 0, parquetWriteBatch),
	}, nil
}

// parquetCodec resolves a --parquet-compression value to the writer's codec. A
// nil codec is how parquet-go spells "uncompressed", which is the default: the
// codec name is part of the file's footer metadata, so switching it changes the
// product bytes and nothing else.
func parquetCodec(name string) (compress.Codec, error) {
	switch name {
	case "", config.ParquetCompressionNone:
		return nil, nil
	case config.ParquetCompressionSnappy:
		// parquet-go's snappy codec is block compression: cheap enough that a
		// text-heavy export measured +4% wall time for ~1/10 the bytes, and it
		// keeps the writer's memory flat.
		return &snappy.Codec{}, nil
	case config.ParquetCompressionZstd:
		// Default level and one encoder goroutine: the measured cost is +15%
		// export time and a large encoder buffer, which is why this stays opt-in.
		return &zstd.Codec{}, nil
	default:
		return nil, errors.Errorf("unsupported parquet compression %q: must be one of none/snappy/zstd", name)
	}
}

// BuildParquetSchema builds a Parquet schema where every column is an
// optional BYTE_ARRAY. Columns flagged in binaryFlags lose the UTF-8 logical
// annotation (plain BYTE_ARRAY) so arbitrary binary content is preserved;
// the remaining columns keep the STRING annotation for interoperability.
func BuildParquetSchema(columns []string, binaryFlags []bool) (*parquet.Schema, error) {
	group := make(parquet.Group, len(columns))
	for i, col := range columns {
		if col == "" {
			return nil, errors.New("parquet column name must not be empty")
		}
		if _, exists := group[col]; exists {
			return nil, errors.Errorf("duplicate column name %q in parquet schema", col)
		}
		if i < len(binaryFlags) && binaryFlags[i] {
			// Plain BYTE_ARRAY leaf: no UTF8 conversion on read/write.
			group[col] = parquet.Optional(parquet.Leaf(parquet.ByteArrayType))
		} else {
			group[col] = parquet.Optional(parquet.String())
		}
	}
	return parquet.NewSchema("schema", group), nil
}

// WriteHeader is a no-op for Parquet: the schema is defined at construction.
func (p *ParquetWriter) WriteHeader(columns []string) error {
	return nil
}

// WriteRow writes a single row to the Parquet file (buffered).
func (p *ParquetWriter) WriteRow(values []interface{}) error {
	row := make(map[string]interface{}, len(p.columns))
	for i, col := range p.columns {
		if i < len(values) {
			row[col] = parquetStringValue(values[i])
		} else {
			row[col] = nil
		}
	}
	return p.append(row)
}

// WriteRawRow writes a row from sql.RawBytes to the Parquet file (buffered).
//
// Values are sent to the map-based GenericWriter as []byte, not Go string.
// parquet-go v0.26.4 encodes an empty string in map[string]interface{} as a
// null BYTE_ARRAY — the "" case is indistinguishable from nil — which breaks
// round-trip fidelity for empty string/varchar/blob columns. Passing []byte
// preserves both empty (len==0, non-nil) and null (nil) values distinctly.
func (p *ParquetWriter) WriteRawRow(values []sql.RawBytes) error {
	row := make(map[string]interface{}, len(p.columns))
	for i, col := range p.columns {
		if i < len(values) && values[i] != nil {
			// []byte(values[i]) allocates a fresh backing array (values[i] is
			// the driver's internal buffer that will be reused on next Scan);
			// the writer must retain the bytes past this call.
			b := make([]byte, len(values[i]))
			copy(b, values[i])
			row[col] = b
		} else {
			row[col] = nil
		}
	}
	return p.append(row)
}

// append adds a row to the pending buffer and flushes it once the batch
// threshold is reached.
func (p *ParquetWriter) append(row map[string]interface{}) error {
	p.pending = append(p.pending, row)
	if len(p.pending) >= parquetWriteBatch {
		return p.flushPending()
	}
	return nil
}

// flushPending hands the buffered rows to the underlying writer.
func (p *ParquetWriter) flushPending() error {
	if len(p.pending) == 0 {
		return nil
	}
	if _, err := p.writer.Write(p.pending); err != nil {
		return err
	}
	p.pending = p.pending[:0]
	return nil
}

// WriteFooter flushes buffered rows and closes the Parquet writer.
func (p *ParquetWriter) WriteFooter() error {
	if err := p.flushPending(); err != nil {
		return err
	}
	return p.writer.Close()
}

// parquetStringValue converts an arbitrary driver value to a Parquet value.
// SQL NULL -> nil; strings and byte slices are passed through as []byte so
// the parquet-go map writer preserves the empty-string case (an empty Go
// string is silently encoded as null on map[string]interface{} writes).
func parquetStringValue(val interface{}) interface{} {
	if val == nil {
		return nil
	}
	switch v := val.(type) {
	case []byte:
		return v
	case string:
		return []byte(v)
	default:
		return []byte(fmt.Sprintf("%v", v))
	}
}
