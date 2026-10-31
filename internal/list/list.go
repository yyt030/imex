package list

import (
	"context"
	"database/sql"
	"io"

	"github.com/pkg/errors"

	"github.com/imex/internal/config"
)

// Run executes one list query and writes its rows to out. It returns the number
// of data rows written (not counting the header).
//
// The query runs before the renderer is created, so a usage or SQL error never
// produces a header for rows that do not exist. The table renderer buffers all
// rows and writes once, so a failed run leaves no partial table behind.
func Run(ctx context.Context, db *sql.DB, cfg *config.ListConfig, out io.Writer) (int, error) {
	query, args, columns, err := BuildQuery(cfg)
	if err != nil {
		return 0, err
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, errors.Wrap(err, "metadata query failed")
	}
	defer rows.Close()

	renderer := newRenderer(columns, !cfg.NoColNames, out)

	count := 0
	for rows.Next() {
		values := make([]sql.NullString, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return count, errors.Wrap(err, "failed to read metadata row")
		}
		row := make([]*string, len(columns))
		for i := range values {
			if values[i].Valid {
				value := values[i].String
				row[i] = &value
			}
		}
		if err := renderer.WriteRow(row); err != nil {
			return count, errors.Wrap(err, "failed to write list output")
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return count, errors.Wrap(err, "metadata query failed")
	}
	if err := renderer.Close(); err != nil {
		return count, errors.Wrap(err, "failed to write list output")
	}
	return count, nil
}
