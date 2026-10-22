package export

import (
	"strings"
	"testing"
)

func TestSplitLeadingOptimizerHint(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		wantHint string
		wantRest string
	}{
		{
			name:     "leading hint before select",
			query:    "/*+ parallel(8) */ SELECT * FROM t",
			wantHint: "/*+ parallel(8) */",
			wantRest: " SELECT * FROM t",
		},
		{
			name:     "leading hint with whitespace",
			query:    "   /*+ QUERY_TIMEOUT(3000000000) */ select 1",
			wantHint: "/*+ QUERY_TIMEOUT(3000000000) */",
			wantRest: " select 1",
		},
		{
			name:     "hint after select keyword is not leading",
			query:    "SELECT /*+ parallel(8) */ * FROM t",
			wantHint: "",
			wantRest: "SELECT /*+ parallel(8) */ * FROM t",
		},
		{
			name:     "no hint",
			query:    "SELECT * FROM t",
			wantHint: "",
			wantRest: "SELECT * FROM t",
		},
		{
			name:     "unterminated hint block",
			query:    "/*+ parallel(8) SELECT * FROM t",
			wantHint: "",
			wantRest: "/*+ parallel(8) SELECT * FROM t",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hint, rest := splitLeadingOptimizerHint(tt.query)
			if hint != tt.wantHint {
				t.Errorf("hint = %q, want %q", hint, tt.wantHint)
			}
			if rest != tt.wantRest {
				t.Errorf("rest = %q, want %q", rest, tt.wantRest)
			}
		})
	}
}

func TestBuildShardedQuery_PreservesLeadingHint(t *testing.T) {
	p := NewSQLParser()
	shard := &ShardInfo{Columns: []string{"id"}}
	rng := ShardRange{
		Mins:   []interface{}{int64(0)},
		Maxs:   []interface{}{int64(100)},
		IsLast: true,
	}

	q := "/*+ parallel(8) */ SELECT * FROM t"
	out, err := p.BuildShardedQuery(q, shard, rng)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(out, "/*+ parallel(8) */") {
		t.Errorf("leading hint dropped: %q", out)
	}
	if !strings.Contains(out, "t.id >= 0") || !strings.Contains(out, "t.id <= 100") {
		t.Errorf("shard condition not injected: %q", out)
	}
}

func TestBuildShardedQuery_PreservesInlineHint(t *testing.T) {
	p := NewSQLParser()
	shard := &ShardInfo{Columns: []string{"id"}}
	rng := ShardRange{Mins: []interface{}{int64(0)}, Maxs: []interface{}{int64(100)}, IsLast: true}

	q := "SELECT /*+ QUERY_TIMEOUT(3000000000) */ * FROM t"
	out, err := p.BuildShardedQuery(q, shard, rng)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "/*+ QUERY_TIMEOUT(3000000000) */") {
		t.Errorf("inline hint dropped: %q", out)
	}
}

// Exact BIGINT boundaries must be emitted as integer literals, not float
// approximations: `%f` formatting would turn 9007199254740999 into
// 9007199254740998.000000 and silently narrow the shard.
func TestBuildShardedQuery_ExactBigIntBoundary(t *testing.T) {
	p := NewSQLParser()
	shard := &ShardInfo{Columns: []string{"id"}}
	rng := ShardRange{
		Mins:   []interface{}{shardBoundary("9007199254740993")},
		Maxs:   []interface{}{shardBoundary("9007199254740999")},
		IsLast: true,
	}

	out, err := p.BuildShardedQuery("SELECT * FROM t", shard, rng)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "9007199254740993") || !strings.Contains(out, "9007199254740999") {
		t.Fatalf("boundaries were not preserved exactly: %q", out)
	}
	if strings.Contains(out, "e+") || strings.Contains(out, ".000000") {
		t.Fatalf("boundary rendered as a float: %q", out)
	}
}

func TestBuildShardedQuery_DecimalBoundaryStaysNumeric(t *testing.T) {
	p := NewSQLParser()
	shard := &ShardInfo{Columns: []string{"price"}}
	rng := ShardRange{
		Mins: []interface{}{shardBoundary("0.1")},
		Maxs: []interface{}{shardBoundary("0.366666666666666666666666666667")},
	}

	out, err := p.BuildShardedQuery("SELECT * FROM t", shard, rng)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "0.366666666666666666666666666667") {
		t.Fatalf("decimal boundary missing: %q", out)
	}
	if strings.Contains(out, "'0.366") {
		t.Fatalf("decimal boundary was quoted as a string: %q", out)
	}
}
