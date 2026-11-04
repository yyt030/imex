package backend

import (
	"errors"
	"math"

	"gitee.com/chunanyong/dm"

	"github.com/imex/internal/common"
)

// transientDamengCodes lists the Dameng driver-level error codes that indicate a
// connection-level failure rather than a problem with the statement itself.
// These are the codes observed in the driver source (za.go) as connection or
// communication errors; the table will be refined once it is measured against a
// live DM8 server.
var transientDamengCodes = map[uint16]string{
	6001:  "communication error",
	9007:  "unknown network",
	9008:  "invalid connection",
	20001: "connection switch failed",
}

// damengServerCode reads the number out of the driver's own error type. ok is
// true for any *dm.DmError, including the numbers above that are deliberately
// absent — the server answered, which is what closes the case.
//
// ErrCode is an int32 while the verdict table is keyed by uint16 (the shape
// common.CodeVerdict takes). Negative numbers are the driver's data- and
// statement-level failures, and a positive number above the 16-bit range cannot
// collide with any key in that table, so both are reported as 0 — "answered, not
// transient" — rather than truncated into a number that might be in it.
func damengServerCode(err error) (uint16, bool) {
	var dmErr *dm.DmError
	if errors.As(err, &dmErr) {
		if dmErr.ErrCode < 0 || dmErr.ErrCode > math.MaxUint16 {
			return 0, true
		}
		return uint16(dmErr.ErrCode), true
	}
	return 0, false
}

// damengVerdict reads the code table through common's shared machinery.
var damengVerdict = common.CodeVerdict(damengServerCode, transientDamengCodes)

// TransientError is the Dameng half of the retry decision.
func (damengBackend) TransientError(err error) (bool, bool) {
	return damengVerdict(err)
}
