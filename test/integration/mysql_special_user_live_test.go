package integration

// Live regression for DSN-metacharacter usernames (the `test@SERVICE:attest`
// bug). go-sql-driver's FormatDSN writes User/Passwd verbatim into
// `user:pass@tcp(...)`, so feeding that string back through sql.Open re-parses a
// ':' in the username as the password separator and truncates it. Production
// now uses mysql.NewConnector on a Config; this test drives that same path with
// a real server. Skipped unless IMEX_TEST_DSN (an admin TCP DSN) is set.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/imex/internal/backend"
)

func TestLive_MySQLSpecialCharUsernameConnects(t *testing.T) {
	admin := newLiveEnv(t) // skips when IMEX_TEST_DSN is unset

	// A username with '@' and ':' — exactly the shape the old DSN path mangled.
	const user = "itest@SERVICE:attest"
	const pass = "attest:i@pass"
	const host = "%"

	// Create the fixture user with privileges on the test schema, and always
	// drop it afterwards so repeated runs are idempotent.
	admin.exec(t, fmt.Sprintf("DROP USER IF EXISTS '%s'@'%s'", user, host))
	admin.exec(t, fmt.Sprintf("CREATE USER '%s'@'%s' IDENTIFIED BY '%s'", user, host, pass))
	admin.exec(t, fmt.Sprintf("GRANT SELECT ON %s.* TO '%s'@'%s'", admin.base.Database, user, host))
	t.Cleanup(func() {
		// Use a fresh admin statement on the shared DB; ignore cleanup failure.
		_, _ = admin.db.Exec(fmt.Sprintf("DROP USER IF EXISTS '%s'@'%s'", user, host))
	})

	// Drive the exact production connection path: a config.Config handed to the
	// MySQL backend's Open (which uses mysql.NewConnector), not a DSN string.
	cfg := admin.base
	cfg.User = user
	cfg.Password = pass

	db, err := backend.Must(backend.EngineMySQL).Open(&cfg)
	if err != nil {
		t.Fatalf("Open with special username: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping with special username %q: %v", user, err)
	}
	var current string
	if err := db.QueryRowContext(ctx, "SELECT CURRENT_USER()").Scan(&current); err != nil {
		t.Fatalf("SELECT CURRENT_USER(): %v", err)
	}
	// CURRENT_USER() reports 'user'@'host'; assert the full exotic user survived.
	if !strings.HasPrefix(current, user+"@") {
		t.Errorf("connected as %q, want user %q intact", current, user)
	}

	// A real query through the opened DB must work too.
	var one int
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Errorf("SELECT 1 = %d, err = %v", one, err)
	}
}
