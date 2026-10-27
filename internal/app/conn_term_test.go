package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"hostbay/internal/sshx"
)

// termServer is an in-process SSH server with a fake shell: it sends a
// banner, echoes input, and exits on "exit\r". "exec" runs "split" (both
// streams, exit 3) or exits 0.
type termServer struct {
	addr    string
	key     ssh.PublicKey
	conns   atomic.Int32
	x11Reqs atomic.Int32

	mu   sync.Mutex
	live []net.Conn
}

func newTermServer(t *testing.T) *termServer {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	hk, _ := ssh.NewSignerFromKey(priv)
	cfg := &ssh.ServerConfig{PasswordCallback: func(_ ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
		if string(pw) == "pw" {
			return nil, nil
		}
		return nil, errors.New("denied")
	}}
	cfg.AddHostKey(hk)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &termServer{addr: ln.Addr().String(), key: hk.PublicKey()}
	t.Cleanup(func() { ln.Close(); s.dropAll() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			s.conns.Add(1)
			s.mu.Lock()
			s.live = append(s.live, nc)
			s.mu.Unlock()
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nch := range chans {
					go serveTermSession(nch, &s.x11Reqs)
				}
			}()
		}
	}()
	return s
}

func serveTermSession(nch ssh.NewChannel, x11Reqs *atomic.Int32) {
	ch, reqs, err := nch.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	exit := func(code uint32) { ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code})) }
	for req := range reqs {
		switch req.Type {
		case "pty-req", "window-change":
			req.Reply(true, nil)
		case "x11-req":
			x11Reqs.Add(1)
			req.Reply(true, nil)
		case "exec":
			var p struct{ Cmd string }
			ssh.Unmarshal(req.Payload, &p)
			req.Reply(true, nil)
			if p.Cmd == shellProbe { // the AI agent's shell probe
				ch.Write([]byte("/bin/bash"))
				exit(0)
				return
			}
			if p.Cmd == "split" {
				ch.Write([]byte("out\n"))
				ch.Stderr().Write([]byte("err\n"))
				exit(3)
			} else {
				exit(0)
			}
			return
		case "shell":
			req.Reply(true, nil)
			go ssh.DiscardRequests(reqs)
			ch.Write([]byte("Welcome to térm\r\n$ "))
			buf := make([]byte, 1024)
			for {
				n, err := ch.Read(buf)
				if err != nil {
					return
				}
				ch.Write(buf[:n])
				if bytes.Contains(buf[:n], []byte("exit\r")) {
					exit(0)
					return
				}
			}
		default:
			req.Reply(false, nil)
		}
	}
}

func (s *termServer) dropAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.live {
		c.Close()
	}
	s.live = nil
}

// termEvents records what the service emits, by event name.
type termEvents struct {
	mu sync.Mutex
	m  map[string][]string
}

func (e *termEvents) emit(name string, data any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.m == nil {
		e.m = map[string][]string{}
	}
	e.m[name] = append(e.m[name], data.(string))
}

func (e *termEvents) joined(name string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.m[name], "")
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// newTermConn connects a ConnService to a fresh termServer and returns the
// session ID.
func newTermConn(t *testing.T) (*ConnService, *termServer, *termEvents, string) {
	t.Helper()
	srv := newTermServer(t)
	host, port, _ := net.SplitHostPort(srv.addr)
	kh := filepath.Join(t.TempDir(), "known_hosts")
	os.WriteFile(kh, []byte(knownhosts.Line([]string{knownhosts.Normalize(srv.addr)}, srv.key)+"\n"), 0o600)
	c := NewConnService(NewHostKeyTrust(sshx.NewKnownHostAppender(kh)), nil, sshx.NewManager(&sshx.Dialer{KnownHostsPath: kh}))
	ev := &termEvents{}
	c.emit = ev.emit
	p, _ := strconv.Atoi(port)
	sid, err := c.acquire(context.Background(), sshx.Target{Key: "host-1@1", Hops: []sshx.Hop{{Host: host, Port: p, User: "u", Auth: sshx.Auth{Password: "pw"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Disconnect(sid) })
	return c, srv, ev, sid
}

// A terminal asks for X11 only on a host that forwards it (KTD20).
func TestPtyOpenAsksForX11OnlyWhenTheHostForwards(t *testing.T) {
	srv := newTermServer(t)
	host, port, _ := net.SplitHostPort(srv.addr)
	kh := filepath.Join(t.TempDir(), "known_hosts")
	os.WriteFile(kh, []byte(knownhosts.Line([]string{knownhosts.Normalize(srv.addr)}, srv.key)+"\n"), 0o600)
	c := NewConnService(NewHostKeyTrust(sshx.NewKnownHostAppender(kh)), nil, sshx.NewManager(&sshx.Dialer{KnownHostsPath: kh}))
	c.emit = (&termEvents{}).emit
	p, _ := strconv.Atoi(port)
	for _, on := range []bool{false, true} {
		hop := sshx.Hop{Host: host, Port: p, User: "u", Auth: sshx.Auth{Password: "pw"}, X11: on}
		sid, err := c.acquire(context.Background(), sshx.Target{Key: "host-" + strconv.FormatBool(on), Hops: []sshx.Hop{hop}})
		if err != nil {
			t.Fatal(err)
		}
		openTerm(t, c, sid)
		c.Disconnect(sid)
	}
	if n := srv.x11Reqs.Load(); n != 1 {
		t.Fatalf("%d x11-req sent, want 1 (from the host that forwards X11)", n)
	}
}

func openTerm(t *testing.T, c *ConnService, sid string) string {
	t.Helper()
	id, err := c.PtyOpen(context.Background(), sid, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PtyAttach(id); err != nil {
		t.Fatal(err)
	}
	return id
}

func echoes(t *testing.T, c *ConnService, ev *termEvents, id, text string) {
	t.Helper()
	if err := c.PtyWrite(id, text); err != nil {
		t.Fatalf("write %q: %v", text, err)
	}
	waitUntil(t, "echo of "+text, func() bool { return strings.Contains(ev.joined(PtyDataEvent(id)), text) })
}

func TestTerminalBannerSentBeforeAttachAppears(t *testing.T) {
	c, _, ev, sid := newTermConn(t)
	id, err := c.PtyOpen(context.Background(), sid, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // the banner arrives before the UI subscribes
	if err := c.PtyAttach(id); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "banner", func() bool { return ev.joined(PtyDataEvent(id)) == "Welcome to térm\r\n$ " })
	if err := c.PtyResize(id, 120, 40); err != nil {
		t.Fatal(err)
	}
}

// Reopening a tab whose shell exited only opens a new channel: the
// connection is reused and a job in another tab keeps running.
func TestReconnectingExitedTerminalLeavesOtherTabRunning(t *testing.T) {
	c, srv, ev, sid := newTermConn(t)
	a, b := openTerm(t, c, sid), openTerm(t, c, sid)
	echoes(t, c, ev, a, "job running\r")

	c.PtyWrite(b, "exit\r")
	waitUntil(t, "b closed", func() bool { return ev.joined(PtyClosedEvent(b)) == "session closed" })

	b2 := openTerm(t, c, sid)
	echoes(t, c, ev, b2, "hello\r")
	echoes(t, c, ev, a, "still running\r")
	if n := srv.conns.Load(); n != 1 {
		t.Fatalf("%d connections, want 1", n)
	}
	if ev.joined(PtyClosedEvent(a)) != "" {
		t.Fatal("the other tab was closed")
	}
}

// After a drop the tab reports it instead of writing into the dead channel,
// and reopening it on the same session ID rebuilds one shared connection.
func TestDroppedConnectionIsReportedAndReopenRebuildsIt(t *testing.T) {
	c, srv, ev, sid := newTermConn(t)
	a := openTerm(t, c, sid)
	echoes(t, c, ev, a, "before\r")

	srv.dropAll()
	waitUntil(t, "a lost", func() bool { return ev.joined(PtyClosedEvent(a)) == "connection lost" })
	waitUntil(t, "conn dropped", func() bool { return ev.joined(ConnStateEvent(sid)) == "dropped" })
	if err := c.PtyWrite(a, "x"); err == nil {
		t.Fatal("write to a dropped terminal succeeded")
	}

	a2 := openTerm(t, c, sid)
	echoes(t, c, ev, a2, "after\r")
	waitUntil(t, "conn live", func() bool { return ev.joined(ConnStateEvent(sid)) == "droppedlive" })

	// Other calls on the session use the rebuilt connection, not a third.
	if _, err := c.ExecCommand(context.Background(), sid, "true"); err != nil {
		t.Fatal(err)
	}
	if n := srv.conns.Load(); n != 2 {
		t.Fatalf("%d connections after one rebuild, want 2", n)
	}
}

func TestDisconnectClosesItsTerminals(t *testing.T) {
	c, _, ev, sid := newTermConn(t)
	a := openTerm(t, c, sid)
	c.Disconnect(sid)
	waitUntil(t, "a closed", func() bool { return ev.joined(PtyClosedEvent(a)) != "" })
	if err := c.PtyWrite(a, "x"); err == nil {
		t.Fatal("terminal still writable after disconnect")
	}
	if _, err := c.PtyOpen(context.Background(), sid, 80, 24); err == nil {
		t.Fatal("opened a terminal on a released session")
	}
}

func TestExecCommandReturnsStreamsAndCode(t *testing.T) {
	c, _, _, sid := newTermConn(t)
	r, err := c.ExecCommand(context.Background(), sid, "split")
	if err != nil {
		t.Fatal(err)
	}
	if r.Stdout != "out\n" || r.Stderr != "err\n" || r.Code == nil || *r.Code != 3 {
		t.Fatalf("result = %+v", r)
	}
}
