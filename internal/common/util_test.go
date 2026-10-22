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
