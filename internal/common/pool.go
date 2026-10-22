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

// ApplyPoolHardening configures database/sql's connection ageing. It is applied
// to every pool the tool opens (export main, export shards, import main, import
// workers). Limit values are left to the caller: the import worker and the
// export main connection set SetMaxOpenConns separately.
func ApplyPoolHardening(db *sql.DB) {
	db.SetConnMaxLifetime(DefaultConnMaxLifetime)
	db.SetConnMaxIdleTime(DefaultConnMaxIdleTime)
}
