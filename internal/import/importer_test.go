package _import

import (
	"reflect"
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
	// The server would reject the INSERT with Error 1110 only after rows had
	// been read, so the repeated target is refused up front.
	if err := validatePositionalColumns([]string{"a", "A"}, []string{"x", "y"}); err == nil {
		t.Error("repeated target column accepted")
	} else if !strings.Contains(err.Error(), "twice") || !strings.Contains(err.Error(), "Error 1110") {
		t.Errorf("error = %v, want a duplicate-column complaint citing Error 1110", err)
	}
}

// TestApplyColumnMapping pins the name-based --columns form: only the written
// mappings must be valid, so a mapping may name a subset of the file's columns
// in any order, while every entry it does name is checked against the header.
func TestApplyColumnMapping(t *testing.T) {
	newImp := func(columns string) *Importer {
		cfg := config.NewImportConfig()
		cfg.Table = "t"
		cfg.Columns = columns
		return &Importer{cfg: cfg}
	}

	ok := []struct {
		name       string
		columns    string
		header     []string
		wantCols   []string
		wantProj   []int
		wantReader []string
	}{
		{
			name:       "a subset of the file columns is enough",
			columns:    "name=t_name",
			header:     []string{"id", "name", "age"},
			wantCols:   []string{"t_name"},
			wantProj:   []int{1},
			wantReader: []string{"", "t_name", ""},
		},
		{
			name:       "all columns mapped in file order take the fast path",
			columns:    "name=t_name,id=t_id",
			header:     []string{"id", "name"},
			wantCols:   []string{"t_id", "t_name"},
			wantProj:   nil,
			wantReader: []string{"t_id", "t_name"},
		},
		{
			name:       "a subset in a different order keeps the header order",
			columns:    "age=a_years,id=t_id",
			header:     []string{"id", "name", "age"},
			wantCols:   []string{"t_id", "a_years"},
			wantProj:   []int{0, 2},
			wantReader: []string{"t_id", "", "a_years"},
		},
		{
			name:       "names are matched case-insensitively and trimmed",
			columns:    " ID = t_id ",
			header:     []string{"id", "Name"},
			wantCols:   []string{"t_id"},
			wantProj:   []int{0},
			wantReader: []string{"t_id", ""},
		},
	}
	for _, c := range ok {
		t.Run(c.name, func(t *testing.T) {
			cm, err := newImp(c.columns).applyColumnMapping(c.header)
			if err != nil {
				t.Fatalf("applyColumnMapping: %v", err)
			}
			if !reflect.DeepEqual(cm.Columns, c.wantCols) {
				t.Errorf("Columns = %v, want %v", cm.Columns, c.wantCols)
			}
			if !reflect.DeepEqual(cm.Projection, c.wantProj) {
				t.Errorf("Projection = %v, want %v", cm.Projection, c.wantProj)
			}
			if !reflect.DeepEqual(cm.ReaderColumns, c.wantReader) {
				t.Errorf("ReaderColumns = %v, want %v", cm.ReaderColumns, c.wantReader)
			}
		})
	}

	bad := []struct {
		name    string
		columns string
		header  []string
		want    string
	}{
		{"file column missing from the header", "nope=x", []string{"id", "name"}, "not in the input header"},
		{"header columns are named in the error", "nope=x", []string{"id", "name"}, "id, name"},
		{"one file column mapped twice", "id=a,ID=b", []string{"id"}, "mapped twice"},
		{"two file columns into one target", "id=a,name=A", []string{"id", "name"}, "Error 1110"},
		{"empty target side", "id=", []string{"id"}, "both file_col and table_col are required"},
		{"entry without =", "id", []string{"id"}, "expected file_col=table_col"},
		{"no header to match names against", "id=a", nil, "no column header"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			_, err := newImp(c.columns).applyColumnMapping(c.header)
			if err == nil {
				t.Fatalf("applyColumnMapping(%q) accepted", c.columns)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to contain %q", err, c.want)
			}
		})
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
