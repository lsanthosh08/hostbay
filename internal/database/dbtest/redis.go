package dbtest

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Redis is a Redis server holding Keys string keys named k000000 and up,
// served by SCAN. It refuses HELLO as Redis before 6 does, so a client
// signs in with AUTH; it accepts any password. Replies answers other
// commands by name with a raw RESP reply. It records every command and
// the first byte each connection sent.
type Redis struct {
	Host string
	Port int
	Keys int
	// Replies maps a command line, as "GET k", to its raw reply.
	Replies map[string]string

	tls *tls.Config

	mu    sync.Mutex
	conns int
	cmds  [][]string
	first []byte
}

// NewRedis serves until the test ends; with a certificate it speaks TLS.
func NewRedis(t testing.TB, cert *tls.Certificate) *Redis {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &Redis{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, Replies: map[string]string{}}
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

// SetCert changes the certificate later connections are served.
func (s *Redis) SetCert(cert *tls.Certificate) {
	var cfg *tls.Config
	if cert != nil {
		cfg = &tls.Config{Certificates: []tls.Certificate{*cert}}
	}
	s.mu.Lock()
	s.tls = cfg
	s.mu.Unlock()
}

func (s *Redis) Addr() string { return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) }

func (s *Redis) Conns() int { s.mu.Lock(); defer s.mu.Unlock(); return s.conns }

// Commands are the commands received, each as its arguments.
func (s *Redis) Commands() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.cmds...)
}

// FirstBytes holds each connection's first byte, in order.
func (s *Redis) FirstBytes() []byte { s.mu.Lock(); defer s.mu.Unlock(); return bytes.Clone(s.first) }

func (s *Redis) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	b, err := br.Peek(1)
	s.mu.Lock()
	s.conns++
	if err == nil {
		s.first = append(s.first, b[0])
	}
	cfg := s.tls
	s.mu.Unlock()
	var rw io.ReadWriter = struct {
		io.Reader
		io.Writer
	}{br, c}
	if cfg != nil {
		tc := tls.Server(&peeked{Conn: c, r: br}, cfg)
		if tc.Handshake() != nil {
			return
		}
		rw = tc
		br = bufio.NewReader(tc)
	}
	for {
		args, err := readRESP(br)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.cmds = append(s.cmds, args)
		s.mu.Unlock()
		if _, err := io.WriteString(rw, s.reply(args)); err != nil {
			return
		}
	}
}

// peeked is a connection whose first bytes were read into r.
type peeked struct {
	net.Conn
	r *bufio.Reader
}

func (p *peeked) Read(b []byte) (int, error) { return p.r.Read(b) }

func (s *Redis) reply(args []string) string {
	if r, ok := s.Replies[strings.Join(args, " ")]; ok {
		return r
	}
	switch strings.ToUpper(args[0]) {
	case "HELLO":
		return "-ERR unknown command 'HELLO'\r\n"
	case "AUTH", "SELECT":
		return "+OK\r\n"
	case "PING":
		return "+PONG\r\n"
	case "TYPE":
		return "+string\r\n"
	case "SCAN":
		cur, _ := strconv.Atoi(args[1])
		n := 10
		for i := 2; i+1 < len(args); i++ {
			if strings.EqualFold(args[i], "COUNT") {
				n, _ = strconv.Atoi(args[i+1])
			}
		}
		end := min(cur+n, s.Keys)
		next := end
		if end == s.Keys {
			next = 0
		}
		out := fmt.Sprintf("*2\r\n$%d\r\n%d\r\n*%d\r\n", len(strconv.Itoa(next)), next, end-cur)
		for i := cur; i < end; i++ {
			k := fmt.Sprintf("k%06d", i)
			out += fmt.Sprintf("$%d\r\n%s\r\n", len(k), k)
		}
		return out
	}
	return "-ERR unknown command '" + args[0] + "'\r\n"
}

// readRESP reads one command: an array of bulk strings.
func readRESP(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("not an array: %q", line)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(line[1:]))
	args := make([]string, n)
	for i := range args {
		h, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		l, _ := strconv.Atoi(strings.TrimSpace(h[1:]))
		b := make([]byte, l+2)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		args[i] = string(b[:l])
	}
	return args, nil
}
