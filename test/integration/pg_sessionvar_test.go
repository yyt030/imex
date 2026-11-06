package integration

// Live PostgreSQL tests for --session-var, the option that was a refusal until
// this engine began honouring it the way MySQL does: a "SET <name> = <value>"
// run on every NEW physical connection. These cases cannot be unit-tested
// because the whole point is what a real server answers back, so they dial the
// same backend the command builds and read the GUC off the session.
//
// Skipped unless IMEX_TEST_PG_DSN is set:
//
//	IMEX_TEST_PG_DSN='postgres://user:pass@127.0.0.1:5432/db?sslmode=disable' \
//	  go test ./test/integration -run PGSessionVar -v

import (
	"context"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/imex/internal/backend"
	"github.com/imex/internal/common"
	"github.com/imex/internal/config"
)

// pgConfigFromEnv builds the same config.Config the CLI would from
// IMEX_TEST_PG_DSN, so the URL backend.Open sees is exactly the one a run writes.
func pgConfigFromEnv(t *testing.T) *config.Config {
	t.Helper()
	dsn := os.Getenv("IMEX_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set IMEX_TEST_PG_DSN to run live PostgreSQL session-variable tests")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse IMEX_TEST_PG_DSN: %v", err)
	}
	pass, _ := u.User.Password()
	host, portText, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split DSN host %q: %v", u.Host, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("DSN port %q: %v", portText, err)
	}
	return &config.Config{
		Engine:             string(backend.EnginePG),
		Host:               host,
		Port:               port,
		User:               u.User.Username(),
		Password:           pass,
		Database:           strings.TrimPrefix(u.Path, "/"),
		LogLevel:           config.LogLevelError,
		DefaultCharset:     config.DefaultCharset,
		ConnectTimeout:     config.DefaultConnectTimeout,
		SocketReadTimeout:  config.DefaultSocketTimeout,
		SocketWriteTimeout: config.DefaultSocketTimeout,
		ProgressInterval:   config.DefaultProgressInterval,
	}
}

// TestPGSessionVarAppliedOnOpen proves the AfterConnect SET actually reaches the
// server: a run that names a work_mem and an application_name reads them back
// from the session it dialed.
func TestPGSessionVarAppliedOnOpen(t *testing.T) {
	cfg := pgConfigFromEnv(t)

	vars, err := common.ParseSessionVars(
		[]string{"application_name=imex-sessionvar-test", "work_mem=64MB"},
		backend.Lookup("pg").IsReservedConnParam,
	)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg.Vars = vars

	db, err := backend.Must(backend.EnginePG).Open(cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	var app, wm string
	if err := db.QueryRowContext(ctx,
		"SELECT current_setting('application_name'), current_setting('work_mem')").
		Scan(&app, &wm); err != nil {
		t.Fatalf("read session vars: %v", err)
	}
	if app != "imex-sessionvar-test" {
		t.Errorf("application_name = %q, want imex-sessionvar-test", app)
	}
	// PostgreSQL reports work_mem in its internal unit (kB), so 64MB reads back as
	// 64MB or 65536kB depending on version; accept either spelling of the size.
	if wm != "64MB" && wm != "65536kB" {
		t.Errorf("work_mem = %q, want 64MB", wm)
	}
}

// TestPGSessionVarOnEveryConnection is the property the mechanism exists for: a
// pooled run must carry the SET on each new physical connection, not just the one
// Ping dialled first. With a pool that can open several, every worker reads the
// same value back.
func TestPGSessionVarOnEveryConnection(t *testing.T) {
	cfg := pgConfigFromEnv(t)

	vars, err := common.ParseSessionVars(
		[]string{"application_name=imex-perconn"},
		backend.Lookup("pg").IsReservedConnParam,
	)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg.Vars = vars

	db, err := backend.Must(backend.EnginePG).Open(cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(6)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const workers = 6
	var wg sync.WaitGroup
	results := make([]string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Hold a dedicated connection so the pool must dial several physical
			// ones, each running AfterConnect.
			conn, err := db.Conn(ctx)
			if err != nil {
				results[i] = "conn: " + err.Error()
				return
			}
			defer conn.Close()
			var app string
			if err := conn.QueryRowContext(ctx,
				"SELECT current_setting('application_name')").Scan(&app); err != nil {
				results[i] = "query: " + err.Error()
				return
			}
			results[i] = app
		}(i)
	}
	wg.Wait()

	for i, got := range results {
		if got != "imex-perconn" {
			t.Errorf("worker %d application_name = %q, want imex-perconn", i, got)
		}
	}
}

// TestPGSessionVarRejectedValueIsConnectError pins the failure mode: a value the
// server will not accept for a valid name fails at connection time (through the
// SET), the same place a bad MySQL session variable fails — it does not slip
// through silently.
func TestPGSessionVarRejectedValueIsConnectError(t *testing.T) {
	cfg := pgConfigFromEnv(t)

	// work_mem is a valid name; "not-a-size" is a value PostgreSQL rejects.
	cfg.Vars = map[string]string{"work_mem": "'not-a-size'"}

	db, err := backend.Must(backend.EnginePG).Open(cfg)
	if err != nil {
		// An outright Open failure is also acceptable; the contract is "fails,
		// does not silently succeed."
		return
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err == nil {
		t.Fatalf("a rejected session-var value must fail the connection, but ping succeeded")
	}
}
