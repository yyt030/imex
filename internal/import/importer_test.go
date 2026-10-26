package _import

import (
	"strings"
	"testing"

	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

func TestValidatePositionalColumns(t *testing.T) {
	if err := validatePositionalColumns([]string{"a", "b"}, []string{"x", "y"}); err != nil {
		t.Errorf("matching counts: err = %v", err)
	}
	// Header unknown (--no-col-names): only the names are checked.
	if err := validatePositionalColumns([]string{"a", "b"}, nil); err != nil {
		t.Errorf("no file header: err = %v", err)
	}
	if err := validatePositionalColumns([]string{"a", ""}, []string{"x", "y"}); err == nil {
		t.Error("empty entry accepted")
	} else if !strings.Contains(err.Error(), "entry 2") {
		t.Errorf("error = %v, want entry 2", err)
	}
	if err := validatePositionalColumns([]string{"a"}, []string{"x", "y", "z"}); err == nil {
		t.Error("count mismatch accepted")
	} else if !strings.Contains(err.Error(), "1 target columns") || !strings.Contains(err.Error(), "3 columns") {
		t.Errorf("error = %v, want counts 1 and 3", err)
	}
}

// TestBuildBatchInsertSQLTagged pins the bytes of a built batch statement. The
// attribution comment is written as the first thing into the statement buffer
// rather than concatenated onto the finished text, so this asserts the result is
// exactly what common.TagSQL would have produced from the same untagged
// statement — including the mode-specific ON DUPLICATE KEY UPDATE suffix.
func TestBuildBatchInsertSQLTagged(t *testing.T) {
	commit := common.GitCommit
	defer func() {
		common.GitCommit = commit
		common.ResetSQLTag()
	}()

	build := func(mode config.ImportMode) string {
		cfg := config.NewImportConfig()
		cfg.Table = "t"
		cfg.Mode = mode
		imp := &Importer{cfg: cfg}
		return imp.buildBatchInsertSQL([]string{"a", " b "}, 3)
	}

	// A dev build injects no commit, so SQLTag() is empty: this is the untagged
	// reference text.
	common.GitCommit = "unknown"
	common.ResetSQLTag()
	plainInsert := build(config.ImportModeInsert)
	plainUpdate := build(config.ImportModeInsertUpdate)
	if strings.HasPrefix(plainInsert, "/*") {
		t.Fatalf("untagged build carries a comment: %s", plainInsert)
	}

	common.GitCommit = "9183226abcdef"
	common.ResetSQLTag()
	taggedInsert := build(config.ImportModeInsert)
	taggedUpdate := build(config.ImportModeInsertUpdate)

	if want := common.TagSQL(plainInsert); taggedInsert != want {
		t.Errorf("insert statement drifted:\n got %q\nwant %q", taggedInsert, want)
	}
	if want := common.TagSQL(plainUpdate); taggedUpdate != want {
		t.Errorf("insert_update statement drifted:\n got %q\nwant %q", taggedUpdate, want)
	}
	if n := strings.Count(taggedInsert, "?)"); n != 3 {
		t.Errorf("placeholder groups: got %d, want 3 for 3 rows", n)
	}
}

// BenchmarkBuildBatchInsertSQL measures the statement text the import side
// produces per batch, with the attribution tag enabled: the tag is what turned
// one build into two passes over the batch text.
func BenchmarkBuildBatchInsertSQL(b *testing.B) {
	commit := common.GitCommit
	defer func() {
		common.GitCommit = commit
		common.ResetSQLTag()
	}()
	common.GitCommit = "9183226abcdef"
	common.ResetSQLTag()

	cfg := config.NewImportConfig()
	cfg.Table = "bench_narrow"
	cfg.Mode = config.ImportModeInsert
	imp := &Importer{cfg: cfg}
	cols := []string{"id", "grp", "amount", "qty", "code", "name", "note", "flag", "created_at", "updated_at"}

	size := 0
	for i := 0; i < b.N; i++ {
		size = len(imp.buildBatchInsertSQL(cols, 1000))
	}
	b.SetBytes(int64(size))
}
