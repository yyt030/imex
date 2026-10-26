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

func TestSQLParser_SimpleTableName(t *testing.T) {
	p := NewSQLParser()

	t.Run("accepts single-table queries", func(t *testing.T) {
		cases := map[string]string{
			"SELECT * FROM users": "users",
			// A schema-qualified query must yield the bare table name: the dump
			// stamps this string into CREATE/INSERT/LOCK and matches it against
			// information_schema, and carrying "db.users" through is what made the
			// connection's own database get prefixed twice (Error 1146). The
			// qualifier is retrieved separately via ExtractTableRef.
			"SELECT id FROM `db`.`users`":                "users",
			"SELECT * FROM users u WHERE u.id = 1":       "users",
			"SELECT * FROM users WHERE a > 1 ORDER BY a": "users",
		}
		for query, want := range cases {
			got, err := p.SimpleTableName(query)
			if err != nil {
				t.Errorf("SimpleTableName(%q) error = %v", query, err)
				continue
			}
			if got != want {
				t.Errorf("SimpleTableName(%q) = %q, want %q", query, got, want)
			}
		}
		// A FROM-less SELECT parses against the implicit DUAL table.
		if got, err := p.SimpleTableName("SELECT 1"); err != nil || got != "dual" {
			t.Errorf("SimpleTableName(%q) = %q, %v, want %q, nil", "SELECT 1", got, err, "dual")
		}
	})

	t.Run("rejects non single-table queries", func(t *testing.T) {
		queries := []string{
			"SELECT * FROM a JOIN b ON a.id = b.id",
			"SELECT * FROM a LEFT JOIN b ON a.id = b.id",
			"SELECT * FROM a, b",
			"SELECT * FROM (SELECT * FROM users) t",
			"WITH c AS (SELECT 1) SELECT * FROM c",
			"UPDATE users SET a = 1",
		}
		for _, query := range queries {
			name, err := p.SimpleTableName(query)
			if err == nil {
				t.Errorf("SimpleTableName(%q) = %q, want error", query, name)
				continue
			}
			if !strings.Contains(err.Error(), "--format del") {
				t.Errorf("SimpleTableName(%q) error = %v, want alternative format hint", query, err)
			}
		}
	})
}

// TestSQLParser_ExtractTableRef pins the schema/name split used to look a table
// up for SHOW CREATE without re-qualifying the emitted dump.
func TestSQLParser_ExtractTableRef(t *testing.T) {
	p := NewSQLParser()
	cases := []struct {
		query      string
		wantSchema string
		wantName   string
	}{
		{"SELECT * FROM users", "", "users"},
		{"SELECT id FROM `db`.`users`", "db", "users"},
		{"SELECT * FROM mysqldump_test.t AS x", "mysqldump_test", "t"},
	}
	for _, tc := range cases {
		schema, name, err := p.ExtractTableRef(tc.query)
		if err != nil {
			t.Errorf("ExtractTableRef(%q) error = %v", tc.query, err)
			continue
		}
		if schema != tc.wantSchema || name != tc.wantName {
			t.Errorf("ExtractTableRef(%q) = (%q,%q), want (%q,%q)", tc.query, schema, name, tc.wantSchema, tc.wantName)
		}
	}
	for _, q := range []string{"UPDATE users SET a = 1", "DELETE FROM users", "INSERT INTO users VALUES (1)"} {
		if _, _, err := p.ExtractTableRef(q); err == nil {
			t.Errorf("ExtractTableRef(%q) = nil error, want failure", q)
		}
	}
}

func TestBuildShardedQuery_NullShard(t *testing.T) {
	p := NewSQLParser()
	info := &ShardInfo{Columns: []string{"k"}, IsNumeric: []bool{true}}
	q, err := p.BuildShardedQuery("SELECT * FROM t", info, ShardRange{NullOnly: true})
	if err != nil {
		t.Fatalf("BuildShardedQuery() error = %v", err)
	}
	if !strings.Contains(strings.ToLower(q), "is null") {
		t.Errorf("query = %q, want an IS NULL condition", q)
	}
	if strings.Contains(q, ">=") || strings.Contains(q, "<") {
		t.Errorf("query = %q, want no range comparison for the NULL shard", q)
	}
}
