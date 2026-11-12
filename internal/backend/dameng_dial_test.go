package backend

import (
	"net"
	"testing"
	"time"

	"github.com/imex/internal/config"
)

// TestDamengDialNameIsTimeoutScoped pins why the timeouts live in the dialer
// name: the driver keeps one global dialer registry keyed by that name, so a
// run with a different --socket-read-timeout must not reuse another run's
// dialer (and therefore another run's deadlines).
func TestDamengDialNameIsTimeoutScoped(t *testing.T) {
	a := damengDialName(&config.Config{
		ConnectTimeout: 5 * time.Second, SocketReadTimeout: 5 * time.Minute,
		SocketWriteTimeout: 5 * time.Minute,
	})
	b := damengDialName(&config.Config{
		ConnectTimeout: 5 * time.Second, SocketReadTimeout: 10 * time.Second,
		SocketWriteTimeout: 5 * time.Minute,
	})
	if a == b {
		t.Errorf("different read timeouts produced the same dialer name %q", a)
	}
}

// TestDamengDeadlineConnBoundsHandshake is the hang fix. The driver's epGroup
// path calls connectSingle(context.Background()), so no context can abort a
// login handshake that never answers. The absolute deadline the dialer puts on
// the connection is the only thing that turns the wait into an error.
func TestDamengDeadlineConnBoundsHandshake(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	dc := &damengDeadlineConn{Conn: client, read: 5 * time.Second, write: 5 * time.Second}
	if err := client.SetDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}

	start := time.Now()
	_, err := dc.Read(make([]byte, 16))
	if err == nil {
		t.Fatal("a handshake that never answers must fail")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("handshake waited %v, want the connect budget", elapsed)
	}
	if dc.established.Load() {
		t.Error("a failed handshake must not mark the session established")
	}
}

// TestDamengDeadlineConnRefreshesPerRead guards the other half: once the server
// has answered, the absolute handshake deadline is cleared and every read gets
// its own budget. Without the refresh a silent connection would hang again, and
// with the original absolute deadline left in place a long statement would be
// cut short mid-session.
func TestDamengDeadlineConnRefreshesPerRead(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	const readBudget = 200 * time.Millisecond
	dc := &damengDeadlineConn{Conn: client, read: readBudget, write: readBudget}
	if err := client.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}

	go func() { _, _ = server.Write([]byte("ok")) }()
	if _, err := dc.Read(make([]byte, 2)); err != nil {
		t.Fatalf("handshake read: %v", err)
	}
	if !dc.established.Load() {
		t.Fatal("a successful handshake read must mark the session established")
	}

	// No more data. The second read must stop at the per-read budget; if the
	// wrapper cleared the deadline without refreshing it, this would hang.
	done := make(chan error, 1)
	go func() {
		_, err := dc.Read(make([]byte, 2))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a silent read must fail at its deadline")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second read hung: the per-read deadline was not refreshed")
	}
}
