package _import

import (
	"context"
	"strings"

	"github.com/imex/internal/common"
	"github.com/pkg/errors"
)

// generatedColumnQuery lists the generated (computed) columns of the target
// table. EXTRA is the only portable marker: MySQL 5.7/8.0 report
// "VIRTUAL GENERATED" / "STORED GENERATED", MariaDB 10.2+ uses the same words,
// and OceanBase in MySQL mode follows the MySQL spelling. GENERATION_EXPRESSION
// is deliberately not selected: a server that lacks that column would fail the
// whole query, turning a nicety into an aborted import.
const generatedColumnQuery = `SELECT UPPER(COLUMN_NAME)
FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND EXTRA LIKE '%GENERATED%'`

// generatedColumns returns the generated columns of the target table among cols.
// The table always exists when this runs: every import mode that reaches the
// data stage has either found the table or created it. A nil map means the table
// has no generated column in this file.
func (imp *Importer) generatedColumns(ctx context.Context, cols []string) (map[string]bool, error) {
	if imp.db == nil || imp.cfg.Table == "" || len(cols) == 0 {
		return nil, nil
	}
	common.LogInteractionSQL(generatedColumnQuery, imp.cfg.Database, imp.cfg.Table)
	rows, err := imp.db.QueryContext(ctx,
		common.TagSQL(generatedColumnQuery),
		imp.cfg.Database, imp.cfg.Table)
	if err != nil {
		return nil, errors.Wrap(err, "failed to query generated columns")
	}
	defer rows.Close()

	generated := make(map[string]bool, 2)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, errors.Wrap(err, "failed to scan generated column")
		}
		generated[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Wrap(err, "generated column iteration error")
	}
	return generated, nil
}

// splitGeneratedColumns separates cols into the INSERT column list and a drop
// mask over the original positions. The mask is what keeps every parsed row and
// the column list the same width: a row carries one value per input column,
// while the INSERT list is shorter once the computed columns are gone. A nil
// mask means nothing was dropped, and callers then take the unmodified row.
func splitGeneratedColumns(cols []string, generated map[string]bool) ([]string, []bool) {
	if len(generated) == 0 {
		return cols, nil
	}
	kept := make([]string, 0, len(cols))
	drop := make([]bool, len(cols))
	dropped := false
	for i, col := range cols {
		if generated[strings.ToUpper(strings.TrimSpace(col))] {
			drop[i] = true
			dropped = true
			continue
		}
		kept = append(kept, col)
	}
	if !dropped {
		return cols, nil
	}
	return kept, drop
}

// appendRowValues appends one parsed row to dst, leaving out the positions the
// drop mask marks. The unmodified fast path (no generated column in the file)
// is the same single append as before, so imports that never touch a computed
// column pay nothing for this.
func appendRowValues(dst, row []interface{}, drop []bool) []interface{} {
	if len(drop) == 0 {
		return append(dst, row...)
	}
	for i, v := range row {
		if i < len(drop) && drop[i] {
			continue
		}
		dst = append(dst, v)
	}
	return dst
}

// excludeGeneratedColumns rewrites the INSERT column list so computed columns
// stay out of it. Writing a value for a generated column is rejected outright
// under strict mode (Error 1906: "The value specified for generated column ...
// has been ignored"), which failed every import of such a table — including a
// --create round trip of imex's own dump, and mysqldump's output on the same
// server. The server recomputes the column from its expression, so the value
// the export carried is redundant rather than lost.
//
// A failure to read information_schema is reported and tolerated: the import
// then behaves exactly as before (the server's own error still names the
// column), which is better than refusing a run that may be perfectly fine on a
// table without generated columns.
func (imp *Importer) excludeGeneratedColumns(ctx context.Context, cols []string) ([]string, []bool, error) {
	generated, err := imp.generatedColumns(ctx, cols)
	if err != nil {
		common.Logger.Warn("could not list generated columns for the target table, the INSERT will include them", "error", err)
		return cols, nil, nil
	}
	kept, drop := splitGeneratedColumns(cols, generated)
	if drop == nil {
		return cols, nil, nil
	}
	if len(kept) == 0 {
		return nil, nil, errors.Errorf("every target column of table %q is a generated column: the server computes all of them from other columns, so there is nothing to insert — import the base columns instead", imp.cfg.Table)
	}
	names := make([]string, 0, len(drop))
	for i, col := range cols {
		if drop[i] {
			names = append(names, strings.TrimSpace(col))
		}
	}
	common.Logger.Info("generated columns excluded from INSERT",
		"table", imp.cfg.Table,
		"columns", names,
		"note", "the server recomputes generated columns from their expression",
	)
	return kept, drop, nil
}
