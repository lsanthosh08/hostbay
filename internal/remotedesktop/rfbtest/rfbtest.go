// Package rfbtest runs an in-process RFB (VNC) server for tests. It offers
// the security types it is given and checks VNC password, Apple Remote
// Desktop and VeNCrypt sign-ins against one user and password. After a
// sign-in it answers ClientInit with a small ServerInit and then echoes
// every byte, so tests can see traffic pass both ways. It also fakes the
// page's end of a GoStream (Page).
package rfbtest

import (
	"bytes"
	"crypto/aes"
	"crypto/des"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"
)

// ServerInit is what the server sends after ClientInit: a 640x480 desktop
// named "rfbtest".
var ServerInit = append([]byte{2, 128, 1, 224, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0, 0, 0, 0, 7}, "rfbtest"...)

// Config is what the server offers and accepts.
type Config struct {
	Version  string   // sent first; "RFB 003.008\n" when empty
	Types    []byte   // security types offered, in order
	SubTypes []uint32 // VeNCrypt subtypes offered
	Cert     tls.Certificate
	User     string // Apple Remote Desktop and VeNCrypt Plain
	Password string
}

// Server is a running test server.
type Server struct {
	Host string
	Port int

	mu  sync.Mutex
	cfg Config
	log []string
	n   int // connections accepted
}

// New starts a server that offers types and stops it when the test ends.
func New(t testing.TB, password string, types ...byte) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := Serve(t, ln, password, types...)
	s.Host, s.Port = "127.0.0.1", ln.Addr().(*net.TCPAddr).Port
	return s
}

// Serve runs the server on ln, such as a Unix socket, until the test ends.
func Serve(t testing.TB, ln net.Listener, password string, types ...byte) *Server {
	t.Cleanup(func() { ln.Close() })
	s := &Server{cfg: Config{Types: types, Password: password}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.n++
			s.mu.Unlock()
			go s.serve(c)
		}
	}()
	return s
}

// Addr is host:port.
func (s *Server) Addr() string { return net.JoinHostPort(s.Host, fmt.Sprint(s.Port)) }

// Set changes the config for later connections.
func (s *Server) Set(change func(*Config)) {
	s.mu.Lock()
	change(&s.cfg)
	s.mu.Unlock()
}

// Conns counts the connections the server accepted.
func (s *Server) Conns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// Log lists what happened, in order: "type N" when a client chose a
// security type, "subtype N", "auth ok" or "auth failed", and "closed" when
// a signed-in client went away.
func (s *Server) Log() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.log...)
}

func (s *Server) record(e string) {
	s.mu.Lock()
	s.log = append(s.log, e)
	s.mu.Unlock()
}

func (s *Server) serve(c net.Conn) {
	defer c.Close()
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	v := cfg.Version
	if v == "" {
		v = "RFB 003.008\n"
	}
	var cv [12]byte
	if _, err := c.Write([]byte(v)); err != nil {
		return
	}
	if _, err := io.ReadFull(c, cv[:]); err != nil {
		return
	}
	if _, err := c.Write(append([]byte{byte(len(cfg.Types))}, cfg.Types...)); err != nil {
		return
	}
	var t [1]byte
	if _, err := io.ReadFull(c, t[:]); err != nil {
		return
	}
	s.record(fmt.Sprint("type ", t[0]))
	var conn net.Conn = c
	ok := false
	switch t[0] {
	case 1:
		ok = true
	case 2:
		ok = vncAuth(c, cfg.Password)
	case 30:
		ok = ardAuth(c, cfg)
	case 19:
		conn, ok = s.veNCrypt(c, cfg)
	default:
		return
	}
	if conn == nil {
		return
	}
	if !ok {
		s.record("auth failed")
		reason := "wrong password"
		conn.Write(append(binary.BigEndian.AppendUint32([]byte{0, 0, 0, 1}, uint32(len(reason))), reason...))
		return
	}
	s.record("auth ok")
	conn.Write([]byte{0, 0, 0, 0})
	if _, err := io.ReadFull(conn, t[:]); err != nil { // ClientInit
		s.record("closed")
		return
	}
	conn.Write(ServerInit)
	io.Copy(conn, conn)
	s.record("closed")
}

func vncAuth(c io.ReadWriter, password string) bool {
	var ch [16]byte
	rand.Read(ch[:])
	if _, err := c.Write(ch[:]); err != nil {
		return false
	}
	var got [16]byte
	if _, err := io.ReadFull(c, got[:]); err != nil {
		return false
	}
	key := make([]byte, 8)
	copy(key, password)
	for i, b := range key { // VNC's DES key has each byte's bits reversed
		var r byte
		for j := 0; j < 8; j++ {
			r |= (b >> j & 1) << (7 - j)
		}
		key[i] = r
	}
	blk, _ := des.NewCipher(key)
	blk.Encrypt(ch[:8], ch[:8])
	blk.Encrypt(ch[8:], ch[8:])
	return got == ch
}

// modp1024 is RFC 2409's 1024-bit MODP group prime.
var modp1024, _ = new(big.Int).SetString("FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7EDEE386BFB5A899FA5AE9F24117C4B1FE649286651ECE65381FFFFFFFFFFFFFFFF", 16)

func ardAuth(c net.Conn, cfg Config) bool {
	const n = 128
	priv, _ := rand.Int(rand.Reader, modp1024)
	pub := new(big.Int).Exp(big.NewInt(2), priv, modp1024)
	msg := []byte{0, 2, 0, n}
	msg = append(msg, modp1024.FillBytes(make([]byte, n))...)
	msg = append(msg, pub.FillBytes(make([]byte, n))...)
	if _, err := c.Write(msg); err != nil {
		return false
	}
	got := make([]byte, 128+n)
	if _, err := io.ReadFull(c, got); err != nil {
		return false
	}
	shared := new(big.Int).Exp(new(big.Int).SetBytes(got[128:]), priv, modp1024)
	key := md5.Sum(shared.FillBytes(make([]byte, n)))
	blk, _ := aes.NewCipher(key[:])
	for i := 0; i < 128; i += 16 {
		blk.Decrypt(got[i:i+16], got[i:i+16])
	}
	cstr := func(b []byte) string { return string(b[:bytes.IndexByte(b, 0)]) }
	return bytes.IndexByte(got[:64], 0) >= 0 && bytes.IndexByte(got[64:128], 0) >= 0 &&
		cstr(got[:64]) == cfg.User && cstr(got[64:128]) == cfg.Password
}

func (s *Server) veNCrypt(c net.Conn, cfg Config) (net.Conn, bool) {
	var v [2]byte
	if _, err := c.Write([]byte{0, 2}); err != nil {
		return nil, false
	}
	if _, err := io.ReadFull(c, v[:]); err != nil || v != [2]byte{0, 2} {
		return nil, false
	}
	msg := []byte{0, byte(len(cfg.SubTypes))}
	for _, st := range cfg.SubTypes {
		msg = binary.BigEndian.AppendUint32(msg, st)
	}
	if _, err := c.Write(msg); err != nil {
		return nil, false
	}
	var st [4]byte
	if _, err := io.ReadFull(c, st[:]); err != nil {
		return nil, false
	}
	sub := binary.BigEndian.Uint32(st[:])
	s.record(fmt.Sprint("subtype ", sub))
	if _, err := c.Write([]byte{1}); err != nil {
		return nil, false
	}
	tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cfg.Cert}})
	tc.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tc.Handshake(); err != nil {
		return nil, false
	}
	tc.SetDeadline(time.Time{})
	switch sub {
	case 260:
		return tc, true
	case 261:
		return tc, vncAuth(tc, cfg.Password)
	case 262:
		var l [8]byte
		if _, err := io.ReadFull(tc, l[:]); err != nil {
			return nil, false
		}
		up := make([]byte, binary.BigEndian.Uint32(l[:4])+binary.BigEndian.Uint32(l[4:]))
		if _, err := io.ReadFull(tc, up); err != nil {
			return nil, false
		}
		return tc, string(up) == cfg.User+cfg.Password
	}
	return nil, false
}

// Page is the page's end of a GoStream for tests: Feed hands the relay a
// frame, Read waits for bytes it sent, and Sent is everything it sent.
type Page struct {
	in     chan []byte
	mu     sync.Mutex
	sent   []byte
	more   chan struct{}
	closed chan struct{}
	once   sync.Once
}

func NewPage() *Page {
	return &Page{in: make(chan []byte, 16), more: make(chan struct{}, 1), closed: make(chan struct{})}
}

func (p *Page) Receive() ([]byte, error) {
	select {
	case b := <-p.in:
		return b, nil
	case <-p.closed:
		return nil, io.EOF
	}
}

func (p *Page) Send(b []byte) error {
	select {
	case <-p.closed:
		return io.EOF
	default:
	}
	p.mu.Lock()
	p.sent = append(p.sent, b...)
	p.mu.Unlock()
	select {
	case p.more <- struct{}{}:
	default:
	}
	return nil
}

// Feed queues a frame from the page.
func (p *Page) Feed(b []byte) { p.in <- b }

// Close is the page going away.
func (p *Page) Close() { p.once.Do(func() { close(p.closed) }) }

// Sent is every byte sent to the page so far.
func (p *Page) Sent() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.sent...)
}

// Read waits up to five seconds for the page to have been sent n bytes in
// all, and returns bytes [from, n).
func (p *Page) Read(t testing.TB, from, n int) []byte {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if s := p.Sent(); len(s) >= n {
			return s[from:n]
		}
		select {
		case <-p.more:
		case <-deadline:
			t.Fatalf("the page got %q, want %d bytes", p.Sent(), n)
		}
	}
}
