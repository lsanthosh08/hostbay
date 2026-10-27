// Package dbtest serves fake database servers for tests of the database
// clients' sign-in: what reached the server, and what never did.
package dbtest

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
)

// PG is a PostgreSQL server that speaks only the sign-in: it answers an
// SSL request (with TLS when it has a certificate), reads the startup
// message, asks for a cleartext password and then fails the sign-in with
// Code and Message. It records what each client sent.
type PG struct {
	Host string
	Port int

	tls *tls.Config

	mu          sync.Mutex
	code, msg   string
	conns       int
	sslRequests int
	startups    []map[string]string
	passwords   []string
	clientCerts int
}

// NewPG serves until the test ends. With a certificate it speaks TLS and
// asks for (but does not need) a client certificate.
func NewPG(t testing.TB, cert *tls.Certificate) *PG {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &PG{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, code: "28P01", msg: "password authentication failed"}
	s.SetCert(cert)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

// SetCert changes the certificate later connections are served, as a
// server restarted with a new one would; nil turns TLS off.
func (s *PG) SetCert(cert *tls.Certificate) {
	var cfg *tls.Config
	if cert != nil {
		cfg = &tls.Config{Certificates: []tls.Certificate{*cert}, ClientAuth: tls.RequestClientCert}
	}
	s.mu.Lock()
	s.tls = cfg
	s.mu.Unlock()
}

// Addr is the server's host:port.
func (s *PG) Addr() string { return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) }

// Fail sets the error the sign-in ends with, such as 3D000 for a database
// that does not exist.
func (s *PG) Fail(code, msg string) {
	s.mu.Lock()
	s.code, s.msg = code, msg
	s.mu.Unlock()
}

// Conns counts TCP connections; SSLRequests the ones that asked for TLS.
func (s *PG) Conns() int       { s.mu.Lock(); defer s.mu.Unlock(); return s.conns }
func (s *PG) SSLRequests() int { s.mu.Lock(); defer s.mu.Unlock(); return s.sslRequests }

// Startups are the startup messages' parameters, in order.
func (s *PG) Startups() []map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]string(nil), s.startups...)
}

// Passwords are the password messages received.
func (s *PG) Passwords() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.passwords...)
}

// ClientCerts counts handshakes in which the client sent a certificate.
func (s *PG) ClientCerts() int { s.mu.Lock(); defer s.mu.Unlock(); return s.clientCerts }

func (s *PG) serve(c net.Conn) {
	defer c.Close()
	s.mu.Lock()
	s.conns++
	s.mu.Unlock()
	var conn net.Conn = c
	r := bufio.NewReader(conn)
	msg, err := readStartup(r)
	if err != nil {
		return
	}
	if len(msg) == 4 && binary.BigEndian.Uint32(msg) == 80877103 { // SSLRequest
		s.mu.Lock()
		s.sslRequests++
		cfg := s.tls
		s.mu.Unlock()
		if cfg == nil {
			c.Write([]byte("N"))
		} else {
			c.Write([]byte("S"))
			tc := tls.Server(c, cfg)
			if tc.Handshake() != nil {
				return
			}
			if len(tc.ConnectionState().PeerCertificates) > 0 {
				s.mu.Lock()
				s.clientCerts++
				s.mu.Unlock()
			}
			conn = tc
		}
		r = bufio.NewReader(conn)
		if msg, err = readStartup(r); err != nil {
			return
		}
	}
	if len(msg) < 4 {
		return
	}
	params := map[string]string{}
	kv := splitZ(msg[4:])
	for i := 0; i+1 < len(kv); i += 2 {
		params[kv[i]] = kv[i+1]
	}
	s.mu.Lock()
	s.startups = append(s.startups, params)
	code, text := s.code, s.msg
	s.mu.Unlock()
	conn.Write([]byte{'R', 0, 0, 0, 8, 0, 0, 0, 3}) // AuthenticationCleartextPassword
	typ, body, err := readMessage(r)
	if err != nil || typ != 'p' {
		return
	}
	s.mu.Lock()
	s.passwords = append(s.passwords, string(body[:max(len(body)-1, 0)]))
	s.mu.Unlock()
	var e []byte
	for _, f := range [][2]string{{"S", "FATAL"}, {"V", "FATAL"}, {"C", code}, {"M", text}} {
		e = append(append(append(e, f[0][0]), f[1]...), 0)
	}
	e = append(e, 0)
	out := []byte{'E', 0, 0, 0, 0}
	binary.BigEndian.PutUint32(out[1:], uint32(len(e)+4))
	conn.Write(append(out, e...))
}

// readStartup reads a length-prefixed message without a type byte.
func readStartup(r io.Reader) ([]byte, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	l := binary.BigEndian.Uint32(n[:])
	if l < 4 || l > 1<<16 {
		return nil, io.ErrUnexpectedEOF
	}
	b := make([]byte, l-4)
	_, err := io.ReadFull(r, b)
	return b, err
}

// readMessage reads a typed message.
func readMessage(r io.Reader) (byte, []byte, error) {
	var t [1]byte
	if _, err := io.ReadFull(r, t[:]); err != nil {
		return 0, nil, err
	}
	b, err := readStartup(r)
	return t[0], b, err
}

func splitZ(b []byte) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == 0 {
			if i == start {
				break
			}
			out = append(out, string(b[start:i]))
			start = i + 1
		}
	}
	return out
}
