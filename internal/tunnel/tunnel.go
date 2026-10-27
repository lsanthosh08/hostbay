// Package tunnel runs saved port forwards over an SSH connection: local
// (-L), remote (-R) and dynamic SOCKS (-D) (R23). Listeners bind loopback
// unless the tunnel explicitly allows more (R33), and a dropped connection
// ends the tunnel with a reason instead of restarting it.
//
// The forwarding mechanics follow uniterm's
// backend/session/tunnel_forward.go (Apache-2.0); host keys are checked by
// internal/sshx, not here.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type Kind string

const (
	Local   Kind = "local"   // listen here, dial from the host
	Remote  Kind = "remote"  // listen on the host, dial from here
	Dynamic Kind = "dynamic" // SOCKS5 here, dial from the host
)

// ErrExternalBind refuses a listener beyond loopback that the tunnel did not
// explicitly allow.
var ErrExternalBind = errors.New("listening beyond 127.0.0.1 must be allowed explicitly for this tunnel")

// Spec describes one tunnel. For Local and Dynamic, BindHost:BindPort is on
// this computer and TargetHost:TargetPort is dialled from the host. For
// Remote, BindHost:BindPort is on the host and the target is dialled from
// this computer. Dynamic has no target. BindPort 0 picks a free port.
type Spec struct {
	Kind          Kind
	BindHost      string // "" means 127.0.0.1
	BindPort      int
	TargetHost    string
	TargetPort    int
	AllowExternal bool
}

func (s Spec) bindAddr() string {
	h := s.BindHost
	if h == "" {
		h = "127.0.0.1"
	}
	return net.JoinHostPort(h, strconv.Itoa(s.BindPort))
}

func (s Spec) target() string { return net.JoinHostPort(s.TargetHost, strconv.Itoa(s.TargetPort)) }

// Validate checks s before anything listens.
func (s Spec) Validate() error {
	switch s.Kind {
	case Local, Remote, Dynamic:
	default:
		return fmt.Errorf("unknown tunnel kind %q", s.Kind)
	}
	if s.BindPort < 0 || s.BindPort > 65535 {
		return fmt.Errorf("listen port %d is out of range", s.BindPort)
	}
	if s.Kind != Dynamic && (s.TargetHost == "" || s.TargetPort < 1 || s.TargetPort > 65535) {
		return errors.New("the tunnel needs a destination host and port")
	}
	if !s.AllowExternal && !isLoopback(s.BindHost) {
		return ErrExternalBind
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Tunnel is a running forward.
type Tunnel struct {
	spec   Spec
	cl     *ssh.Client
	ln     net.Listener
	ctx    context.Context // cancelled at shutdown so pending dials give up
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
	done   chan struct{}

	mu      sync.Mutex
	open    map[net.Conn]struct{}
	closing bool
	err     error
}

// Open starts listening and forwards connections over cl until Close, or
// until cl's connection ends, which shuts the tunnel down by itself with a
// reason in Err. Bind failures (a port in use, a host refusing to listen)
// come back as the error.
func Open(cl *ssh.Client, s Spec) (*Tunnel, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	var ln net.Listener
	var err error
	if s.Kind == Remote {
		if ln, err = cl.Listen("tcp", s.bindAddr()); err != nil {
			return nil, fmt.Errorf("the host refused to listen on %s: %w", s.bindAddr(), err)
		}
	} else if ln, err = net.Listen("tcp", s.bindAddr()); err != nil {
		return nil, err // names the address and why, e.g. already in use
	}
	t := &Tunnel{spec: s, cl: cl, ln: ln, done: make(chan struct{}), open: map[net.Conn]struct{}{}}
	t.ctx, t.cancel = context.WithCancel(context.Background())
	t.wg.Add(1)
	go t.accept()
	// shortcut: this goroutine outlives a stopped tunnel until the shared
	// connection ends, since Wait cannot be cancelled; fine at a few per start.
	go func() { t.shutdown(dropped(cl.Wait())) }()
	return t, nil
}

func dropped(err error) error {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return errors.New("the connection to the host dropped")
	}
	return fmt.Errorf("the connection to the host dropped: %w", err)
}

// Addr is where the tunnel listens: on this computer, or on the host for a
// remote tunnel.
func (t *Tunnel) Addr() net.Addr { return t.ln.Addr() }

// Done is closed once the tunnel has stopped and its connections are gone.
func (t *Tunnel) Done() <-chan struct{} { return t.done }

// Err says why the tunnel ended by itself; it is nil after Close.
func (t *Tunnel) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// Close stops listening, cuts every forwarded connection and waits for them.
func (t *Tunnel) Close() { t.shutdown(nil) }

func (t *Tunnel) shutdown(reason error) {
	t.once.Do(func() {
		t.mu.Lock()
		t.closing, t.err = true, reason
		for c := range t.open {
			c.Close()
		}
		t.mu.Unlock()
		t.cancel()
		// A remote listener's Close asks the host to stop forwarding; on a
		// connection that is already gone it returns at once.
		t.ln.Close()
		t.wg.Wait()
		close(t.done)
	})
}

func (t *Tunnel) accept() {
	defer t.wg.Done()
	for {
		c, err := t.ln.Accept()
		if err != nil {
			if !t.track(nil) {
				return // closed by shutdown
			}
			// The listener failed on its own. Report a drop if the
			// connection is gone, else the listener's error. Outside wg, since
			// shutdown waits for wg.
			go func() {
				reason := fmt.Errorf("the tunnel stopped accepting connections: %w", err)
				if _, _, kerr := t.cl.SendRequest("keepalive@openssh.com", true, nil); kerr != nil {
					reason = dropped(kerr)
				}
				t.shutdown(reason)
			}()
			return
		}
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			t.serve(c)
		}()
	}
}

// serve carries one accepted connection to the tunnel's destination.
func (t *Tunnel) serve(c net.Conn) {
	if !t.track(c) {
		c.Close()
		return
	}
	defer t.untrack(c)
	var dst net.Conn
	var err error
	switch t.spec.Kind {
	case Local:
		dst, err = t.cl.DialContext(t.ctx, "tcp", t.spec.target())
	case Remote:
		dst, err = (&net.Dialer{Timeout: 10 * time.Second}).DialContext(t.ctx, "tcp", t.spec.target())
	case Dynamic:
		c.SetDeadline(time.Now().Add(socksHandshakeWait * time.Second))
		var addr string
		if addr, err = socksHandshake(c); err == nil {
			if dst, err = t.cl.DialContext(t.ctx, "tcp", addr); err != nil {
				socksReply(c, socksUnreachable)
			} else if err = socksReply(c, socksOK); err != nil {
				dst.Close()
			}
		}
		c.SetDeadline(time.Time{})
	}
	if err != nil {
		c.Close()
		return
	}
	if !t.track(dst) {
		dst.Close()
		c.Close()
		return
	}
	defer t.untrack(dst)
	pipe(c, dst)
}

// track registers c so shutdown can cut it, and reports false once shutdown
// has begun. track(nil) only asks.
func (t *Tunnel) track(c net.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closing {
		return false
	}
	if c != nil {
		t.open[c] = struct{}{}
	}
	return true
}

func (t *Tunnel) untrack(c net.Conn) {
	t.mu.Lock()
	delete(t.open, c)
	t.mu.Unlock()
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
