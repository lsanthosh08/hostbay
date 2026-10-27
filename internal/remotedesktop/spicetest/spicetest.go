// Package spicetest runs an in-process SPICE server for tests. It answers
// each channel's link message with an RSA-1024 link key, as spice-server
// does, and accepts the ticket only when it decrypts to the password. After
// that it echoes every byte, so tests can see traffic pass both ways.
package spicetest

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
)

// Config is how the server answers.
type Config struct {
	Password  string
	Cert      tls.Certificate // when set, the port speaks TLS from the first byte, as a SPICE tls-port does
	LinkError uint32          // when set, every link is refused with this SPICE_LINK_ERR_* code
}

// Server is a running test server.
type Server struct {
	Host string
	Port int

	key *rsa.PrivateKey
	mu  sync.Mutex
	cfg Config
	log []string
	n   int
}

// New starts a plain server that accepts password and stops it when the
// test ends.
func New(t testing.TB, password string) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	key, err := rsa.GenerateKey(rand.Reader, 1024) // SPICE_TICKET_KEY_PAIR_LENGTH
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, key: key, cfg: Config{Password: password}}
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

// Log lists what happened, in order: "link T:I" for a channel's link
// message (type and ID), "ticket ok" or "ticket wrong", and "closed T:I"
// when a signed-in channel went away.
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
	var conn net.Conn = c
	if cfg.Cert.Certificate != nil {
		tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cfg.Cert}})
		if tc.Handshake() != nil {
			return
		}
		conn = tc
	}

	var hdr [16]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil || string(hdr[:4]) != "REDQ" {
		return
	}
	body := make([]byte, binary.LittleEndian.Uint32(hdr[12:]))
	if _, err := io.ReadFull(conn, body); err != nil || len(body) < 18 {
		return
	}
	ch := fmt.Sprintf("%d:%d", body[4], body[5])
	s.record("link " + ch)
	authSelection := false
	if nc, off := binary.LittleEndian.Uint32(body[6:]), binary.LittleEndian.Uint32(body[14:]); nc > 0 && int(off)+4 <= len(body) {
		authSelection = binary.LittleEndian.Uint32(body[off:])&1 != 0
	}

	pub, _ := x509.MarshalPKIXPublicKey(&s.key.PublicKey)
	reply := binary.LittleEndian.AppendUint32(nil, cfg.LinkError)
	reply = append(reply, pub...)                        // 162 bytes
	reply = binary.LittleEndian.AppendUint32(reply, 1)   // common caps
	reply = binary.LittleEndian.AppendUint32(reply, 0)   // channel caps
	reply = binary.LittleEndian.AppendUint32(reply, 178) // caps offset
	reply = binary.LittleEndian.AppendUint32(reply, 1<<0|1<<1|1<<3)
	out := append([]byte("REDQ"), 2, 0, 0, 0, 2, 0, 0, 0)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(reply)))
	if _, err := conn.Write(append(out, reply...)); err != nil || cfg.LinkError != 0 {
		return
	}

	if authSelection {
		var m [4]byte
		if _, err := io.ReadFull(conn, m[:]); err != nil || binary.LittleEndian.Uint32(m[:]) != 1 { // SPICE_COMMON_CAP_AUTH_SPICE
			return
		}
	}
	ticket := make([]byte, 128)
	if _, err := io.ReadFull(conn, ticket); err != nil {
		return
	}
	pw, err := rsa.DecryptOAEP(sha1.New(), nil, s.key, ticket, nil)
	if err != nil || !bytes.Equal(pw, append([]byte(cfg.Password), 0)) {
		s.record("ticket wrong")
		conn.Write([]byte{7, 0, 0, 0}) // SPICE_LINK_ERR_PERMISSION_DENIED
		return
	}
	s.record("ticket ok")
	conn.Write([]byte{0, 0, 0, 0})
	io.Copy(conn, conn)
	s.record("closed " + ch)
}
