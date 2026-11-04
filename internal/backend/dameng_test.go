package backend

import (
	"errors"
	"testing"

	"gitee.com/chunanyong/dm"
)

// TestDamengTimeText pins the rewriter CellFixups installs for DATE and
// TIMESTAMP columns. The DM driver hands database/sql a time.Time, which the
// stdlib renders as RFC3339Nano text — an ISO "T", a zone offset and nanoseconds
// no Oracle-mode literal uses. The rewriter turns it back into server-style text
// and leaves anything it cannot parse alone.
func TestDamengTimeText(t *testing.T) {
	dateText := dmTimeText("2006-01-02 15:04:05")
	tsText := dmTimeText("2006-01-02 15:04:05.999999")

	for _, c := range []struct {
		layout string
		in     string
		want   string
	}{
		// Oracle-mode DATE carries a time component, so it must survive.
		{"date", "2024-05-06T07:08:09Z", "2024-05-06 07:08:09"},
		// The wall clock is kept: no conversion to UTC.
		{"date", "2024-05-06T07:08:09+08:00", "2024-05-06 07:08:09"},
		{"date", "2024-05-06T07:08:09.5Z", "2024-05-06 07:08:09"},
		{"timestamp", "2024-05-06T07:08:09Z", "2024-05-06 07:08:09"},
		{"timestamp", "2024-05-06T07:08:09.123456Z", "2024-05-06 07:08:09.123456"},
		{"timestamp", "2024-05-06T07:08:09.100Z", "2024-05-06 07:08:09.1"},
		// Values the driver did not render as a time are passed through: a NULL
		// marker or an already-formatted cell must not be rewritten into the zero
		// time.
		{"date", "NULL", "NULL"},
		{"timestamp", "", ""},
	} {
		f := dateText
		if c.layout == "timestamp" {
			f = tsText
		}
		if got := string(f([]byte(c.in))); got != c.want {
			t.Errorf("%s %q -> %q, want %q", c.layout, c.in, got, c.want)
		}
	}
}

// TestDamengTransientVerdict pins the retry table: the four driver-level
// connection errors are transient, a server answer that is not one of them is
// final, and an error the driver never numbered is left to common's neutral
// rules.
func TestDamengTransientVerdict(t *testing.T) {
	b := damengBackend{}
	for _, code := range []int32{6001, 9007, 9008, 20001} {
		transient, decided := b.TransientError(&dm.DmError{ErrCode: code})
		if !transient || !decided {
			t.Errorf("code %d: got (%v,%v), want (true,true)", code, transient, decided)
		}
	}
	// A negative code is how the driver numbers data- and statement-level
	// failures (its own ECGO_DATA_OVERFLOW is -6102): the case is closed, no
	// retry.
	if transient, decided := b.TransientError(&dm.DmError{ErrCode: -6102}); transient || !decided {
		t.Errorf("server error: got (%v,%v), want (false,true)", transient, decided)
	}
	// A code outside the table's 16-bit range must not be truncated into it:
	// 6001 + 65536 collides with the communication error under a naive cast.
	if transient, decided := b.TransientError(&dm.DmError{ErrCode: 6001 + 1<<16}); transient || !decided {
		t.Errorf("out-of-range code: got (%v,%v), want (false,true) — the number must not wrap into the table", transient, decided)
	}
	// Something the driver did not number leaves the decision open.
	if transient, decided := b.TransientError(errors.New("dial tcp: connection refused")); transient || decided {
		t.Errorf("unnumbered error: got (%v,%v), want (false,false)", transient, decided)
	}
}

// TestDamengUpsertIsNotImplemented records why --mode insert_update is refused:
// the suffix builder itself errors, so a library caller that skipped the CLI
// refusal still gets a failure rather than a MySQL ON DUPLICATE KEY clause.
func TestDamengUpsertIsNotImplemented(t *testing.T) {
	_, _, err := damengBackend{}.UpsertSuffix("RT", []string{"A"}, []string{"?"})
	if err == nil {
		t.Fatal("dm upsert must fail until it is measured against a server")
	}
	if (damengBackend{}).UpsertNeedsTarget() {
		t.Error("UpsertNeedsTarget must be false while the suffix is unimplemented")
	}
}

// TestDamengTimeNamesAreRewritten is the anti-drift guard. gitee.com/dm's
// zzn.go:getColumnData answers exactly one group of column types with a Go
// time.Time — DATE, TIME, DATETIME, TIME_TZ, DATETIME_TZ, DATETIME2 and
// DATETIME2_TZ — and database/sql renders any time.Time handed to sql.RawBytes
// as RFC3339Nano text, which no Dameng or MySQL literal accepts. Every spelling
// such a column can reach the dump under therefore has to carry a layout, or the
// dump silently starts writing ISO text: this is the shape that made a TIME
// column classified but never rewritten.
func TestDamengTimeNamesAreRewritten(t *testing.T) {
	for _, name := range []string{
		"DATE", "TIME", "TIME WITH TIME ZONE",
		"DATETIME", "DATETIME2", "TIMESTAMP",
		"DATETIME WITH TIME ZONE", "TIMESTAMP WITH TIME ZONE",
		"TIMESTAMP WITH LOCAL TIME ZONE",
		// suffix and case tolerance: what the catalogue actually spells.
		"TIMESTAMP(6)", "timestamp with time zone", " DATETIME(3) ",
	} {
		if fix := damengCellFixup(name); fix == nil {
			t.Errorf("%q: no rewriter, the dump would carry RFC3339 text", name)
		}
	}
	// A text column must not gain one by accident.
	if fix := damengCellFixup("VARCHAR"); fix != nil {
		t.Error("VARCHAR must not be rewritten")
	}
}

// TestDamengCellFixupTexts pins the file text each time-bearing name produces
// for the same driver value. The wall clock is never shifted; a zone-bearing
// column keeps its offset, because a dump that drops it no longer identifies
// the instant it read — the same choice the PostgreSQL dialect makes for
// timestamptz. The layout without an offset is what makes a plain TIMESTAMP
// column land in a MySQL DATETIME unchanged.
func TestDamengCellFixupTexts(t *testing.T) {
	const in = "2024-05-06T07:08:09.123456+08:00"
	for _, c := range []struct {
		name, want string
	}{
		{"DATE", "2024-05-06 07:08:09"},
		{"TIME", "07:08:09.123456"},
		{"TIME WITH TIME ZONE", "07:08:09.123456+08:00"},
		{"DATETIME", "2024-05-06 07:08:09.123456"},
		{"TIMESTAMP", "2024-05-06 07:08:09.123456"},
		{"TIMESTAMP WITH TIME ZONE", "2024-05-06 07:08:09.123456+08:00"},
		{"TIMESTAMP WITH LOCAL TIME ZONE", "2024-05-06 07:08:09.123456+08:00"},
	} {
		got := string(damengCellFixup(c.name)([]byte(in)))
		if got != c.want {
			t.Errorf("%s: %q -> %q, want %q", c.name, in, got, c.want)
		}
	}
	// A UTC instant keeps its offset marker, and a zero TIME-of-day keeps six
	// fraction digits out of the picture rather than printing a bare ".0".
	if got := string(damengCellFixup("TIMESTAMP WITH TIME ZONE")(
		[]byte("2024-05-06T07:08:09Z"))); got != "2024-05-06 07:08:09+00:00" {
		t.Errorf("UTC timestamptz -> %q, want %q", got, "2024-05-06 07:08:09+00:00")
	}
	if got := string(damengCellFixup("TIME")([]byte("0001-01-01T15:04:05+08:00"))); got != "15:04:05" {
		t.Errorf("TIME -> %q, want %q", got, "15:04:05")
	}
	// Anything the driver did not render as a time is left alone: a NULL cell
	// must not become the zero date.
	for _, name := range []string{"DATE", "TIMESTAMP", "TIME"} {
		if got := string(damengCellFixup(name)([]byte("NULL"))); got != "NULL" {
			t.Errorf("%s passthrough -> %q", name, got)
		}
	}
}

// TestDamengBoolText pins the BOOLEAN normalisation: the driver answers a
// BOOLEAN column with a Go bool, database/sql turns that into "true"/"false",
// and neither spelling is a literal a MySQL-family numeric column takes (both
// Error 1366 and Error 1406 are what the cross-engine test measured). One digit
// is what a TINYINT(1) target stores correctly, and a BIT column already arrives
// as a digit, so it must pass through untouched.
func TestDamengBoolText(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"true", "1"}, {"false", "0"}, {"TRUE", "1"},
		{"1", "1"}, {"0", "0"}, {"", ""}, {"NULL", "NULL"},
	} {
		if got := string(damengCellFixup("BOOLEAN")([]byte(c.in))); got != c.want {
			t.Errorf("BOOLEAN %q -> %q, want %q", c.in, got, c.want)
		}
	}
	if got := string(damengCellFixup("BIT")([]byte("1"))); got != "1" {
		t.Errorf("BIT %q -> %q", "1", got)
	}
}

// TestDamengBinaryNames covers the large-binary family the driver spells without
// a space (zzn.go / i.go use LONGVARBINARY) next to the spaced catalogue
// spelling. A name missing here is exported as text: the raw bytes land inside a
// quoted field, the file stops being valid in its own charset, and whether the
// value survives the import depends on the target column type rather than on the
// dump.
func TestDamengBinaryNames(t *testing.T) {
	for _, name := range []string{"BLOB", "RAW", "LONG RAW", "BINARY", "VARBINARY",
		"LONGVARBINARY", "LONG VARBINARY", "IMAGE", "blob(8)"} {
		if !isDamengBinaryType(name) {
			t.Errorf("%q must be binary", name)
		}
	}
	for _, name := range []string{"CLOB", "TEXT", "LONG", "LONGVARCHAR", "LONG VARCHAR",
		"VARCHAR", "CHAR", "NUMBER", "DATE"} {
		if isDamengBinaryType(name) {
			t.Errorf("%q must not be binary", name)
		}
	}
}

// TestDamengTypeTableSingleSource guards the table itself: every entry must be
// reachable through the three public questions, and a name that carries a layout
// must be one the driver answers with a time (so a text type can never be
// rewritten by accident).
func TestDamengTypeTableSingleSource(t *testing.T) {
	timeBearing := map[string]bool{
		"DATE": true, "TIME": true, "TIME WITH TIME ZONE": true,
		"DATETIME": true, "DATETIME2": true, "TIMESTAMP": true,
		"DATETIME WITH TIME ZONE": true, "TIMESTAMP WITH TIME ZONE": true,
		"TIMESTAMP WITH LOCAL TIME ZONE": true,
	}
	for name, typ := range damengTypes {
		if typ.textLayout != "" && !timeBearing[name] {
			t.Errorf("%s: layout on a non-time type", name)
		}
		if got := classifyDamengType(name); got != typ.kind {
			t.Errorf("%s: kind %v, table says %v", name, got, typ.kind)
		}
		if got := isDamengBinaryType(name); got != typ.binary {
			t.Errorf("%s: binary %v, table says %v", name, got, typ.binary)
		}
		if typ.binary && typ.textLayout != "" {
			t.Errorf("%s: a binary column cannot also be re-rendered", name)
		}
	}
	// The name that started this: classified as a time, so it must be rewritten.
	if damengCellFixup("TIME") == nil {
		t.Error("TIME is KindTime but has no rewriter")
	}
}
