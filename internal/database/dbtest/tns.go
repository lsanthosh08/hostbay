package dbtest

import (
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
)

// TNS is an Oracle listener that answers every connect packet with a
// redirect, by default to its own address over plain TCP. With a
// certificate it speaks TCPS. It counts the connect packets received.
type TNS struct {
	Host string
	Port int

	mu       sync.Mutex
	tls      *tls.Config
	redirect string
	connects int
}

// SetRedirect changes the connect descriptor later redirects name.
func (s *TNS) SetRedirect(descriptor string) {
	s.mu.Lock()
	s.redirect = descriptor
	s.mu.Unlock()
}

// NewTNS serves until the test ends.
func NewTNS(t testing.TB, cert *tls.Certificate) *TNS {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &TNS{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port}
	s.redirect = "(DESCRIPTION=(ADDRESS=(PROTOCOL=TCP)(HOST=" + s.Host + ")(PORT=" + strconv.Itoa(s.Port) + ")))"
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

// SetCert changes the certificate later connections get; nil is plain TCP.
func (s *TNS) SetCert(cert *tls.Certificate) {
	var cfg *tls.Config
	if cert != nil {
		cfg = &tls.Config{Certificates: []tls.Certificate{*cert}}
	}
	s.mu.Lock()
	s.tls = cfg
	s.mu.Unlock()
}

// Connects counts the connect packets received.
func (s *TNS) Connects() int { s.mu.Lock(); defer s.mu.Unlock(); return s.connects }

func (s *TNS) serve(c net.Conn) {
	defer c.Close()
	s.mu.Lock()
	cfg, redirect := s.tls, s.redirect
	s.mu.Unlock()
	var conn net.Conn = c
	if cfg != nil {
		tc := tls.Server(c, cfg)
		if tc.Handshake() != nil {
			return
		}
		conn = tc
	}
	var h [8]byte
	if _, err := io.ReadFull(conn, h[:]); err != nil || h[4] != 1 { // CONNECT
		return
	}
	s.mu.Lock()
	s.connects++
	s.mu.Unlock()
	out := make([]byte, 10, 10+len(redirect))
	binary.BigEndian.PutUint16(out, uint16(10+len(redirect)))
	out[4] = 5 // REDIRECT
	binary.BigEndian.PutUint16(out[8:], uint16(len(redirect)))
	conn.Write(append(out, redirect...))
	io.Copy(io.Discard, conn)
}
