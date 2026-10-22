package export

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gomydump/internal/common"
	"github.com/gomydump/internal/config"
)

// mergeTestExporter builds the minimal Exporter mergeShardFiles needs: no
// database, because every case here runs with --no-create.
func mergeTestExporter(t *testing.T, format string, noCreate, noData, noColNames bool) *Exporter {
	t.Helper()

	cfg := config.NewExportConfig()
	cfg.Output = filepath.Join(t.TempDir(), "merged."+format)
	cfg.Format = format
	cfg.Query = "SELECT * FROM users"
	cfg.NoCreate = noCreate
	cfg.NoData = noData
	cfg.DelModifier.NoColNames = noColNames

	return &Exporter{config: cfg, sqlParser: NewSQLParser()}
}

func writeShard(t *testing.T, dir, name, content string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write shard %s: %v", name, err)
	}
	return path
}

func readOut(t *testing.T, path string) string {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read merged output: %v", err)
	}
	return string(b)
}

func TestMergeShardFiles_SQLWritesOneLockPair(t *testing.T) {
	dir := t.TempDir()
	e := mergeTestExporter(t, "sql", true, false, false)

	shards := []string{
		writeShard(t, dir, "s0", "INSERT INTO `users` (`id`) VALUES (1);\n"),
		writeShard(t, dir, "s1", "INSERT INTO `users` (`id`) VALUES (2);\n"),
	}
	if err := e.mergeShardFiles(shards, []string{"id"}, common.ServerMySQL); err != nil {
		t.Fatalf("mergeShardFiles: %v", err)
	}

	out := readOut(t, e.config.Output)
	if got := strings.Count(out, "LOCK TABLES `"); got != 1 {
		t.Errorf("expected exactly one LOCK TABLES, got %d in:\n%s", got, out)
	}
	if got := strings.Count(out, "UNLOCK TABLES;"); got != 1 {
		t.Errorf("expected exactly one UNLOCK TABLES, got %d in:\n%s", got, out)
	}
	if got := strings.Count(out, "SET NAMES"); got != 1 {
		t.Errorf("expected exactly one SET NAMES header, got %d in:\n%s", got, out)
	}

	// Order matters: preamble, both shards, then the unlock.
	lockIdx := strings.Index(out, "LOCK TABLES")
	firstRow := strings.Index(out, "VALUES (1)")
	secondRow := strings.Index(out, "VALUES (2)")
	unlockIdx := strings.Index(out, "UNLOCK TABLES;")
	if !(lockIdx >= 0 && lockIdx < firstRow && firstRow < secondRow && secondRow < unlockIdx) {
		t.Errorf("unexpected statement order in:\n%s", out)
	}
}

// TestMergeShardFiles_SQLNoCreateStillLocks guards the previous behaviour, where
// --no-create dropped LOCK TABLES but still emitted UNLOCK TABLES.
func TestMergeShardFiles_SQLNoCreateStillLocks(t *testing.T) {
	dir := t.TempDir()
	e := mergeTestExporter(t, "sql", true, false, false)

	shards := []string{writeShard(t, dir, "s0", "INSERT INTO `users` (`id`) VALUES (1);\n")}
	if err := e.mergeShardFiles(shards, []string{"id"}, common.ServerMySQL); err != nil {
		t.Fatalf("mergeShardFiles: %v", err)
	}

	out := readOut(t, e.config.Output)
	if !strings.Contains(out, "LOCK TABLES `users` WRITE;") {
		t.Errorf("missing LOCK TABLES pair:\n%s", out)
	}
	if !strings.Contains(out, "UNLOCK TABLES;") {
		t.Errorf("missing UNLOCK TABLES:\n%s", out)
	}
}

func TestMergeShardFiles_OceanBaseSkipsLocks(t *testing.T) {
	dir := t.TempDir()
	e := mergeTestExporter(t, "sql", true, false, false)

	shards := []string{writeShard(t, dir, "s0", "INSERT INTO `users` (`id`) VALUES (1);\n")}
	if err := e.mergeShardFiles(shards, []string{"id"}, common.ServerOceanBase); err != nil {
		t.Fatalf("mergeShardFiles: %v", err)
	}

	out := readOut(t, e.config.Output)
	if strings.Contains(out, "LOCK TABLES `") || strings.Contains(out, "UNLOCK TABLES;") {
		t.Errorf("OceanBase dump must not lock tables:\n%s", out)
	}
	if !strings.Contains(out, "SET NAMES") {
		t.Errorf("charset header missing:\n%s", out)
	}
}

func TestMergeShardFiles_DelHeaderOnlyWhenColumnNamesRequested(t *testing.T) {
	dir := t.TempDir()

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
			e := mergeTestExporter(t, "del", true, false, c.noColNames)
			shards := []string{
				writeShard(t, dir, "d0", "1,\"a\"\n"),
				writeShard(t, dir, "d1", "2,\"b\"\n"),
			}
			if err := e.mergeShardFiles(shards, []string{"id", "name"}, common.ServerMySQL); err != nil {
				t.Fatalf("mergeShardFiles: %v", err)
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
