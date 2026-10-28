package common

import (
	"context"
	"database/sql"

	"github.com/pkg/errors"
)

// MaxConnections reads the server's connection ceiling, which is what decides how
// many parallel workers can hold a connection at the same time. The CLI thread
// limit is a fixed 1..256 and says nothing about the target, and exceeding it is
// not a startup-time error but a runtime one: the workers that cannot connect
// retry a bounded number of times and then fail the whole run, after other
// workers have already committed their batches. Reading it lets the parallel
// paths report the mismatch before any data moves.
func MaxConnections(ctx context.Context, db *sql.DB) (int, error) {
	query := "SELECT @@max_connections"
	LogInteractionSQL(query)
	var value int
	if err := db.QueryRowContext(ctx, TagSQL(query)).Scan(&value); err != nil {
		return 0, errors.Wrap(err, "failed to read max_connections")
	}
	return value, nil
}
