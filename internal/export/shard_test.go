package export

import (
	"testing"
)

func TestGenerateShardRanges_SingleColumn(t *testing.T) {
	info := &ShardInfo{
		Columns:     []string{"id"},
		ColumnTypes: []string{"int"},
		Mins:        []interface{}{int64(1)},
		Maxs:        []interface{}{int64(1000)},
		IsNumeric:   []bool{true},
	}

	ranges := GenerateShardRanges(info, 4)

	if len(ranges) != 4 {
		t.Errorf("Expected 4 ranges, got %d", len(ranges))
	}

	// Check first range
	if toFloat64(ranges[0].Mins[0]) != 1.0 {
		t.Errorf("Expected first min to be 1, got %v", ranges[0].Mins[0])
	}
	// Range size = (1000-1)/4 = 249.75, so first max = 1 + 249.75 = 250.75
	expectedMax := 1.0 + (1000.0-1.0)/4.0
	if toFloat64(ranges[0].Maxs[0]) != expectedMax {
		t.Errorf("Expected first max to be %v, got %v", expectedMax, ranges[0].Maxs[0])
	}

	// Check last range
	if !ranges[3].IsLast {
		t.Errorf("Expected last range to have IsLast=true")
	}
	if toFloat64(ranges[3].Maxs[0]) != 1000.0 {
		t.Errorf("Expected last max to be 1000, got %v", ranges[3].Maxs[0])
	}
}

func TestGenerateShardRanges_CompositeKey(t *testing.T) {
	info := &ShardInfo{
		Columns:     []string{"tenant_id", "user_id"},
		ColumnTypes: []string{"int", "int"},
		Mins:        []interface{}{int64(1), int64(100)},
		Maxs:        []interface{}{int64(10), int64(1000)},
		IsNumeric:   []bool{true, true},
	}

	ranges := GenerateShardRanges(info, 4)

	if len(ranges) != 4 {
		t.Errorf("Expected 4 ranges, got %d", len(ranges))
	}

	// All ranges should have 2 columns
	for i, r := range ranges {
		if len(r.Mins) != 2 {
			t.Errorf("Range %d: expected 2 min values, got %d", i, len(r.Mins))
		}
		if len(r.Maxs) != 2 {
			t.Errorf("Range %d: expected 2 max values, got %d", i, len(r.Maxs))
		}
	}

	// First column should be divided into ranges
	// tenant_id: [1, 10] divided into 4 parts
	// Each part should be approximately 2.25 wide
	if toFloat64(ranges[0].Mins[0]) != 1.0 {
		t.Errorf("Expected first tenant_id min to be 1, got %v", ranges[0].Mins[0])
	}

	// Second column should use full range for all shards
	for i, r := range ranges {
		if toFloat64(r.Mins[1]) != 100.0 {
			t.Errorf("Range %d: expected user_id min to be 100, got %v", i, r.Mins[1])
		}
		if toFloat64(r.Maxs[1]) != 1000.0 {
			t.Errorf("Range %d: expected user_id max to be 1000, got %v", i, r.Maxs[1])
		}
	}
}

func TestFormatValue(t *testing.T) {
	tests := []struct {
		name     string
		input    interface{}
		expected string
	}{
		{
			name:     "string",
			input:    "test",
			expected: "'test'",
		},
		{
			name:     "string with quote",
			input:    "test's",
			expected: "'test''s'",
		},
		{
			name:     "int",
			input:    123,
			expected: "123",
		},
		{
			name:     "int64",
			input:    int64(456),
			expected: "456",
		},
		{
			name:     "float",
			input:    3.14,
			expected: "3.14",
		},
		{
			name:     "nil",
			input:    nil,
			expected: "NULL",
		},
		{
			name:     "byte slice",
			input:    []byte("data"),
			expected: "'data'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatValue(tt.input)
			if result != tt.expected {
				t.Errorf("formatValue(%v) = %s, want %s", tt.input, result, tt.expected)
			}
		})
	}
}

func TestToFloat64(t *testing.T) {
	tests := []struct {
		name     string
		input    interface{}
		expected float64
	}{
		{"int", 42, 42.0},
		{"int8", int8(8), 8.0},
		{"int16", int16(16), 16.0},
		{"int32", int32(32), 32.0},
		{"int64", int64(64), 64.0},
		{"uint", uint(10), 10.0},
		{"uint8", uint8(8), 8.0},
		{"uint16", uint16(16), 16.0},
		{"uint32", uint32(32), 32.0},
		{"uint64", uint64(64), 64.0},
		{"float32", float32(3.14), 3.14},
		{"float64", 3.14159, 3.14159},
		{"byte slice", []byte("123"), 123.0},
		{"invalid string", []byte("abc"), 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := toFloat64(tt.input)
			// Use epsilon comparison for floating point numbers
			epsilon := 0.0001
			if result < tt.expected-epsilon || result > tt.expected+epsilon {
				t.Errorf("toFloat64(%v) = %f, want %f", tt.input, result, tt.expected)
			}
		})
	}
}
