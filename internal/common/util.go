package common

import (
	"encoding/hex"
	"fmt"
	"github.com/pkg/errors"
	"os"
	"strings"

	"github.com/go-sql-driver/mysql"

	"github.com/imex/internal/config"
)

// ParseDelimiter parses a delimiter string that may be in one of these formats:
//   - Single character: ","
//   - Multi-byte string: "||", ":::"
//   - Escape sequences: "\t", "\n", "\r"
//   - Hex format: "0x09" (tab), "0x0D0A" (CRLF), "0x7C0x7C" (||)
func ParseDelimiter(s string) (string, error) {
	if s == "" {
		return "", errors.New("delimiter cannot be empty")
	}

	// Check for hex format: starts with "0x" or "0X"
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		return parseHexDelimiter(s)
	}

	// Process escape sequences
	return processEscapes(s), nil
}

// parseHexDelimiter parses a hex-encoded delimiter string.
// Supports formats like "0x09", "0x0D0A", "0x7C0x7C".
func parseHexDelimiter(s string) (string, error) {
	// Remove all "0x" / "0X" prefixes to get the raw hex string
	hexStr := strings.ReplaceAll(s, "0x", "")
	hexStr = strings.ReplaceAll(hexStr, "0X", "")

	if len(hexStr) == 0 {
		return "", errors.Errorf("invalid hex delimiter: %s", s)
	}

	if len(hexStr)%2 != 0 {
		return "", errors.Errorf("invalid hex delimiter (odd length): %s", s)
	}

	b, err := hex.DecodeString(hexStr)
	if err != nil {
		return "", errors.Wrapf(err, "invalid hex delimiter %s", s)
	}

	return string(b), nil
}

// processEscapes processes common escape sequences in a string.
func processEscapes(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))

	i := 0
	for i < len(s) {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 't':
				sb.WriteByte('\t')
				i += 2
			case 'n':
				sb.WriteByte('\n')
				i += 2
			case 'r':
				sb.WriteByte('\r')
				i += 2
			case '\\':
				sb.WriteByte('\\')
				i += 2
			case '0':
				sb.WriteByte(0)
				i += 2
			default:
				sb.WriteByte(s[i])
				i++
			}
		} else {
			sb.WriteByte(s[i])
			i++
		}
	}

	return sb.String()
}

// FileSize returns the size of path in bytes, or -1 if it cannot be stat'ed.
// Used for logging the produced/consumed file byte count; never fails hard.
func FileSize(path string) int64 {
	if path == "" {
		return -1
	}
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return -1
	}
	return fi.Size()
}

// BuildDSN renders the connection string for a Config. It uses mysql.Config so
// special characters in the username and password are escaped rather than
// reinterpreted as DSN syntax.
//
// OceanBase usernames are passed through verbatim; the tenant and cluster are
// part of the user string (`root@sys`, `root@sys#obcluster`), not separate
// options, which is why the removed --tenant flag has no replacement here.
func BuildDSN(c *config.Config) string {
	cfg := mysql.NewConfig()
	cfg.User = c.User
	cfg.Passwd = c.Password
	cfg.DBName = c.Database
	cfg.Params = make(map[string]string)
	cfg.Params["charset"] = c.DefaultCharset
	// parseTime is intentionally disabled: the exporter reads every column as
	// sql.RawBytes (the exact text stored by the server). If parseTime were
	// enabled, DATE/DATETIME/TIMESTAMP columns would be returned as Go
	// time.Time and rendered as RFC3339 ("2026-01-01T10:00:00Z"), which is not
	// a valid MySQL DATETIME literal and breaks round-tripping.
	cfg.Params["parseTime"] = "false"
	cfg.Params["interpolateParams"] = "true"
	cfg.Params["multiStatements"] = "false"
	cfg.Params["clientFoundRows"] = "false"

	if c.Socket != "" {
		cfg.Net = "unix"
		cfg.Addr = c.Socket
	} else {
		cfg.Net = "tcp"
		cfg.Addr = fmt.Sprintf("%s:%d", c.Host, c.Port)
	}

	// Timeouts are expressed with the driver's own fields so they survive
	// FormatDSN/ParseDSN round-tripping. connectTimeout is a dial deadline;
	// readTimeout/writeTimeout bound a single socket read or write, which is
	// what actually stops a stalled server from hanging a long export.
	cfg.Timeout = c.ConnectTimeout
	cfg.ReadTimeout = c.SocketReadTimeout
	cfg.WriteTimeout = c.SocketWriteTimeout

	// --session-var values travel as extra DSN parameters. The driver executes
	// "SET <name> = <value>" for every parameter it does not recognise, on every
	// NEW physical connection (connector.go -> handleParams). That is the only
	// mechanism that reliably survives database/sql pooling: the main export
	// connection, every shard worker and every parallel import worker carry the
	// variables without extra lifecycle hooks.
	for name, value := range c.Vars {
		if reservedDSNParams[name] {
			// Defensive: ParseSessionVars already rejects these.
			continue
		}
		cfg.Params[name] = value
	}

	return cfg.FormatDSN()
}
