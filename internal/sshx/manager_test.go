package sshx

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"hostbay/internal/apperr"
)

// testServer is an in-process SSH server: password "pw" or an authorized key
// for user "u", "exec" echoes its argument, and "direct-tcpip" dials out so
// it can serve as a jump host or a forward endpoint.
type testServer struct {
	host     string
	port     int
	hostKeys []ssh.Signer
	conns    atomic.Int32 // TCP connections accepted
	auths    atomic.Int32 // password or public-key attempts
	pings    atomic.Int32 // keepalive@openssh.com requests

	mu   sync.Mutex
	live []net.Conn
	ln   net.Listener
}

type serverOpts struct {
	hostKeys    []ssh.Signer    // default: one fresh ed25519 key
	authorized  []ssh.PublicKey // client keys accepted for "u"
	pubKeyAlgos []string        // server's accepted public-key signature algorithms
	kex         []string        // server's key exchanges; nil for the defaults
	gss         *ssh.GSSAPIWithMICConfig
}

func newTestServer(t *testing.T, o serverOpts) *testServer {
	t.Helper()
	s := &testServer{hostKeys: o.hostKeys}
	if len(s.hostKeys) == 0 {
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		hk, _ := ssh.NewSignerFromKey(priv)
		s.hostKeys = []ssh.Signer{hk}
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			s.auths.Add(1)
			if c.User() == "u" && string(pw) == "pw" {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			s.auths.Add(1)
			for _, k := range o.authorized {
				if c.User() == "u" && bytes.Equal(k.Marshal(), key.Marshal()) {
					return nil, nil
				}
			}
			return nil, errors.New("denied")
		},
		PublicKeyAuthAlgorithms: o.pubKeyAlgos,
	}
	cfg.KeyExchanges = o.kex
	cfg.GSSAPIWithMICConfig = o.gss
	for _, k := range s.hostKeys {
		cfg.AddHostKey(k)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.ln = ln
	s.host = "127.0.0.1"
	s.port = ln.Addr().(*net.TCPAddr).Port
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
			go s.serve(nc, cfg)
		}
	}()
	t.Cleanup(func() { ln.Close(); s.dropAll() })
	return s
}

func (s *testServer) serve(nc net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		nc.Close()
		return
	}
	go func() {
		for r := range reqs {
			if r.Type == "keepalive@openssh.com" {
				s.pings.Add(1)
			}
			if r.WantReply {
				r.Reply(false, nil)
			}
		}
	}()
	for nch := range chans {
		switch nch.ChannelType() {
		case "session":
			go serveSession(sc, nch)
		case "direct-tcpip":
			go serveDirect(nch)
		default:
			nch.Reject(ssh.UnknownChannelType, "unsupported")
		}
	}
}

func serveSession(sc *ssh.ServerConn, nch ssh.NewChannel) {
	ch, reqs, err := nch.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	var x11Cookie string // hex, from this session's x11-req
	for req := range reqs {
		if req.Type == "auth-agent-req@openssh.com" {
			req.Reply(true, nil)
			continue
		}
		if req.Type == "x11-req" {
			var p struct {
				Single        bool
				Proto, Cookie string
				Screen        uint32
			}
			ssh.Unmarshal(req.Payload, &p)
			x11Cookie = p.Cookie
			req.Reply(true, nil)
			continue
		}
		if req.Type != "exec" {
			req.Reply(false, nil)
			continue
		}
		var p struct{ Cmd string }
		ssh.Unmarshal(req.Payload, &p)
		req.Reply(true, nil)
		switch {
		case p.Cmd == "ssh-add -l":
			io.WriteString(ch, listForwardedAgent(sc))
		case p.Cmd == "xcookie":
			io.WriteString(ch, x11Cookie)
		case strings.HasPrefix(p.Cmd, "xopen "):
			io.WriteString(ch, openX11(sc, strings.TrimPrefix(p.Cmd, "xopen "), x11Cookie))
		default:
			io.WriteString(ch, strings.TrimPrefix(p.Cmd, "echo ")+"\n")
		}
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
		return
	}
}

// openX11 is the test server's X client: it opens an x11 channel back to
// the client and sends a little-endian connection setup presenting cookie
// ("session" for the hex one the session's x11-req carried, "none" for no
// authorization, else hex), then reports the X server's 8-byte answer,
// "closed" when the channel ends first, or "refused".
func openX11(sc *ssh.ServerConn, which, sessionCookie string) string {
	ch, reqs, err := sc.OpenChannel("x11", ssh.Marshal(struct {
		Addr string
		Port uint32
	}{"127.0.0.1", 40000}))
	if err != nil {
		return "refused"
	}
	go ssh.DiscardRequests(reqs)
	defer ch.Close()
	name, cookie := x11Proto, which
	switch which {
	case "session":
		cookie = sessionCookie
	case "none":
		name, cookie = "", ""
	}
	data, _ := hex.DecodeString(cookie)
	ch.Write(testSetup('l', name, data))
	var reply [8]byte
	if _, err := io.ReadFull(ch, reply[:]); err != nil {
		return "closed"
	}
	return string(reply[:])
}

// testSetup is an X11 connection setup, protocol 11.0, in order 'B' or 'l'.
func testSetup(order byte, name string, data []byte) []byte {
	var bo binary.AppendByteOrder = binary.LittleEndian
	if order == 'B' {
		bo = binary.BigEndian
	}
	b := []byte{order, 0}
	b = bo.AppendUint16(b, 11)
	b = bo.AppendUint16(b, 0)
	b = bo.AppendUint16(b, uint16(len(name)))
	b = bo.AppendUint16(b, uint16(len(data)))
	b = append(b, 0, 0)
	b = append(b, name...)
	b = append(b, make([]byte, pad4(len(name))-len(name))...)
	b = append(b, data...)
	return append(b, make([]byte, pad4(len(data))-len(data))...)
}

// listForwardedAgent is the test server's "ssh-add -l": it opens an agent
// channel back to the client and lists the keys it offers.
func listForwardedAgent(sc *ssh.ServerConn) string {
	ch, reqs, err := sc.OpenChannel("auth-agent@openssh.com", nil)
	if err != nil {
		return "refused: " + err.Error() + "\n"
	}
	go ssh.DiscardRequests(reqs)
	defer ch.Close()
	keys, err := agent.NewClient(ch).List()
	if err != nil {
		return "error: " + err.Error() + "\n"
	}
	var out string
	for _, k := range keys {
		out += ssh.FingerprintSHA256(k) + "\n"
	}
	return out
}

func serveDirect(nch ssh.NewChannel) {
	var p struct {
		Host  string
		Port  uint32
		OHost string
		OPort uint32
	}
	if err := ssh.Unmarshal(nch.ExtraData(), &p); err != nil {
		nch.Reject(ssh.ConnectionFailed, "bad payload")
		return
	}
	c, err := net.Dial("tcp", net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port))))
	if err != nil {
		nch.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nch.Accept()
	if err != nil {
		c.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	pipe(ch, c)
}

// dropAll cuts every connection, as a network failure would.
func (s *testServer) dropAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.live {
		c.Close()
	}
	s.live = nil
}

func (s *testServer) knownLine(t *testing.T) string {
	return khLine(s.host, s.port, s.hostKeys[0].PublicKey())
}

func (s *testServer) hop() Hop {
	return Hop{Host: s.host, Port: s.port, User: "u", Auth: Auth{Password: "pw"}}
}

func run(t *testing.T, c *Client, cmd string) string {
	t.Helper()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()
	out, err := sess.Output(cmd)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	return string(out)
}

func TestDialRunsCommand(t *testing.T) {
	s := newTestServer(t, serverOpts{})
	d := &Dialer{KnownHostsPath: writeKnownHosts(t, s.knownLine(t))}
	c, err := d.Dial(context.Background(), Target{Key: "k", Hops: []Hop{s.hop()}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := run(t, c, "echo hi"); got != "hi\n" {
		t.Fatalf("output = %q", got)
	}
}

// Covers AE10: known_hosts holds only an ed25519 key and the server also
// offers RSA, which the default algorithm order would pick first.
func TestKnownEd25519HostOfferingRSAConnects(t *testing.T) {
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	rsaHost, _ := ssh.NewSignerFromKey(rk)
	_, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	edHost, _ := ssh.NewSignerFromKey(edPriv)
	s := newTestServer(t, serverOpts{hostKeys: []ssh.Signer{rsaHost, edHost}})

	d := &Dialer{KnownHostsPath: writeKnownHosts(t, khLine(s.host, s.port, edHost.PublicKey()))}
	c, err := d.Dial(context.Background(), Target{Key: "k", Hops: []Hop{s.hop()}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c.Close()
}

// Covers AE3 and AE9.
func TestTwoHopChainReportsTheFailingHop(t *testing.T) {
	bastion := newTestServer(t, serverOpts{})
	target := newTestServer(t, serverOpts{})
	tgt := Target{Key: "k", Hops: []Hop{bastion.hop(), target.hop()}}
	ctx := context.Background()

	// Unknown key at the bastion: the target is never dialled.
	d := &Dialer{KnownHostsPath: writeKnownHosts(t, target.knownLine(t))}
	_, err := d.Dial(ctx, tgt)
	var hk *HostKeyError
	if !errors.As(err, &hk) || hk.Changed() || hk.Hop != 0 || hk.Port != bastion.port {
		t.Fatalf("unknown bastion: err = %v", err)
	}
	if target.conns.Load() != 0 {
		t.Fatal("target dialled before the bastion's key was trusted")
	}

	// Changed key at the bastion: refused against hop 0, not the target.
	_, stale := seedKey(t, 20)
	d.KnownHostsPath = writeKnownHosts(t, khLine(bastion.host, bastion.port, stale), target.knownLine(t))
	_, err = d.Dial(ctx, tgt)
	if !errors.As(err, &hk) || !hk.Changed() || hk.Hop != 0 || apperr.CodeOf(err) != apperr.HostKeyChanged {
		t.Fatalf("changed bastion: err = %v", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(bastion.port)) {
		t.Fatalf("refusal does not name the bastion: %v", err)
	}
	if target.conns.Load() != 0 {
		t.Fatal("target dialled past a changed bastion key")
	}

	// Unknown key at the target is reported against hop 1.
	d.KnownHostsPath = writeKnownHosts(t, bastion.knownLine(t))
	if _, err = d.Dial(ctx, tgt); !errors.As(err, &hk) || hk.Hop != 1 || hk.Port != target.port {
		t.Fatalf("unknown target: err = %v", err)
	}

	// Both known: the chain works end to end.
	d.KnownHostsPath = writeKnownHosts(t, bastion.knownLine(t), target.knownLine(t))
	c, err := d.Dial(ctx, tgt)
	if err != nil {
		t.Fatal(err)
	}
	if got := run(t, c, "echo through"); got != "through\n" {
		t.Fatalf("output = %q", got)
	}
	c.Close()
}

func TestKeyAuthWithPassphraseEd25519AndPPKv3RSA(t *testing.T) {
	_, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKeyWithPassphrase(edPriv, "", []byte("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	edPub, _ := ssh.NewPublicKey(edPriv.Public())

	s := newTestServer(t, serverOpts{authorized: []ssh.PublicKey{edPub, fixturePub(t, "rsa-v3")}})
	d := &Dialer{KnownHostsPath: writeKnownHosts(t, s.knownLine(t))}

	for name, a := range map[string]Auth{
		"ed25519 passphrase": {Key: pem.EncodeToMemory(block), Passphrase: "s3cret"},
		"ppk v3 rsa":         {Key: readFixture(t, "rsa-v3")},
	} {
		h := s.hop()
		h.Auth = a
		c, err := d.Dial(context.Background(), Target{Key: "k", Hops: []Hop{h}})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		c.Close()
	}
}

func TestRSAKeyNeverSignsWithSHA1(t *testing.T) {
	s := newTestServer(t, serverOpts{
		authorized:  []ssh.PublicKey{fixturePub(t, "rsa-v3")},
		pubKeyAlgos: []string{ssh.KeyAlgoRSA}, // the server accepts SHA-1 only
	})
	d := &Dialer{KnownHostsPath: writeKnownHosts(t, s.knownLine(t))}
	h := s.hop()
	h.Auth = Auth{Key: readFixture(t, "rsa-v3")}
	if c, err := d.Dial(context.Background(), Target{Key: "k", Hops: []Hop{h}}); err == nil {
		c.Close()
		t.Fatal("authenticated with an ssh-rsa (SHA-1) signature")
	}
}

func TestMissingPasswordAsksForSecretWithoutSendingAuth(t *testing.T) {
	bastion := newTestServer(t, serverOpts{})
	target := newTestServer(t, serverOpts{})
	d := &Dialer{KnownHostsPath: writeKnownHosts(t, bastion.knownLine(t), target.knownLine(t))}
	h := target.hop()
	h.Auth = Auth{}
	_, err := d.Dial(context.Background(), Target{Key: "k", Hops: []Hop{bastion.hop(), h}})
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.SecretNeeded || ae.Details["hop"] != "1" {
		t.Fatalf("err = %v, want SecretNeeded for hop 1", err)
	}
	if n := bastion.auths.Load() + target.auths.Load() + bastion.conns.Load() + target.conns.Load(); n != 0 {
		t.Fatalf("%d connections or auth attempts sent before asking for the secret", n)
	}
}

func TestHandshakeTimeoutIsUnreachable(t *testing.T) {
	if DefaultTimeout != 20*time.Second {
		t.Fatalf("DefaultTimeout = %v, want 20s (session.rs)", DefaultTimeout)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { // accept and never speak
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	d := &Dialer{KnownHostsPath: writeKnownHosts(t, "# empty"), Timeout: 200 * time.Millisecond}
	start := time.Now()
	_, err = d.Dial(context.Background(), Target{Key: "k", Hops: []Hop{{Host: "127.0.0.1", Port: port, User: "u", Auth: Auth{Password: "pw"}}}})
	if apperr.CodeOf(err) != apperr.Unreachable {
		t.Fatalf("err = %v, want Unreachable", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout not applied to the handshake")
	}
}

func newManager(t *testing.T, s *testServer) (*Manager, Target) {
	t.Helper()
	return NewManager(&Dialer{KnownHostsPath: writeKnownHosts(t, s.knownLine(t))}), Target{Key: "host-1@rev3", Hops: []Hop{s.hop()}}
}

// waitClosed reports whether c's connection ends within a second.
func waitClosed(c *Client) bool {
	done := make(chan struct{})
	go func() { c.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(time.Second):
		return false
	}
}

func TestLeasesShareOneConnectionUntilLastRelease(t *testing.T) {
	s := newTestServer(t, serverOpts{})
	m, tgt := newManager(t, s)
	ctx := context.Background()

	a, err := m.Acquire(ctx, tgt)
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Acquire(ctx, tgt)
	if err != nil {
		t.Fatal(err)
	}
	if a.Client() != b.Client() || s.conns.Load() != 1 {
		t.Fatalf("two leases, %d connections", s.conns.Load())
	}
	if a.ID() == b.ID() || m.Lease(a.ID()) != a {
		t.Fatal("lease IDs are not distinct handles")
	}

	c := a.Client()
	a.Release()
	a.Release() // idempotent
	if m.Lease(a.ID()) != nil {
		t.Fatal("released lease still looked up")
	}
	if got := run(t, b.Client(), "echo still"); got != "still\n" {
		t.Fatalf("other lease broken after release: %q", got)
	}
	b.Release()
	if !waitClosed(c) {
		t.Fatal("last release did not close the connection")
	}
}

func TestReconnectAfterDropSwapsEveryLease(t *testing.T) {
	s := newTestServer(t, serverOpts{})
	m, tgt := newManager(t, s)
	ctx := context.Background()
	a, _ := m.Acquire(ctx, tgt)
	b, _ := m.Acquire(ctx, tgt)
	defer a.Release()
	defer b.Release()

	// While alive, Reconnect keeps the connection (only the caller's channel reopens).
	old := a.Client()
	if c, err := a.Reconnect(ctx); err != nil || c != old || s.conns.Load() != 1 {
		t.Fatalf("live reconnect redialled: %v, %d conns", err, s.conns.Load())
	}

	s.dropAll()
	fresh, err := a.Reconnect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fresh == old || b.Client() != fresh {
		t.Fatal("leases do not share the new connection")
	}
	if !waitClosed(old) {
		t.Fatal("old connection not closed")
	}
	if got := run(t, b.Client(), "echo again"); got != "again\n" {
		t.Fatalf("output = %q", got)
	}
	if a.Generation() != 2 || b.Generation() != 2 {
		t.Fatalf("generation = %d, %d; want 2", a.Generation(), b.Generation())
	}
}

func TestConcurrentReconnectDialsOnce(t *testing.T) {
	s := newTestServer(t, serverOpts{})
	m, tgt := newManager(t, s)
	ctx := context.Background()
	var leases []*Lease
	for range 3 {
		l, err := m.Acquire(ctx, tgt)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Release()
		leases = append(leases, l)
	}
	s.dropAll()

	var wg sync.WaitGroup
	clients := make([]*Client, 3)
	for i, l := range leases {
		wg.Go(func() {
			c, err := l.Reconnect(ctx)
			if err != nil {
				t.Error(err)
			}
			clients[i] = c
		})
	}
	wg.Wait()
	// One TCP connection per dial, and each dial runs one host-key check.
	if n := s.conns.Load(); n != 2 {
		t.Fatalf("%d connections after three concurrent reconnects, want 2", n)
	}
	if clients[0] != clients[1] || clients[1] != clients[2] {
		t.Fatal("reconnects returned different clients")
	}
}

func TestForwardCarriesTrafficAndClosesCleanly(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()

	s := newTestServer(t, serverOpts{})
	m, tgt := newManager(t, s)
	l, err := m.Acquire(context.Background(), tgt)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()

	f, err := l.Forward(echo.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if ip := f.Addr().(*net.TCPAddr).IP; !ip.IsLoopback() {
		t.Fatalf("forward listens on %v, want loopback", ip)
	}
	c, err := net.Dial("tcp", f.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(c, "ping")
	buf := make([]byte, 4)
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("read %q, %v", buf, err)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(buf); err == nil {
		t.Fatal("open forwarded connection survived Close")
	}
	if c2, err := net.Dial("tcp", f.Addr().String()); err == nil {
		c2.Close()
		t.Fatal("forward still listening after Close")
	}
}

// The connection is probed at the target's keepalive interval (R52).
func TestKeepaliveIntervalIsHonoured(t *testing.T) {
	s := newTestServer(t, serverOpts{})
	m, tg := newManager(t, s)
	tg.Keepalive = 40 * time.Millisecond
	l, err := m.Acquire(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	deadline := time.Now().Add(5 * time.Second)
	for s.pings.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := s.pings.Load(); n < 3 {
		t.Fatalf("%d keepalives in 5s at a 40ms interval", n)
	}
}
