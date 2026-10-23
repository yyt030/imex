package _import

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/gomydump/internal/common"
	"github.com/gomydump/internal/config"
)

// stubResult stands in for the driver's per-batch affected-rows report.
type stubResult struct {
	affected int64
	err      error
}

func (r stubResult) LastInsertId() (int64, error) { return 0, nil }
func (r stubResult) RowsAffected() (int64, error) { return r.affected, r.err }

// TestAddBatchResultCounters locks down the inserted/updated/unchanged split.
// The affected values are the ones measured against a real MariaDB (see the
// table in addBatchResult's comment): 1 per new row, 2 per row whose stored
// values changed, 0 per row written identically.
func TestAddBatchResultCounters(t *testing.T) {
	cases := []struct {
		name     string
		mode     config.ImportMode
		rows     int64
		affected int64
		wantIn   int64
		wantUpd  int64
		wantUnch int64
	}{
		{"insert fresh table", config.ImportModeInsert, 4, 4, 4, 0, 0},
		{"create mode same as insert", config.ImportModeCreate, 3, 3, 3, 0, 0},
		{"replace empties the table first so every row is new", config.ImportModeReplace, 4, 4, 4, 0, 0},
		{"insert_update all new", config.ImportModeInsertUpdate, 4, 4, 4, 0, 0},
		{"insert_update all changed", config.ImportModeInsertUpdate, 4, 8, 0, 4, 0},
		{"insert_update all identical", config.ImportModeInsertUpdate, 4, 0, 0, 0, 4},
		// affected=2 over 4 rows is genuinely ambiguous (2 new + 2 identical, or
		// 1 changed + 3 identical). The deficit is charged to unchanged and the
		// rest to inserts, so the counters always sum to the batch size.
		{"insert_update deficit charged to unchanged", config.ImportModeInsertUpdate, 4, 2, 2, 0, 2},
		{"insert_update 1 changed 3 new", config.ImportModeInsertUpdate, 4, 5, 3, 1, 0},
		{"insert_update single identical row", config.ImportModeInsertUpdate, 1, 0, 0, 0, 1},
		{"insert_update single changed row", config.ImportModeInsertUpdate, 1, 2, 0, 1, 0},
		{"affected beyond the theoretical maximum is clamped", config.ImportModeInsertUpdate, 2, 9, 0, 2, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			imp := &Importer{cfg: &config.ImportConfig{Mode: tc.mode}, stats: common.NewStats()}
			imp.addBatchResult(tc.rows, stubResult{affected: tc.affected})

			got := imp.Stats()
			if got.Inserted != tc.wantIn || got.Updated != tc.wantUpd || got.Unchanged != tc.wantUnch {
				t.Errorf("inserted=%d updated=%d unchanged=%d, want %d/%d/%d",
					got.Inserted, got.Updated, got.Unchanged, tc.wantIn, tc.wantUpd, tc.wantUnch)
			}
			if sum := got.Inserted + got.Updated + got.Unchanged; sum != tc.rows {
				t.Errorf("counters sum to %d, want rows=%d", sum, tc.rows)
			}
		})
	}
}

// TestAddBatchResultUnreadableAffected covers a driver that reports no count:
// the batch is still counted so read/inserted stay reconcilable.
func TestAddBatchResultUnreadableAffected(t *testing.T) {
	for name, res := range map[string]sql.Result{
		"error":    stubResult{affected: 0, err: errors.New("no count")},
		"negative": stubResult{affected: -1},
	} {
		t.Run(name, func(t *testing.T) {
			imp := &Importer{cfg: &config.ImportConfig{Mode: config.ImportModeInsertUpdate}, stats: common.NewStats()}
			imp.addBatchResult(5, res)
			got := imp.Stats()
			if got.Inserted != 5 || got.Updated != 0 || got.Unchanged != 0 {
				t.Errorf("inserted=%d updated=%d unchanged=%d, want 5/0/0", got.Inserted, got.Updated, got.Unchanged)
			}
		})
	}
}

// TestAddBatchResultAccumulatesAcrossBatches mirrors a real run: several
// batches of one import add up, which is also how parallel workers converge on
// the shared counters.
func TestAddBatchResultAccumulatesAcrossBatches(t *testing.T) {
	imp := &Importer{cfg: &config.ImportConfig{Mode: config.ImportModeInsertUpdate}, stats: common.NewStats()}

	// batch 1: 2 new rows + 1 unchanged of 3
	imp.addBatchResult(3, stubResult{affected: 2})
	// batch 2: 3 changed rows
	imp.addBatchResult(3, stubResult{affected: 6})
	// batch 3: 2 identical rows
	imp.addBatchResult(2, stubResult{affected: 0})

	got := imp.Stats()
	if got.Inserted != 2 || got.Updated != 3 || got.Unchanged != 3 {
		t.Fatalf("inserted=%d updated=%d unchanged=%d, want 2/3/3", got.Inserted, got.Updated, got.Unchanged)
	}
	if sum := got.Inserted + got.Updated + got.Unchanged; sum != 8 {
		t.Errorf("counters sum to %d, want 8 rows", sum)
	}
}

// TestStatsBeforeImportIsZero guards the accessor used by integration tests:
// reading counters before Import() allocated them must not panic.
func TestStatsBeforeImportIsZero(t *testing.T) {
	imp := &Importer{cfg: &config.ImportConfig{Mode: config.ImportModeInsert}}
	if got := imp.Stats(); got != (common.StatsSnapshot{}) {
		t.Errorf("Stats() before import = %+v, want zero snapshot", got)
	}
}
