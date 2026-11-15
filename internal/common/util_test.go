package common

import (
	"testing"
)

func TestParseDelimiter(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
		wantErr  bool
	}{
		{
			name:     "single character",
			input:    ",",
			expected: ",",
			wantErr:  false,
		},
		{
			name:     "multi-byte string",
			input:    "||",
			expected: "||",
			wantErr:  false,
		},
		{
			name:     "escape sequence tab",
			input:    `\t`,
			expected: "\t",
			wantErr:  false,
		},
		{
			name:     "escape sequence newline",
			input:    `\n`,
			expected: "\n",
			wantErr:  false,
		},
		{
			name:     "escape sequence carriage return",
			input:    `\r`,
			expected: "\r",
			wantErr:  false,
		},
		{
			name:     "escape sequence backslash",
			input:    `\\`,
			expected: "\\",
			wantErr:  false,
		},
		{
			name:     "hex format 0x09 (tab)",
			input:    "0x09",
			expected: "\t",
			wantErr:  false,
		},
		{
			name:     "hex format 0x0D0A (CRLF)",
			input:    "0x0D0A",
			expected: "\r\n",
			wantErr:  false,
		},
		{
			name:     "hex format 0x7C0x7C (||)",
			input:    "0x7C0x7C",
			expected: "||",
			wantErr:  false,
		},
		{
			name:     "hex format uppercase 0X09",
			input:    "0X09",
			expected: "\t",
			wantErr:  false,
		},
		{
			name:     "complex multi-byte",
			input:    ":::",
			expected: ":::",
			wantErr:  false,
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
			wantErr:  true,
		},
		{
			name:     "invalid hex odd length",
			input:    "0x0",
			expected: "",
			wantErr:  true,
		},
		{
			name:     "invalid hex characters",
			input:    "0xGG",
			expected: "",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseDelimiter(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseDelimiter() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && result != tt.expected {
				t.Errorf("ParseDelimiter() = %v, want %v", result, tt.expected)
			}
		})
	}
}

// TestParseEmptyableDelimiters pins the single point where the two merged flags
// differ from the other delimiters. --char-del and --escape accept the empty
// string (bare fields and no escaping respectively, the states the removed
// --no-char-del and --no-escape flags selected), while every other value they
// accept is delegated to ParseDelimiter unchanged. This is the only place in the
// code that has to know the empty value is legal; downstream both spellings of
// each pair are already the same state.
func TestParseEmptyableDelimiters(t *testing.T) {
	parsers := []struct {
		name  string
		parse func(string) (string, error)
	}{
		{"ParseCharDelimiter", ParseCharDelimiter},
		{"ParseEscape", ParseEscape},
	}
	// Both parsers must agree on every non-empty value: neither adds its own
	// interpretation, it just funnels through ParseDelimiter.
	values := []struct {
		input    string
		expected string
		wantErr  bool
	}{
		{"", "", false}, // the one legal exception
		{",", ",", false},
		{`"`, `"`, false},
		{`\t`, "\t", false},
		{`\n`, "\n", false},
		{`\\`, "\\", false},
		{`\0`, "\x00", false},
		{`\x`, `\x`, false}, // unknown escape is kept verbatim
		{"0x09", "\t", false},
		{"0x0D0A", "\r\n", false},
		{"0x7C0x7C", "||", false},
		{"0X2C", ",", false},
		{"§", "§", false},  // multi-byte UTF-8 passes through
		{"0x9", "", true},  // odd-length hex
		{"0xZZ", "", true}, // non-hex digits
		{"0x", "", true},   // bare prefix
	}
	for _, p := range parsers {
		for _, v := range values {
			t.Run(p.name+"/"+v.input, func(t *testing.T) {
				got, err := p.parse(v.input)
				if (err != nil) != v.wantErr {
					t.Fatalf("%s(%q) error = %v, wantErr %v", p.name, v.input, err, v.wantErr)
				}
				if !v.wantErr && got != v.expected {
					t.Errorf("%s(%q) = %q, want %q", p.name, v.input, got, v.expected)
				}
				if v.wantErr {
					// An invalid value must be reported as that value's own error,
					// not silently turned into the empty (disabled) state.
					if p.name == "ParseCharDelimiter" {
						if _, perr := ParseDelimiter(v.input); perr == nil {
							t.Errorf("ParseCharDelimiter(%q) rejected a value ParseDelimiter accepts", v.input)
						}
					}
				}
			})
		}
	}
}

func TestParseDelimiterHexFormats(t *testing.T) {
	// Test various hex formats mentioned in requirements
	testCases := []struct {
		input    string
		expected string
		desc     string
	}{
		{"0x09", "\t", "tab character"},
		{"0x0D0A", "\r\n", "CRLF"},
		{"0x7C0x7C", "||", "double pipe"},
		{"0x2C", ",", "comma"},
		{"0x3A", ":", "colon"},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			result, err := ParseDelimiter(tc.input)
			if err != nil {
				t.Errorf("ParseDelimiter(%s) unexpected error: %v", tc.input, err)
				return
			}
			if result != tc.expected {
				t.Errorf("ParseDelimiter(%s) = %q, want %q", tc.input, result, tc.expected)
			}
		})
	}
}
