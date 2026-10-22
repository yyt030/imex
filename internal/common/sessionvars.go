package common

import (
	"regexp"
	"strings"

	"github.com/pkg/errors"
)

// reservedDSNParams are DSN parameter names that the tool itself sets or that
// the go-sql-driver treats specially (they configure the driver/connection
// rather than a MySQL session variable, and "strict" even panics on parse).
// A session variable using one of these names would be silently hijacked at
// DSN re-parse time, so it is rejected up front.
var reservedDSNParams = map[string]bool{
	// set by the tool (see BuildDSN)
	"charset":           true,
	"parseTime":         true,
	"interpolateParams": true,
	"multiStatements":   true,
	"clientFoundRows":   true,
	"tls":               true,
	"client-cert":       true,
	"client-key":        true,
	"ca-cert":           true,
	"tenant":            true,
	// interpreted by the driver's DSN parser
	"allowAllFiles":            true,
	"allowCleartextPasswords":  true,
	"allowFallbackToPlaintext": true,
	"allowNativePasswords":     true,
	"allowOldPasswords":        true,
	"checkConnLiveness":        true,
	"collation":                true,
	"columnsWithAlias":         true,
	"compress":                 true,
	"connectionAttributes":     true,
	"loc":                      true,
	"maxAllowedPacket":         true,
	"readTimeout":              true,
	"rejectReadOnly":           true,
	"serverPubKey":             true,
	"strict":                   true,
	"timeTruncate":             true,
	"timeout":                  true,
	"writeTimeout":             true,
}

// validVarName matches MySQL/OceanBase system variable names.
var validVarName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// numericValue matches plain numbers, which must stay unquoted in SET.
var numericValue = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

// keywordValue matches literals that are valid unquoted in SET.
var keywordValue = regexp.MustCompile(`(?i)^(default|on|off|true|false|null)$`)

// quoteSessionVarValue turns a user-supplied value into a safe SQL literal
// for the "SET name = value" statement the driver emits per connection.
//
//   - numbers and the bare keywords DEFAULT/ON/OFF/TRUE/FALSE/NULL pass
//     through unquoted;
//   - everything else is wrapped in single quotes with inner single quotes
//     doubled (valid both with and without NO_BACKSLASH_ESCAPS);
//   - backslashes are rejected, since escaping them correctly depends on the
//     sql_mode that is in effect when the statement runs.
func quoteSessionVarValue(value string) (string, error) {
	if numericValue.MatchString(value) || keywordValue.MatchString(value) {
		return value, nil
	}
	if strings.Contains(value, "\\") {
		return "", errors.Errorf("value %q contains a backslash, which cannot be escaped safely", value)
	}
	if value == "" {
		return "''", nil
	}
	return "'" + strings.ReplaceAll(value, "'", "''") + "'", nil
}

// ParseSessionVars parses --session-var flag values of the form "name=value"
// into a map ready for DSN injection. The flag is repeatable precisely because
// values may legally contain commas (sql_mode="STRICT,NO_ZERO_DATE"), so pairs
// are never split on commas.
func ParseSessionVars(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		if strings.TrimSpace(p) == "" {
			return nil, errors.New("--session-var: empty entry")
		}
		name, value, found := strings.Cut(p, "=")
		if !found {
			return nil, errors.Errorf(`--session-var %q: expected name=value`, p)
		}
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !validVarName.MatchString(name) {
			return nil, errors.Errorf("--session-var %q: invalid variable name (must match [A-Za-z][A-Za-z0-9_]*)", name)
		}
		if reservedDSNParams[name] {
			return nil, errors.Errorf("--session-var %q: this parameter is managed by the tool or the driver and cannot be overridden", name)
		}
		if strings.ContainsAny(value, "`") || strings.Contains(value, "/*") || strings.Contains(value, "--") {
			return nil, errors.Errorf("--session-var %q: value contains forbidden characters (backtick or comment sequences)", p)
		}
		quoted, err := quoteSessionVarValue(value)
		if err != nil {
			return nil, errors.Wrapf(err, "--session-var %q", p)
		}
		out[name] = quoted
	}
	return out, nil
}
