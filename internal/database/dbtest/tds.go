package dbtest

import (
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"unicode/utf16"
)

// TDS is a SQL Server that speaks only the sign-in. With a certificate it
// speaks TDS 8, TLS first; without one it says it cannot encrypt. It counts
// LOGIN7 packets, which carry the password, and fails each sign-in, or,
// after Route, accepts it and routes the client to another address.
type TDS struct {
	Host string
	Port int

	mu     sync.Mutex
	tls    *tls.Config
	route  string
	conns  int
	logins int
}

// NewTDS serves until the test ends.
func NewTDS(t testing.TB, cert *tls.Certificate) *TDS {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &TDS{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port}
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

// SetCert changes the certificate later connections get, as a restarted
// server with a new self-generated one would; nil turns TLS off.
func (s *TDS) SetCert(cert *tls.Certificate) {
	var cfg *tls.Config
	if cert != nil {
		cfg = &tls.Config{Certificates: []tls.Certificate{*cert}, NextProtos: []string{"tds/8.0"}}
	}
	s.mu.Lock()
	s.tls = cfg
	s.mu.Unlock()
}

// Route makes the server accept sign-ins and route them to host:port.
func (s *TDS) Route(host string, port int) {
	s.mu.Lock()
	s.route = net.JoinHostPort(host, strconv.Itoa(port))
	s.mu.Unlock()
}

// Addr is the server's host:port.
func (s *TDS) Addr() string { return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) }

// Conns counts TCP connections; Logins the LOGIN7 packets received.
func (s *TDS) Conns() int  { s.mu.Lock(); defer s.mu.Unlock(); return s.conns }
func (s *TDS) Logins() int { s.mu.Lock(); defer s.mu.Unlock(); return s.logins }

func (s *TDS) serve(c net.Conn) {
	defer c.Close()
	s.mu.Lock()
	s.conns++
	cfg, route := s.tls, s.route
	s.mu.Unlock()
	var conn net.Conn = c
	if cfg != nil {
		tc := tls.Server(c, cfg)
		if tc.Handshake() != nil {
			return
		}
		conn = tc
	}
	if typ, err := readTDS(conn); err != nil || typ != 0x12 { // PRELOGIN
		return
	}
	// ENCRYPTION is NOT_SUP; under TDS 8 the TLS is already there.
	if writeTDS(conn, []byte{0x01, 0, 6, 0, 1, 0xff, 0x02}) != nil {
		return
	}
	if typ, err := readTDS(conn); err != nil || typ != 0x10 { // LOGIN7
		return
	}
	s.mu.Lock()
	s.logins++
	s.mu.Unlock()
	var out []byte
	if route == "" {
		msg := ucs2("Login failed for user 'app'.")
		e := binary.LittleEndian.AppendUint32(nil, 18456)
		e = append(e, 1, 14)
		e = binary.LittleEndian.AppendUint16(e, uint16(len(msg)/2))
		e = append(e, msg...)
		e = append(e, 0, 0, 0, 0, 0, 0) // server and procedure names, line number
		out = append(binary.LittleEndian.AppendUint16([]byte{0xaa}, uint16(len(e))), e...)
		out = append(out, done(2)...)
	} else {
		host, p, _ := net.SplitHostPort(route)
		port, _ := strconv.Atoi(p)
		prog := ucs2("fake")
		ack := append([]byte{1, 0x74, 0, 0, 4, byte(len(prog) / 2)}, prog...)
		ack = append(ack, 16, 0, 0, 0)
		out = append(binary.LittleEndian.AppendUint16([]byte{0xad}, uint16(len(ack))), ack...)
		name := ucs2(host)
		v := binary.LittleEndian.AppendUint16([]byte{0}, uint16(port))
		v = binary.LittleEndian.AppendUint16(v, uint16(len(name)/2))
		v = append(v, name...)
		env := binary.LittleEndian.AppendUint16([]byte{20}, uint16(len(v)))
		env = append(append(env, v...), 0, 0)
		out = append(out, 0xe3)
		out = binary.LittleEndian.AppendUint16(out, uint16(len(env)))
		out = append(out, env...)
		out = append(out, done(0)...)
	}
	writeTDS(conn, out)
	io.Copy(io.Discard, conn)
}

// done is a DONE token with status.
func done(status uint16) []byte {
	b := binary.LittleEndian.AppendUint16([]byte{0xfd}, status)
	return append(b, make([]byte, 10)...)
}

func ucs2(s string) []byte {
	var b []byte
	for _, r := range utf16.Encode([]rune(s)) {
		b = binary.LittleEndian.AppendUint16(b, r)
	}
	return b
}

// readTDS reads one message, which may span packets, and returns its type.
func readTDS(r io.Reader) (byte, error) {
	for {
		var h [8]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return 0, err
		}
		n := int(binary.BigEndian.Uint16(h[2:]))
		if n < 8 {
			return 0, io.ErrUnexpectedEOF
		}
		if _, err := io.CopyN(io.Discard, r, int64(n-8)); err != nil {
			return 0, err
		}
		if h[1]&1 != 0 {
			return h[0], nil
		}
	}
}

// writeTDS sends body as one tabular-result packet.
func writeTDS(w io.Writer, body []byte) error {
	h := []byte{0x04, 0x01, 0, 0, 0, 0, 1, 0}
	binary.BigEndian.PutUint16(h[2:], uint16(len(body)+8))
	_, err := w.Write(append(h, body...))
	return err
}
