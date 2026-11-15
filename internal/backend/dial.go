package backend

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

// addrCursor is the round-robin position shared by every connection this
// process opens. Each OpenWithFailover call advances it, so the main connection
// takes endpoint 0, the first shard/parallel worker endpoint 1, and so on,
// wrapping around. It is deliberately process-global rather than per-Config:
// export and import hand the same Config to every worker, and a per-Config
// counter would either race or pin all workers to one endpoint.
var addrCursor atomic.Uint64

type dialOpenFunc func(*config.Config) (*sql.DB, error)
type dialPingFunc func(context.Context, *sql.DB) error

// OpenWithFailover is the single dial entry point for a Backend.
//
// A Config with no resolved address list (the single-address case, which is
// every run that does not use the new comma syntax) goes straight to be.Open:
// not one byte of the DSN, the config record, the error text or the issued SQL
// changes on that path, and PingWithRetry stays the caller's decision.
//
// With several addresses, each call picks a round-robin starting endpoint and
// tries the whole list from there: a per-address Config copy is handed to the
// ordinary engine Open (so no dialect had to learn a second address shape),
// then a bounded PingContext decides whether the endpoint is usable. A dial or
// login failure moves to the next endpoint; the first success returns the DB.
// A rejected ALTER SESSION SET is a configuration error rather than a dead node,
// so it stops the loop immediately with the option that failed (see
// damengSessionVarError). If all of them fail, the error names every endpoint's
// failure so the operator can see which node to fix.
//
// The verification ping is a single attempt, not PingWithRetry: failover is
// itself the retry, and waiting out the shared backoff on a dead node before
// trying the next one would defeat the point. The caller's own PingWithRetry
// still runs afterwards on the returned DB, exactly as it did before, so a
// transient blip on the chosen endpoint keeps its original retry behaviour.
func OpenWithFailover(be Backend, cfg *config.Config) (*sql.DB, error) {
	if len(cfg.Addrs) <= 1 {
		return be.Open(cfg)
	}
	return openWithFailover(cfg,
		func(c *config.Config) (*sql.DB, error) { return be.Open(c) },
		func(ctx context.Context, db *sql.DB) error { return db.PingContext(ctx) },
	)
}

// openWithFailover holds the loop behind OpenWithFailover with its two I/O
// seams injected, so the round-robin order and the failover policy can be
// asserted without a live server. The production call above supplies the real
// engine Open and a bounded PingContext; nothing else calls this function.
func openWithFailover(cfg *config.Config, open dialOpenFunc, ping dialPingFunc) (*sql.DB, error) {
	addrs := cfg.Addrs
	n := len(addrs)
	start := int((addrCursor.Add(1) - 1) % uint64(n))
	timeout := cfg.ConnectTimeout
	if timeout <= 0 {
		timeout = config.DefaultConnectTimeout
	}

	failures := make([]string, 0, n)
	for i := 0; i < n; i++ {
		a := addrs[(start+i)%n]
		one := *cfg
		one.Host, one.Port = a.Host, a.Port
		one.Addrs, one.PortSpec = nil, ""

		db, err := open(&one)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s:%d: %v", a.Host, a.Port, err))
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		pingErr := ping(ctx, db)
		cancel()
		if pingErr == nil {
			common.Logger.Info("connection endpoint selected",
				"host", a.Host, "port", a.Port, "attempt", i+1, "of", n)
			return db, nil
		}
		db.Close()
		// A rejected ALTER SESSION SET is a bad --session-var, not a dead node.
		// Every endpoint would reject the same name/value, so walking the rest
		// of the list would only produce N copies of the error and hide it
		// behind "all N addresses failed". Report the option that failed and
		// stop here; the single-address path never reaches this loop.
		var svErr *damengSessionVarError
		if errors.As(pingErr, &svErr) {
			return nil, svErr
		}
		common.Logger.Warn("connection endpoint unavailable, trying next",
			"host", a.Host, "port", a.Port, "error", pingErr)
		failures = append(failures, fmt.Sprintf("%s:%d: %v", a.Host, a.Port, pingErr))
	}
	return nil, fmt.Errorf("all %d addresses failed: %s", n, strings.Join(failures, "; "))
}
