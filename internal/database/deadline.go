package database

import (
	"context"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// withDeadlines wraps dial so every connection takes deadlines. A
// connection through a jump host is an SSH channel, which has none, and
// go-redis and the MongoDB driver fail a connection whose deadline cannot
// be set. Such a connection is closed when a deadline passes instead, and
// the blocked read or write reports a timeout: both drivers drop a
// connection that timed out anyway.
func withDeadlines(dial Dial) Dial {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		c, err := dial(ctx, network, address)
		if err != nil || c.SetDeadline(time.Time{}) == nil {
			return c, err
		}
		return &timedConn{Conn: c}, nil
	}
}

type timedConn struct {
	net.Conn
	expired atomic.Bool

	mu   sync.Mutex
	r, w *time.Timer
}

func (c *timedConn) SetDeadline(t time.Time) error {
	c.SetReadDeadline(t)
	return c.SetWriteDeadline(t)
}

func (c *timedConn) SetReadDeadline(t time.Time) error  { c.set(&c.r, t); return nil }
func (c *timedConn) SetWriteDeadline(t time.Time) error { c.set(&c.w, t); return nil }

func (c *timedConn) set(timer **time.Timer, t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if *timer != nil {
		(*timer).Stop()
		*timer = nil
	}
	if !t.IsZero() {
		*timer = time.AfterFunc(time.Until(t), func() {
			c.expired.Store(true)
			c.Conn.Close()
		})
	}
}

func (c *timedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	return n, c.timeout(err)
}

func (c *timedConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	return n, c.timeout(err)
}

// timeout is err as a timeout once a deadline closed the connection.
func (c *timedConn) timeout(err error) error {
	if err != nil && c.expired.Load() {
		return os.ErrDeadlineExceeded
	}
	return err
}

func (c *timedConn) Close() error {
	c.set(&c.r, time.Time{})
	c.set(&c.w, time.Time{})
	return c.Conn.Close()
}
