package export

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imex/internal/config"
)

// streamTestExporter builds the minimal Exporter writeShardStream needs: no
// database, because every case here is a data-only export (withCreate=false,
// which is also what the CLI does unless --create is given).
func streamTestExporter(t *testing.T, format string, withCreate, noData, noColNames bool) *Exporter {
	t.Helper()

	cfg := config.NewExportConfig()
	cfg.Output = filepath.Join(t.TempDir(), "out."+format)
	cfg.Format = format
	cfg.Query = "SELECT * FROM users"
	cfg.Create = withCreate
	cfg.NoData = noData
	cfg.DelModifier.NoColNames = noColNames

	return &Exporter{config: cfg, sqlParser: NewSQLParser()}
}

// filledPipe returns a closed pipe carrying content, split into blocks of the
// given size the way a real shard writer would deliver it.
func filledPipe(t *testing.T, content string, blockSize int) *shardPipe {
	t.Helper()

	p := newShardPipe(context.Background(), 64, blockSize)
	for i := 0; i < len(content); i += blockSize {
		end := i + blockSize
		if end > len(content) {
			end = len(content)
		}
		p.ch <- []byte(content[i:end])
	}
	close(p.ch)
	return p
}

func readOut(t *testing.T, path string) string {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	return string(b)
}

// TestWriteShardStreamSQLHasNoTableLocks pins the preamble of a parallel SQL
// dump: the charset header leads, both shard bodies follow in order, and there
// is no LOCK TABLES/UNLOCK TABLES pair anywhere. The pair used to be written by
// this merge step (and suppressed per shard); it is gone because it made the
// dump unreplayable into an empty schema.
func TestWriteShardStreamSQLHasNoTableLocks(t *testing.T) {
	e := streamTestExporter(t, "sql", false, false, false)

	pipes := []*shardPipe{
		filledPipe(t, "INSERT INTO `users` (`id`) VALUES (1);\n", 1<<20),
		filledPipe(t, "INSERT INTO `users` (`id`) VALUES (2);\n", 1<<20),
	}
	if err := e.writeShardStream(pipes, []string{"id"}); err != nil {
		t.Fatalf("writeShardStream: %v", err)
	}

	out := readOut(t, e.config.Output)
	if strings.Contains(out, "LOCK TABLES") || strings.Contains(out, "UNLOCK TABLES") {
		t.Errorf("dump must not contain lock statements:\n%s", out)
	}
	if got := strings.Count(out, "SET NAMES"); got != 1 {
		t.Errorf("expected exactly one SET NAMES header, got %d in:\n%s", got, out)
	}

	// The charset header still leads, and the shards stay in order behind it.
	header := strings.Index(out, "SET NAMES")
	firstRow := strings.Index(out, "VALUES (1)")
	secondRow := strings.Index(out, "VALUES (2)")
	if !(header >= 0 && header < firstRow && firstRow < secondRow) {
		t.Errorf("unexpected statement order in:\n%s", out)
	}
}

func TestWriteShardStream_DelHeaderOnlyWhenColumnNamesRequested(t *testing.T) {
	cases := []struct {
		name       string
		noColNames bool
		wantHeader bool
	}{
		{"header written by default", false, true},
		{"header suppressed", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := streamTestExporter(t, "del", false, false, c.noColNames)
			pipes := []*shardPipe{
				filledPipe(t, "1,\"a\"\n", 1<<20),
				filledPipe(t, "2,\"b\"\n", 1<<20),
			}
			if err := e.writeShardStream(pipes, []string{"id", "name"}); err != nil {
				t.Fatalf("writeShardStream: %v", err)
			}
			out := readOut(t, e.config.Output)
			hasHeader := strings.HasPrefix(out, `"id","name"`)
			if hasHeader != c.wantHeader {
				t.Errorf("column header present = %v, want %v in:\n%s", hasHeader, c.wantHeader, out)
			}
			if !strings.Contains(out, `2,"b"`) {
				t.Errorf("second shard lost:\n%s", out)
			}
		})
	}
}

// A shard split across many blocks must land in the output contiguously and in
// order, which is the property the concatenated temporary files used to give.
func TestWriteShardStream_KeepsBlockBoundariesIntact(t *testing.T) {
	e := streamTestExporter(t, "del", false, false, true)

	// 3 blocks per shard, each shard carrying a marker per block.
	pipeA := filledPipe(t, "AAAA1\nAAAA2\nAAAA3\n", 6)
	pipeB := filledPipe(t, "BBBB1\nBBBB2\nBBBB3\n", 6)
	if err := e.writeShardStream([]*shardPipe{pipeA, pipeB}, []string{"id"}); err != nil {
		t.Fatalf("writeShardStream: %v", err)
	}

	want := "AAAA1\nAAAA2\nAAAA3\nBBBB1\nBBBB2\nBBBB3\n"
	if got := readOut(t, e.config.Output); got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}
