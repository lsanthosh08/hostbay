package sshx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

const (
	defaultKeepalive = 30 * time.Second
	probeTimeout     = 10 * time.Second
)

// Manager owns one connection per Target.Key and hands out leases on it
// (KTD5). The last lease released closes the connection.
type Manager struct {
	dialer *Dialer

	mu     sync.Mutex
	conns  map[string]*conn
	leases map[string]*Lease
}

func NewManager(d *Dialer) *Manager {
	return &Manager{dialer: d, conns: map[string]*conn{}, leases: map[string]*Lease{}}
}

// Dialer is the manager's dialer, for a connection that is not shared,
// such as a test before saving (R53).
func (m *Manager) Dialer() *Dialer { return m.dialer }

// KnownHostsPath is the known_hosts file this manager's connections check.
func (m *Manager) KnownHostsPath() string { return m.dialer.KnownHostsPath }

// conn is one shared connection. client is nil before the first dial and
// after a drop; ready is non-nil while a dial is in flight.
type conn struct {
	key    string
	target Target
	refs   int
	client *Client
	gen    uint64
	ready  chan struct{}
	err    error
}

// Lease is one consumer's hold on a shared connection. It stays valid across
// reconnects.
type Lease struct {
	id   string
	m    *Manager
	c    *conn
	once sync.Once
}

// Acquire returns a lease on t's connection, dialling it if no live one
// exists. Host-key and secret errors come back as from Dialer.Dial.
func (m *Manager) Acquire(ctx context.Context, t Target) (*Lease, error) {
	if t.Key == "" {
		return nil, errors.New("sshx: target has no key")
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	m.mu.Lock()
	c := m.conns[t.Key]
	if c == nil {
		c = &conn{key: t.Key, target: t}
		m.conns[t.Key] = c
	}
	c.refs++
	l := &Lease{id: hex.EncodeToString(b[:]), m: m, c: c}
	m.leases[l.id] = l
	m.mu.Unlock()

	if _, err := l.Reconnect(ctx); err != nil {
		l.Release()
		return nil, err
	}
	return l, nil
}

// Lease looks up a lease by ID, or returns nil.
func (m *Manager) Lease(id string) *Lease {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.leases[id]
}

func (l *Lease) ID() string { return l.id }

// Client is the connection's current client, or nil while it is dropped.
// After Reconnect, every lease sees the new client.
func (l *Lease) Client() *Client {
	l.m.mu.Lock()
	defer l.m.mu.Unlock()
	return l.c.client
}

// Live returns the current client, or rebuilds the connection when it has
// dropped. Unlike Reconnect it does not probe a client that is still set.
func (l *Lease) Live(ctx context.Context) (*Client, error) {
	if cl := l.Client(); cl != nil {
		return cl, nil
	}
	return l.Reconnect(ctx)
}

// Generation is 1 after the first dial and grows by one per rebuild.
func (l *Lease) Generation() uint64 {
	l.m.mu.Lock()
	defer l.m.mu.Unlock()
	return l.c.gen
}

// LoginPassword is the destination's saved login password, or "" for key
// auth. Sudo reuses it (R5); it lives on the connection, so it survives a
// rebuild (KTD5).
func (l *Lease) LoginPassword() string {
	hops := l.c.target.Hops // set once when the connection is created
	if len(hops) == 0 {
		return ""
	}
	return hops[len(hops)-1].Auth.Password
}

// Reconnect returns a live client. While the connection answers, it is
// returned as is and the caller reopens only its own channel. Once the
// connection has dropped, one rebuild runs for all concurrent callers: it
// re-dials the chain with fresh host-key checks, gives every lease the new
// client and closes the old one (R34).
func (l *Lease) Reconnect(ctx context.Context) (*Client, error) {
	m, c := l.m, l.c
	m.mu.Lock()
	if cl := c.client; cl != nil && c.ready == nil {
		m.mu.Unlock()
		if alive(cl) {
			return cl, nil
		}
		cl.Close()
		m.mu.Lock()
		if c.client == cl {
			c.client = nil
		}
	}
	if c.client == nil && c.ready == nil {
		c.ready = make(chan struct{})
		go m.rebuild(c)
	}
	ready := c.ready
	m.mu.Unlock()

	if ready != nil {
		select {
		case <-ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.client == nil {
		if c.err == nil {
			return nil, errors.New("sshx: connection dropped")
		}
		return nil, c.err
	}
	return c.client, nil
}

// rebuild dials c's chain once on behalf of every waiting caller. It is not
// tied to any one caller's context; Dialer timeouts bound it.
func (m *Manager) rebuild(c *conn) {
	cl, err := m.dialer.Dial(context.Background(), c.target)
	m.mu.Lock()
	if err == nil && c.refs == 0 { // every lease left while dialling
		cl.Close()
		cl, err = nil, errors.New("sshx: connection released")
	}
	if err == nil {
		c.client = cl
		c.gen++
		go m.watch(c, cl)
	}
	c.err = err
	close(c.ready)
	c.ready = nil
	m.mu.Unlock()
}

// watch marks c dropped when cl's transport ends, and closes cl when a
// keepalive goes unanswered so that a dead link is noticed.
func (m *Manager) watch(c *conn, cl *Client) {
	done := make(chan struct{})
	go func() { cl.Wait(); close(done) }()
	every := c.target.Keepalive
	if every <= 0 {
		every = defaultKeepalive
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-done:
			m.mu.Lock()
			if c.client == cl {
				c.client = nil
			}
			m.mu.Unlock()
			cl.Close() // the jump hops outlive the last hop's transport
			return
		case <-t.C:
			if !alive(cl) {
				cl.Close()
			}
		}
	}
}

func alive(cl *Client) bool {
	res := make(chan error, 1)
	go func() {
		_, _, err := cl.SendRequest("keepalive@openssh.com", true, nil)
		res <- err
	}()
	select {
	case err := <-res:
		return err == nil
	case <-time.After(probeTimeout):
		return false
	}
}

// Release gives up the lease; the last one closes the connection. Calling
// it again does nothing.
func (l *Lease) Release() {
	l.once.Do(func() {
		m, c := l.m, l.c
		m.mu.Lock()
		delete(m.leases, l.id)
		c.refs--
		var cl *Client
		if c.refs == 0 {
			if m.conns[c.key] == c {
				delete(m.conns, c.key)
			}
			cl, c.client = c.client, nil
		}
		m.mu.Unlock()
		if cl != nil {
			cl.Close()
		}
	})
}

// Forward is a local listener on 127.0.0.1 whose connections are carried to
// a remote address through a lease's connection.
type Forward struct {
	ln    net.Listener
	wg    sync.WaitGroup
	mu    sync.Mutex
	open  map[net.Conn]struct{}
	close bool
}

// Forward listens on 127.0.0.1:0 and carries each accepted connection to
// remoteAddr (host:port as seen from the last hop). New connections use the
// lease's current client, so they follow a reconnect.
func (l *Lease) Forward(remoteAddr string) (*Forward, error) {
	return ForwardFunc(func() (net.Conn, error) {
		cl := l.Client()
		if cl == nil {
			return nil, errors.New("sshx: connection dropped")
		}
		return cl.Dial("tcp", remoteAddr)
	})
}

// ForwardFunc listens on 127.0.0.1:0 and carries each accepted connection
// to one dial opens, for a helper that can only dial a plain address: a
// jump host's channel, or a proxy's tunnel (KTD22).
func ForwardFunc(dial func() (net.Conn, error)) (*Forward, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f := &Forward{ln: ln, open: map[net.Conn]struct{}{}}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			lc, err := ln.Accept()
			if err != nil {
				return
			}
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				if !f.track(lc) {
					lc.Close()
					return
				}
				defer f.untrack(lc)
				rc, err := dial()
				if err != nil || !f.track(rc) {
					if rc != nil {
						rc.Close()
					}
					lc.Close()
					return
				}
				defer f.untrack(rc)
				pipe(lc, rc)
			}()
		}
	}()
	return f, nil
}

// Addr is the local address to connect to.
func (f *Forward) Addr() net.Addr { return f.ln.Addr() }

// Close stops listening, cuts open connections and waits for them to end.
func (f *Forward) Close() error {
	err := f.ln.Close()
	f.mu.Lock()
	f.close = true
	for c := range f.open {
		c.Close()
	}
	f.mu.Unlock()
	f.wg.Wait()
	return err
}

func (f *Forward) track(c net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.close {
		return false
	}
	f.open[c] = struct{}{}
	return true
}

func (f *Forward) untrack(c net.Conn) {
	f.mu.Lock()
	delete(f.open, c)
	f.mu.Unlock()
}

// pipe copies both ways until either side ends, then closes both.
func pipe(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	cp := func(dst io.Writer, src io.Reader) { io.Copy(dst, src); done <- struct{}{} }
	go cp(a, b)
	go cp(b, a)
	<-done
	a.Close()
	b.Close()
	<-done
}
