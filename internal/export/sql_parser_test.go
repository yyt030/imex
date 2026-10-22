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
