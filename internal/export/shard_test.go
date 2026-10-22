package export

import (
	"fmt"
	"math/big"
	"testing"
)

// boundaryText renders a range boundary for assertions.
func boundaryText(v interface{}) string {
	if b, ok := v.(shardBoundary); ok {
		return string(b)
	}
	return fmt.Sprint(v)
}

func TestGenerateShardRanges_SingleColumn(t *testing.T) {
	info := &ShardInfo{
		Columns:     []string{"id"},
		ColumnTypes: []string{"int"},
		Mins:        []interface{}{[]byte("1")},
		Maxs:        []interface{}{[]byte("1000")},
		IsNumeric:   []bool{true},
	}

	ranges := GenerateShardRanges(info, 4)

	if len(ranges) != 4 {
		t.Fatalf("Expected 4 ranges, got %d", len(ranges))
	}
	if got := boundaryText(ranges[0].Mins[0]); got != "1" {
		t.Errorf("first min = %s, want 1", got)
	}
	// (1000-1)/4 = 249.75, so the first boundary is 250.75.
	if got := boundaryText(ranges[0].Maxs[0]); got != "250.75" {
		t.Errorf("first max = %s, want 250.75", got)
	}
	if !ranges[3].IsLast {
		t.Error("Expected last range to have IsLast=true")
	}
	if got := boundaryText(ranges[3].Maxs[0]); got != "1000" {
		t.Errorf("last max = %s, want the exact maximum 1000", got)
	}
	for i := 0; i+1 < len(ranges); i++ {
		if a, b := boundaryText(ranges[i].Maxs[0]), boundaryText(ranges[i+1].Mins[0]); a != b {
			t.Fatalf("range %d max %s != range %d min %s", i, a, i+1, b)
		}
	}
}

func TestGenerateShardRanges_CompositeKey(t *testing.T) {
	info := &ShardInfo{
		Columns:     []string{"tenant_id", "user_id"},
		ColumnTypes: []string{"int", "int"},
		Mins:        []interface{}{[]byte("1"), []byte("100")},
		Maxs:        []interface{}{[]byte("10"), []byte("1000")},
		IsNumeric:   []bool{true, true},
	}

	ranges := GenerateShardRanges(info, 4)
	if len(ranges) != 4 {
		t.Fatalf("Expected 4 ranges, got %d", len(ranges))
	}
	for i, r := range ranges {
		if len(r.Mins) != 2 || len(r.Maxs) != 2 {
			t.Fatalf("Range %d: expected 2 columns, got %d/%d", i, len(r.Mins), len(r.Maxs))
		}
		if got := boundaryText(r.Mins[1]); got != "100" {
			t.Errorf("Range %d: second column min = %s, want 100", i, got)
		}
		if got := boundaryText(r.Maxs[1]); got != "1000" {
			t.Errorf("Range %d: second column max = %s, want 1000", i, got)
		}
	}
	if got := boundaryText(ranges[0].Mins[0]); got != "1" {
		t.Errorf("first boundary = %s, want 1", got)
	}
}

// A BIGINT key above 2^53 used to be rounded through float64, which moved the
// final MAX down and silently dropped the highest rows from every shard.
func TestGenerateShardRanges_PreservesBigIntPrecision(t *testing.T) {
	info := &ShardInfo{
		Columns:     []string{"id"},
		ColumnTypes: []string{"bigint"},
		Mins:        []interface{}{[]byte("9007199254740993")},
		Maxs:        []interface{}{[]byte("9007199254740999")},
		IsNumeric:   []bool{true},
	}

	ranges := GenerateShardRanges(info, 2)
	if len(ranges) != 2 {
		t.Fatalf("Expected 2 ranges, got %d", len(ranges))
	}
	if got := boundaryText(ranges[0].Mins[0]); got != "9007199254740993" {
		t.Errorf("first min = %s, want 9007199254740993", got)
	}
	if got := boundaryText(ranges[0].Maxs[0]); got != "9007199254740996" {
		t.Errorf("middle boundary = %s, want 9007199254740996", got)
	}
	if got := boundaryText(ranges[1].Maxs[0]); got != "9007199254740999" {
		t.Errorf("last max = %s, want the exact maximum 9007199254740999", got)
	}
	if ranges[1].Maxs[0] == ranges[0].Maxs[0] {
		t.Fatal("final shard collapsed onto the middle boundary")
	}
}

func TestGenerateShardRanges_DecimalBoundariesStayContiguous(t *testing.T) {
	info := &ShardInfo{
		Columns:     []string{"price"},
		ColumnTypes: []string{"decimal"},
		Mins:        []interface{}{[]byte("0.1")},
		Maxs:        []interface{}{[]byte("0.9")},
		IsNumeric:   []bool{true},
	}

	ranges := GenerateShardRanges(info, 3)
	if len(ranges) != 3 {
		t.Fatalf("Expected 3 ranges, got %d", len(ranges))
	}
	for i := 0; i+1 < len(ranges); i++ {
		a, b := boundaryText(ranges[i].Maxs[0]), boundaryText(ranges[i+1].Mins[0])
		if a != b {
			t.Fatalf("range %d max %s != range %d min %s", i, a, i+1, b)
		}
		if _, ok := new(big.Rat).SetString(a); !ok {
			t.Fatalf("boundary %q is not a valid numeric literal", a)
		}
	}
	if got := boundaryText(ranges[2].Maxs[0]); got != "0.9" {
		t.Errorf("last max = %s, want 0.9", got)
	}
}

func TestGenerateShardRanges_NonNumericFallsBack(t *testing.T) {
	info := &ShardInfo{
		Columns:     []string{"uuid"},
		ColumnTypes: []string{"varchar"},
		Mins:        []interface{}{"a"},
		Maxs:        []interface{}{"z"},
		IsNumeric:   []bool{false},
	}

	ranges := GenerateShardRanges(info, 4)
	if len(ranges) != 1 {
		t.Fatalf("Expected a single fallback range, got %d", len(ranges))
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

func TestGenerateShardRanges_NullKeyCoverage(t *testing.T) {
	t.Run("single nullable column gets an IS NULL shard", func(t *testing.T) {
		info := &ShardInfo{
			Columns:   []string{"k"},
			Mins:      []interface{}{[]byte("1")},
			Maxs:      []interface{}{[]byte("10")},
			IsNumeric: []bool{true},
			Nullable:  []bool{true},
		}
		ranges := GenerateShardRanges(info, 3)
		if len(ranges) != 4 {
			t.Fatalf("want 3 range shards + 1 NULL shard, got %d", len(ranges))
		}
		if !ranges[3].NullOnly {
			t.Error("last shard must be the NULL shard")
		}
		for i := 0; i < 3; i++ {
			if ranges[i].NullOnly {
				t.Errorf("shard %d unexpectedly marked NullOnly", i)
			}
		}
	})

	t.Run("not-null key gets no NULL shard", func(t *testing.T) {
		info := &ShardInfo{
			Columns:   []string{"id"},
			Mins:      []interface{}{[]byte("1")},
			Maxs:      []interface{}{[]byte("10")},
			IsNumeric: []bool{true},
			Nullable:  []bool{false},
		}
		if got := len(GenerateShardRanges(info, 4)); got != 4 {
			t.Errorf("shards = %d, want 4", got)
		}
	})

	t.Run("legacy info without nullability stays sharded", func(t *testing.T) {
		info := &ShardInfo{
			Columns:   []string{"id"},
			Mins:      []interface{}{[]byte("1")},
			Maxs:      []interface{}{[]byte("10")},
			IsNumeric: []bool{true},
		}
		if got := len(GenerateShardRanges(info, 2)); got != 2 {
			t.Errorf("shards = %d, want 2", got)
		}
	})

	t.Run("composite nullable key refuses to shard", func(t *testing.T) {
		info := &ShardInfo{
			Columns:   []string{"a", "b"},
			Mins:      []interface{}{[]byte("1"), []byte("1")},
			Maxs:      []interface{}{[]byte("10"), []byte("10")},
			IsNumeric: []bool{true, true},
			Nullable:  []bool{false, true},
		}
		if ranges := GenerateShardRanges(info, 4); len(ranges) != 0 {
			t.Errorf("want no ranges (single-threaded fallback), got %d", len(ranges))
		}
	})
}
