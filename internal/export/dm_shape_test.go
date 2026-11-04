package export

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/imex/internal/backend"
	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

// This file pins the DEL text a Dameng export writes for boundary values,
// without a Dameng server (the sandbox has never had one). It is a shape test,
// not a round trip: a fake driver hands the real export pipeline —
// exportSingle, backend.CellFixups, DelWriter — exactly the Go values the DM
// driver's getColumnData returns for each column type (string for the character
// and decimal families, []byte for the binary families, time.Time for every
// date/time type, bool for BOOLEAN, int8 for BIT, nil for NULL), so what the
// file says is the tool's own product. The one thing no fake can supply is the
// spelling the server reports for a type name; that is what
// internal/backend/dameng_test.go pins per name instead.

type dmShapeCol struct {
	name string
	typ  string
	val  driver.Value
}

func dmShapeLocation(off int) *time.Location {
	return time.FixedZone("dm", off*3600)
}

// dmShapeColumns is the fixture: one column per answer the driver gives back.
func dmShapeColumns(nulls bool) []dmShapeCol {
	stamped := time.Date(2024, 5, 6, 7, 8, 9, 123456000, dmShapeLocation(8))
	clockOnly := time.Date(1, 1, 1, 15, 4, 5, 0, dmShapeLocation(8))
	if nulls {
		stamped, clockOnly = time.Time{}, time.Time{}
	}
	cols := []dmShapeCol{
		{"ID", "NUMBER", dmShapeValue(nulls, "7")},
		{"DT", "DATETIME", dmShapeTime(nulls, stamped)},
		{"TS", "TIMESTAMP(6)", dmShapeTime(nulls, stamped)},
		{"D", "DATE", dmShapeTime(nulls, stamped)},
		{"TM", "TIME", dmShapeTime(nulls, clockOnly)},
		{"TSTZ", "TIMESTAMP WITH TIME ZONE", dmShapeTime(nulls, stamped)},
		{"FLAG", "BIT", dmShapeValue(nulls, int8(1))},
		{"BOOL", "BOOLEAN", dmShapeValue(nulls, true)},
		{"LB", "LONGVARBINARY", dmShapeValue(nulls, []byte{0xff, 0x00})},
		{"B", "BLOB", dmShapeValue(nulls, []byte{0x00, 0x01, 'a'})},
		{"S", "VARCHAR", dmShapeValue(nulls, "plain")},
	}
	return cols
}

func dmShapeValue(nulls bool, v driver.Value) driver.Value {
	if nulls {
		return nil
	}
	return v
}

func dmShapeTime(nulls bool, v time.Time) driver.Value {
	if nulls {
		return nil
	}
	return v
}

var dmShapeSeq int64

type dmShapeDriver struct{ cols []dmShapeCol }

func (d *dmShapeDriver) Open(string) (driver.Conn, error) { return &dmShapeConn{cols: d.cols}, nil }

type dmShapeConn struct{ cols []dmShapeCol }

func (c *dmShapeConn) Prepare(string) (driver.Stmt, error) { return &dmShapeStmt{cols: c.cols}, nil }
func (c *dmShapeConn) Close() error                        { return nil }
func (c *dmShapeConn) Begin() (driver.Tx, error) {
	return nil, fmt.Errorf("dm shape fixture: no transactions")
}

type dmShapeStmt struct{ cols []dmShapeCol }

func (s *dmShapeStmt) Close() error  { return nil }
func (s *dmShapeStmt) NumInput() int { return -1 }
func (s *dmShapeStmt) Exec([]driver.Value) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}
func (s *dmShapeStmt) Query([]driver.Value) (driver.Rows, error) {
	return &dmShapeRows{cols: s.cols}, nil
}

type dmShapeRows struct {
	cols []dmShapeCol
	done bool
}

func (r *dmShapeRows) Columns() []string {
	out := make([]string, len(r.cols))
	for i, c := range r.cols {
		out[i] = c.name
	}
	return out
}
func (r *dmShapeRows) Close() error { return nil }
func (r *dmShapeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	for i, c := range r.cols {
		dest[i] = c.val
	}
	return nil
}

func (r *dmShapeRows) ColumnTypeDatabaseTypeName(i int) string { return r.cols[i].typ }

// writeDmShapeDump runs the real single-threaded export path over the fixture
// and returns the file contents.
func writeDmShapeDump(t *testing.T, cols []dmShapeCol) string {
	t.Helper()

	name := fmt.Sprintf("dmshape-%d", atomic.AddInt64(&dmShapeSeq, 1))
	sql.Register(name, &dmShapeDriver{cols: cols})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	out := fmt.Sprintf("%s/dm-shape-%s.del", t.TempDir(), name)
	cfg := &config.ExportConfig{
		Config: config.Config{Engine: "dm", DefaultCharset: "utf8mb4",
			ProgressInterval: time.Hour},
		Output: out,
		Format: "del",
		Query:  "SELECT * FROM IMEX_SHAPE.T1",
		DelModifier: config.DelModifier{
			CharDel: `"`, ColDel: ",", RowDel: "\n", Escape: `"`,
			TimestampFmt: "yyyy-MM-dd HH:mm:ss", DateFmt: "yyyy-MM-dd",
		},
	}
	e := &Exporter{config: cfg, be: backend.Must(backend.EngineDM), db: db,
		stopMonitor: func() {}, stats: common.NewStats()}
	if err := e.exportSingle(context.Background(), time.Now()); err != nil {
		t.Fatalf("exportSingle: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	return string(data)
}

// TestExportDmBoundaryCellText pins the record a Dameng export writes for one
// row of boundary values. Every field spelling is the point: DATETIME and TIME
// had no rewriter and left the file as ISO text with a zone, BOOLEAN came out as
// "true", LONGVARBINARY was treated as text and wrote its raw bytes into a
// quoted field, and a zone-bearing timestamp dropped its offset.
func TestExportDmBoundaryCellText(t *testing.T) {
	got := writeDmShapeDump(t, dmShapeColumns(false))

	wantHeader := `"ID","DT","TS","D","TM","TSTZ","FLAG","BOOL","LB","B","S"` + "\n"
	wantRow := `"7","2024-05-06 07:08:09.123456","2024-05-06 07:08:09.123456",` +
		`"2024-05-06 07:08:09","15:04:05","2024-05-06 07:08:09.123456+08:00",` +
		`"1","1",0xff00,0x000161,"plain"` + "\n"
	if got != wantHeader+wantRow {
		t.Errorf("dump text:\n got %q\nwant %q", got, wantHeader+wantRow)
	}
	// The two signatures of the defects this guards, stated separately so a
	// failure names the cause rather than just the diff.
	if strings.Contains(got, "T07:08:09") {
		t.Error("RFC3339 text reached the file: a time-bearing column has no layout")
	}
	if strings.Contains(got, "true") || strings.Contains(got, "false") {
		t.Error("Go boolean text reached the file: BOOLEAN is not normalised")
	}
}

// TestExportDmNullRowKeepsTheEmptyFieldContract is the other half of the file
// shape: every NULL must stay an unquoted empty field, including in the binary
// columns, so the import side binds NULL rather than an empty value.
func TestExportDmNullRowKeepsTheEmptyFieldContract(t *testing.T) {
	got := writeDmShapeDump(t, dmShapeColumns(true))
	// Eleven fields, ten delimiters: a NULL in a binary column is an empty
	// unquoted field, not "0x", which is the empty value.
	want := `"ID","DT","TS","D","TM","TSTZ","FLAG","BOOL","LB","B","S"` + "\n" +
		",,,,,,,,,,\n"
	if got != want {
		t.Errorf("NULL dump:\n got %q\nwant %q", got, want)
	}
}
