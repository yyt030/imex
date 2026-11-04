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
