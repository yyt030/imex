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

// splitGeneratedColumns separates cols into the INSERT column list and the row
// positions that feed it. The projection is what keeps every parsed row and the
// column list the same width: a row carries one value per input column, while
// the INSERT list is shorter once the computed columns are gone. A nil
// projection means nothing was dropped and rows are still appended whole.
func splitGeneratedColumns(cols []string, projection []int, generated map[string]bool) ([]string, []int) {
	if len(generated) == 0 {
		return cols, projection
	}
	kept := make([]string, 0, len(cols))
	keptProjection := make([]int, 0, len(cols))
	dropped := false
	for i, col := range cols {
		src := i
		if projection != nil {
			src = projection[i]
		}
		if generated[strings.ToUpper(strings.TrimSpace(col))] {
			dropped = true
			continue
		}
		kept = append(kept, col)
		keptProjection = append(keptProjection, src)
	}
	if !dropped {
		return cols, projection
	}
	return kept, keptProjection
}

// appendRowValues appends one parsed row to dst through projection: INSERT
// column i takes row[projection[i]]. A nil projection means the row already is
// the INSERT value list, and the whole row goes in with one append — the fast
// path for every import that writes all columns in file order.
func appendRowValues(dst, row []interface{}, projection []int) []interface{} {
	if projection == nil {
		return append(dst, row...)
	}
	for _, idx := range projection {
		if idx < len(row) {
			dst = append(dst, row[idx])
		}
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
func (imp *Importer) excludeGeneratedColumns(ctx context.Context, cols []string, projection []int) ([]string, []int, error) {
	generated, err := imp.generatedColumns(ctx, cols)
	if err != nil {
		common.Logger.Warn("could not list generated columns for the target table, the INSERT will include them", "error", err)
		return cols, projection, nil
	}
	kept, keptProjection := splitGeneratedColumns(cols, projection, generated)
	if len(kept) == len(cols) {
		return cols, projection, nil
	}
	if len(kept) == 0 {
		return nil, nil, errors.Errorf("every target column of table %q is a generated column: the server computes all of them from other columns, so there is nothing to insert — import the base columns instead", imp.cfg.Table)
	}
	names := make([]string, 0, len(cols)-len(kept))
	for _, col := range cols {
		if generated[strings.ToUpper(strings.TrimSpace(col))] {
			names = append(names, strings.TrimSpace(col))
		}
	}
	common.Logger.Info("generated columns excluded from INSERT",
		"table", imp.cfg.Table,
		"columns", names,
		"note", "the server recomputes generated columns from their expression",
	)
	return kept, keptProjection, nil
}
