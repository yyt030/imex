package _import

import (
	"strings"
	"testing"

	"github.com/gomydump/internal/config"
)

func TestEffectiveBatchSize(t *testing.T) {
	cases := []struct {
		name     string
		batch    int
		columns  int
		expected int
	}{
		{"no column info keeps the request", 1000, 0, 1000},
		{"negative column info keeps the request", 1000, -3, 1000},
		{"narrow table is untouched", 1000, 8, 1000},
		{"placeholder ceiling clamps", 100000, 3, maxPlaceholders / 3},
		{"exactly at the ceiling is kept", maxPlaceholders / 3, 3, maxPlaceholders / 3},
		{"absurdly wide table still ships one row", 1000, 70000, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := effectiveBatchSize(c.batch, c.columns); got != c.expected {
				t.Fatalf("effectiveBatchSize(%d, %d) = %d, want %d", c.batch, c.columns, got, c.expected)
			}
		})
	}
}

func TestEstimateRowBytes(t *testing.T) {
	// One column contributes its value width plus the framing of the row.
	single := func(v interface{}) int64 { return estimateRowBytes([]interface{}{v}) }

	if got, want := single(nil), int64(4+4); got != want {
		t.Errorf("NULL row = %d, want %d", got, want)
	}
	if got, want := single("abc"), int64(4+3+2); got != want {
		t.Errorf("string row = %d, want %d", got, want)
	}
	if got, want := single([]byte("abc")), int64(4+3+2); got != want {
		t.Errorf("bytes row = %d, want %d", got, want)
	}
	if got, want := single(12345), int64(4+21); got != want {
		t.Errorf("int row = %d, want %d", got, want)
	}
	if got, want := single(int64(1)<<62), int64(4+21); got != want {
		t.Errorf("int64 row = %d, want %d", got, want)
	}
	if got, want := single(3.5), int64(4+25); got != want {
		t.Errorf("float row = %d, want %d", got, want)
	}
	if got, want := single(struct{}{}), int64(4+24); got != want {
		t.Errorf("unknown type row = %d, want %d", got, want)
	}
}

// TestEstimateRowBytes_AccountsForEscaping is the point of the byte cap: a value
// made entirely of bytes the driver has to escape must be counted at its
// escaped width, otherwise a wide row would sail past max_allowed_packet.
func TestEstimateRowBytes_AccountsForEscaping(t *testing.T) {
	payload := strings.Repeat(`'"\`+"\n\r\x00\x1a", 100)
	plain := strings.Repeat("a", len(payload))

	escaped := estimateRowBytes([]interface{}{[]byte(payload)})
	raw := estimateRowBytes([]interface{}{[]byte(plain)})

	if want := int64(7 * 100); escaped-raw != want {
		t.Fatalf("escaping contributed %d bytes, want %d", escaped-raw, want)
	}
	// A string carrying the same bytes must be measured identically.
	if asStr := estimateRowBytes([]interface{}{payload}); asStr != escaped {
		t.Fatalf("string estimate %d != []byte estimate %d", asStr, escaped)
	}
}

func TestBatchByteLimit(t *testing.T) {
	cases := []struct {
		packet int64
		want   int64
	}{
		{4 << 20, (4 << 20) * 9 / 10},
		{1 << 20, (1 << 20) * 9 / 10},
		{64 << 20, (64 << 20) * 9 / 10},
		{256 << 20, (256 << 20) * 9 / 10},
		{1024, 64 << 10}, // floor: never shrink a batch below one page-ish
		{0, 64 << 10},
		{-1, 64 << 10},
	}
	for _, c := range cases {
		if got := batchByteLimit(c.packet); got != c.want {
			t.Errorf("batchByteLimit(%d) = %d, want %d", c.packet, got, c.want)
		}
	}
}

func TestShouldFlush(t *testing.T) {
	const budget = int64(1000)
	cases := []struct {
		name  string
		rows  int
		batch int
		bytes int64
		want  bool
	}{
		{"empty batch waits", 0, 10, 0, false},
		{"below both limits waits", 9, 10, budget - 1, false},
		{"row limit flushes", 10, 10, 1, true},
		{"byte limit flushes", 2, 10, budget, true},
		// The condition is evaluated after the append, so a single row that is
		// itself larger than the budget still forms a batch instead of
		// accumulating forever.
		{"single oversized row flushes alone", 1, 10, budget + 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldFlush(c.rows, c.batch, c.bytes, budget); got != c.want {
				t.Fatalf("shouldFlush(%d, %d, %d) = %v, want %v", c.rows, c.batch, c.bytes, got, c.want)
			}
		})
	}
}

// TestResolveBatchSizeWithoutConnection covers the nil-`*sql.DB` guard: the
// reader goroutines call this before any query runs, and a unit-test importer
// has no server to ask for max_allowed_packet.
func TestResolveBatchSizeWithoutConnection(t *testing.T) {
	cfg := config.NewImportConfig()
	cfg.BatchSize = 5000
	imp := &Importer{cfg: cfg}

	batchSize, maxBytes := imp.resolveBatchSize(20)
	if want := maxPlaceholders / 20; batchSize != want {
		t.Fatalf("batchSize = %d, want clamp to %d", batchSize, want)
	}
	if want := batchByteLimit(defaultMaxAllowedPacket); maxBytes != want {
		t.Fatalf("maxBytes = %d, want fallback %d", maxBytes, want)
	}

	// Narrow tables keep the configured size, and the memoised budget is stable.
	cfg2 := config.NewImportConfig()
	imp2 := &Importer{cfg: cfg2}
	if size, _ := imp2.resolveBatchSize(3); size != cfg2.BatchSize {
		t.Fatalf("batchSize = %d, want configured %d", size, cfg2.BatchSize)
	}
}

// BenchmarkEstimateRowBytes measures the cost of the byte cap on the hot path:
// the importer already touches every value once, and the cap adds one linear
// scan over string/byte payload widths.
func BenchmarkEstimateRowBytes(b *testing.B) {
	row := []interface{}{
		int64(1), "Alice", nil, []byte("name"), 3.14,
		strings.Repeat("a", 1024),
	}
	for i := 0; i < b.N; i++ {
		_ = estimateRowBytes(row)
	}
}

func BenchmarkEstimateRowBytesWide(b *testing.B) {
	row := make([]interface{}, 0, 256)
	for i := 0; i < 256; i++ {
		row = append(row, strings.Repeat("x", 64))
	}
	b.SetBytes(int64(len(row) * 64))
	for i := 0; i < b.N; i++ {
		_ = estimateRowBytes(row)
	}
}
