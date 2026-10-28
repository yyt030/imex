package common

import (
	"context"
	"database/sql"
	"strings"

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

// ServerType represents the type of database server.
type ServerType int

const (
	ServerMySQL ServerType = iota
	ServerOceanBase
)

// String returns the string representation of the server type.
func (s ServerType) String() string {
	switch s {
	case ServerOceanBase:
		return "OceanBase"
	default:
		return "MySQL"
	}
}

// DetectServerType detects the database server type by querying version_comment.
func DetectServerType(db *sql.DB) (ServerType, error) {
	var varName, versionComment string
	detectSQL := "SHOW VARIABLES LIKE 'version_comment'"
	LogInteractionSQL(detectSQL)
	err := db.QueryRow(detectSQL).Scan(&varName, &versionComment)
	if err != nil {
		// If we can't determine the server type, assume MySQL
		return ServerMySQL, nil
	}

	if strings.Contains(strings.ToLower(versionComment), "oceanbase") {
		return ServerOceanBase, nil
	}
	return ServerMySQL, nil
}
