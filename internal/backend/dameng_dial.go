package backend

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"gitee.com/chunanyong/dm"

	"github.com/imex/internal/config"
)

// Why this file exists.
//
// `imex list dbs --engine dm` used to hang forever against a Dameng server that
// stopped answering after the TCP connection was accepted (a sleeping container,
// a half-open firewall, a stalled login handshake). Three facts combine:
//
//  1. gitee.com/chunanyong/dm v1.8.23 has NO SetReadDeadline / SetDeadline call
//     anywhere in its source. Its only TCP deadline is socketTimeout (seconds),
//     handed to net.DialTimeout in a.go:50, and that applies to the dial only.
//  2. The driver's epGroup path — used even for a plain single-host DSN, see
//     n.go:880 — calls connectSingle(context.Background()) at x.go:158. The
//     context the caller passed to db.PingContext/QueryContext is DISCARDED for
//     the login handshake, so watchCancel sees ctx.Done() == nil and never
//     starts. No context can abort a handshake that never answers.
//  3. Once the connection exists, the driver does honour the caller's context
//     (ping/queryContext call watchCancel(ctx)), so the Go-side deadlines added
//     in cmd/ cover statements; only the handshake needs a socket deadline.
//
// The driver's RegisterDialContext hook (p.go:71, selected by the `dialName`
// DSN parameter) is the one place we can wrap the net.Conn. The wrapper puts an
// absolute deadline on the handshake and then switches to per-read/per-write
// deadlines so a long-running statement is not cut short.

const damengDialPrefix = "imex-dm-deadline"

// damengDialName is part of the DSN. The driver keeps ONE global dialer registry
// keyed by name, so the timeout values have to be part of the identity: two runs
// with different --socket-read-timeout must not silently share one dialer.
func damengDialName(c *config.Config) string {
	return fmt.Sprintf("%s-%d-%d-%d", damengDialPrefix,
		c.ConnectTimeout.Milliseconds(),
		c.SocketReadTimeout.Milliseconds(),
		c.SocketWriteTimeout.Milliseconds())
}

var damengDialRegistry sync.Map

// registerDamengDial installs the deadline-keeping dialer once per timeout set.
// It is idempotent and safe for concurrent runs.
func registerDamengDial(c *config.Config) {
	name := damengDialName(c)
	if _, loaded := damengDialRegistry.LoadOrStore(name, struct{}{}); loaded {
		return
	}
	connect, read, write := c.ConnectTimeout, c.SocketReadTimeout, c.SocketWriteTimeout
	dm.RegisterDialContext(name, func(_ context.Context, addr string) (net.Conn, error) {
		conn, err := net.DialTimeout("tcp", addr, connect)
		if err != nil {
			return nil, err
		}
		// dm_build_22 (the driver's own dialer) sets these; a custom dialer
		// bypasses it, so keep the same socket options.
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetKeepAlive(true)
			_ = tcp.SetNoDelay(true)
		}
		dc := &damengDeadlineConn{Conn: conn, read: read, write: write}
		if connect > 0 {
			// Absolute budget for the login handshake, which no ctx can cancel
			// on this driver (see the file comment). An unresponsive server now
			// fails within --connect-timeout instead of hanging the process.
			_ = dc.Conn.SetDeadline(time.Now().Add(connect))
		}
		return dc, nil
	})
}

// damengDeadlineConn keeps the handshake bounded without shortening the rest of
// the session. Before the first successful read the absolute deadline set by the
// dialer applies; once the server has answered, every read/write refreshes its
// own deadline so a query that legitimately runs for minutes is not cut off.
type damengDeadlineConn struct {
	net.Conn
	read, write time.Duration
	established atomic.Bool
}

func (c *damengDeadlineConn) Read(b []byte) (int, error) {
	if c.established.Load() && c.read > 0 {
		if err := c.Conn.SetReadDeadline(time.Now().Add(c.read)); err != nil {
			return 0, err
		}
	}
	n, err := c.Conn.Read(b)
	if err == nil && n > 0 && c.established.CompareAndSwap(false, true) {
		// The handshake answered. Drop the absolute deadline; from here on the
		// per-read/per-write budgets (and the Go-side context) govern the socket.
		_ = c.Conn.SetDeadline(time.Time{})
	}
	return n, err
}

func (c *damengDeadlineConn) Write(b []byte) (int, error) {
	if c.established.Load() && c.write > 0 {
		if err := c.Conn.SetWriteDeadline(time.Now().Add(c.write)); err != nil {
			return 0, err
		}
	}
	return c.Conn.Write(b)
}
