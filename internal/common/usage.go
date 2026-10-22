package common

import (
	"fmt"

	"github.com/pkg/errors"
)

// UsageError marks an error as a command-line / parameter problem rather than a
// runtime failure. main.go maps it to exit code 2 (matching DB2's "bad usage"
// code), while any other error exits 1.
type UsageError struct {
	msg string
}

func (e *UsageError) Error() string { return e.msg }

// Usagef builds a UsageError with the usual printf formatting. Validation
// helpers in cmd/ use it so that every rejected option produces the same exit
// code and the same "invalid --option ..." phrasing, and so the message names
// the exact long option the user must fix.
func Usagef(format string, args ...interface{}) error {
	return &UsageError{msg: fmt.Sprintf(format, args...)}
}

// IsUsageError reports whether err (possibly wrapped) is a UsageError.
func IsUsageError(err error) bool {
	var ue *UsageError
	return errors.As(err, &ue)
}
