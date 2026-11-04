package common

import (
	"encoding/hex"
	"github.com/pkg/errors"
	"os"
	"strings"
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
