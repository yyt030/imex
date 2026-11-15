package backend

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/imex/internal/config"
)

// lazyTestDB returns a *sql.DB that has never dialled. sql.Open only stores
// the DSN, so Close() is safe on the failover path and no server is needed.
func lazyTestDB() (*sql.DB, error) {
	return sql.Open("mysql", "u:p@tcp(127.0.0.1:1)/")
}

func addrKey(c *config.Config) string {
	return c.Host + ":" + strconv.Itoa(c.Port)
}

func twoAddrConfig() *config.Config {
	return &config.Config{Addrs: []config.Addr{{Host: "1.1.1.1", Port: 3306}, {Host: "2.2.2.2", Port: 3307}}}
}

func TestOpenWithFailoverSingleAddressPassesThrough(t *testing.T) {
	// A single-address Config must never be pinged here: that is the caller's
	// PingWithRetry job, and dialling an unreachable server would fail the test.
	be := Must(EngineMySQL)
	cfg := &config.Config{Engine: "mysql", Host: "127.0.0.1", Port: 3306, DefaultCharset: "utf8mb4"}
	db, err := OpenWithFailover(be, cfg)
	if err != nil {
		t.Fatalf("single address took the failover path: %v", err)
	}
	db.Close()
}

func TestOpenWithFailoverRoundRobin(t *testing.T) {
	addrCursor.Store(0)
	cfg := twoAddrConfig()
	var picked []string
	open := func(c *config.Config) (*sql.DB, error) {
		if c.Addrs != nil || c.PortSpec != "" {
			t.Errorf("the per-address copy still carries the list: %+v", c)
		}
		picked = append(picked, addrKey(c))
		return lazyTestDB()
	}
	ping := func(context.Context, *sql.DB) error { return nil }
	for i := 0; i < 4; i++ {
		db, err := openWithFailover(cfg, open, ping)
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
	want := []string{"1.1.1.1:3306", "2.2.2.2:3307", "1.1.1.1:3306", "2.2.2.2:3307"}
	if strings.Join(picked, ",") != strings.Join(want, ",") {
		t.Fatalf("round-robin order = %v, want %v", picked, want)
	}
	if len(cfg.Addrs) != 2 || cfg.Host != "" {
		t.Errorf("the caller's Config was mutated: %+v", cfg)
	}
}

func TestOpenWithFailoverSkipsUnavailableEndpoint(t *testing.T) {
	addrCursor.Store(0)
	cfg := twoAddrConfig()
	var current string
	open := func(c *config.Config) (*sql.DB, error) {
		current = addrKey(c)
		return lazyTestDB()
	}
	ping := func(_ context.Context, _ *sql.DB) error {
		if current == "1.1.1.1:3306" {
			return errors.New("dial tcp: connection refused")
		}
		return nil
	}
	db, err := openWithFailover(cfg, open, ping)
	if err != nil {
		t.Fatalf("failover should have found the healthy endpoint: %v", err)
	}
	defer db.Close()
	if current != "2.2.2.2:3307" {
		t.Errorf("selected %s, want the second endpoint", current)
	}
}

func TestOpenWithFailoverSkipsAnOpenErrorToo(t *testing.T) {
	addrCursor.Store(0)
	cfg := twoAddrConfig()
	var current string
	open := func(c *config.Config) (*sql.DB, error) {
		current = addrKey(c)
		if c.Host == "1.1.1.1" {
			return nil, errors.New("bad connection string")
		}
		return lazyTestDB()
	}
	ping := func(context.Context, *sql.DB) error { return nil }
	db, err := openWithFailover(cfg, open, ping)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if current != "2.2.2.2:3307" {
		t.Errorf("selected %s, want the second endpoint", current)
	}
}

func TestOpenWithFailoverReportsEveryEndpoint(t *testing.T) {
	addrCursor.Store(0)
	cfg := twoAddrConfig()
	open := func(*config.Config) (*sql.DB, error) { return lazyTestDB() }
	ping := func(context.Context, *sql.DB) error { return errors.New("dial failed") }
	db, err := openWithFailover(cfg, open, ping)
	if db != nil {
		db.Close()
	}
	if err == nil {
		t.Fatal("all endpoints failing must be an error")
	}
	for _, want := range []string{"all 2 addresses failed", "1.1.1.1:3306", "2.2.2.2:3307", "dial failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err.Error(), want)
		}
	}
}

// TestOpenWithFailoverStopsOnSessionVarError pins that a rejected
// ALTER SESSION SET is a configuration error, not a dead node: the loop must
// not try the second address and must not wrap the cause in "all addresses
// failed". The ping error is wrapped once so errors.As has to see through it.
func TestOpenWithFailoverStopsOnSessionVarError(t *testing.T) {
	addrCursor.Store(0)
	cfg := twoAddrConfig()
	opened := 0
	open := func(*config.Config) (*sql.DB, error) {
		opened++
		return lazyTestDB()
	}
	svErr := &damengSessionVarError{name: "SORT_BUF_SIZE", cause: errors.New("Error -837: 配置参数不存在")}
	ping := func(context.Context, *sql.DB) error { return fmt.Errorf("ping: %w", svErr) }
	db, err := openWithFailover(cfg, open, ping)
	if db != nil {
		db.Close()
	}
	if err == nil {
		t.Fatal("a rejected session variable must be an error")
	}
	if opened != 1 {
		t.Errorf("opened %d endpoints, want 1 (no failover for a bad --session-var)", opened)
	}
	if !errors.Is(err, svErr) {
		t.Errorf("error = %v, want the session-var error itself", err)
	}
	if strings.Contains(err.Error(), "all 2 addresses failed") {
		t.Errorf("error must not be the failover aggregate: %v", err)
	}
	if !strings.Contains(err.Error(), "--session-var SORT_BUF_SIZE") {
		t.Errorf("error must name the option: %v", err)
	}
}
