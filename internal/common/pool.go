package common

import (
	"database/sql"
	"time"
)

const (
	// DefaultConnMaxLifetime recycles pooled connections before common LB/NAT
	// idle reapers (often 5–10 minutes) can silently drop them. A connection
	// that is currently checked out (e.g. inside an open transaction or a
	// consistent-snapshot read) is never closed by this limit; it only affects
	// connections returned to the idle pool.
	DefaultConnMaxLifetime = 5 * time.Minute

	// DefaultConnMaxIdleTime closes connections that sat idle in the pool, so a
	// long import that pauses between tables does not keep a half-dead socket
	// around until the next write discovers it.
	DefaultConnMaxIdleTime = time.Minute
)

// ApplyPoolHardening configures database/sql's connection ageing. It is meant
// for the import pools (main and per-worker). Export pools must NOT use it: they
// open a consistent snapshot with a raw START TRANSACTION, so the snapshot
// connection returns to the idle pool between statements and ageing it would
// silently roll the snapshot back (exporter.go sets both limits to 0).
func ApplyPoolHardening(db *sql.DB) {
	db.SetConnMaxLifetime(DefaultConnMaxLifetime)
	db.SetConnMaxIdleTime(DefaultConnMaxIdleTime)
}
