// Package proxytest runs in-process SOCKS5 and HTTP CONNECT proxies for
// tests. Each records the targets clients asked it to reach.
package proxytest

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
)

// Server is a running test proxy.
type Server struct {
	Host string
	Port int

	mu      sync.Mutex
	targets []string
}

// Targets lists the host:port values clients asked for, unresolved.
func (s *Server) Targets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.targets...)
}

func (s *Server) record(target string) {
	s.mu.Lock()
	s.targets = append(s.targets, target)
	s.mu.Unlock()
}

func listen(t testing.TB, serve func(net.Conn)) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(c)
		}
	}()
	return &Server{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port}
}

// SOCKS5 serves RFC 1928 CONNECT. With a user it requires RFC 1929
// username and password authentication.
func SOCKS5(t testing.TB, user, pass string) *Server {
	var s *Server
	s = listen(t, func(c net.Conn) {
		defer c.Close()
		r := bufio.NewReader(c)
		var hdr [2]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil || hdr[0] != 5 {
			return
		}
		methods := make([]byte, hdr[1])
		if _, err := io.ReadFull(r, methods); err != nil {
			return
		}
		if user == "" {
			c.Write([]byte{5, 0})
		} else {
			c.Write([]byte{5, 2})
			var v [2]byte
			if _, err := io.ReadFull(r, v[:]); err != nil {
				return
			}
			u := make([]byte, v[1])
			io.ReadFull(r, u)
			var pl [1]byte
			io.ReadFull(r, pl[:])
			p := make([]byte, pl[0])
			io.ReadFull(r, p)
			if string(u) != user || string(p) != pass {
				c.Write([]byte{1, 1})
				return
			}
			c.Write([]byte{1, 0})
		}
		var req [4]byte
		if _, err := io.ReadFull(r, req[:]); err != nil || req[1] != 1 {
			return
		}
		var host string
		switch req[3] {
		case 1:
			ip := make([]byte, 4)
			io.ReadFull(r, ip)
			host = net.IP(ip).String()
		case 3:
			var n [1]byte
			io.ReadFull(r, n[:])
			name := make([]byte, n[0])
			io.ReadFull(r, name)
			host = string(name)
		default:
			return
		}
		var port [2]byte
		io.ReadFull(r, port[:])
		target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:]))))
		s.record(target)
		out, err := net.Dial("tcp4", target) // test servers listen on 127.0.0.1
		if err != nil {
			c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
			return
		}
		defer out.Close()
		c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
		pipe(&readConn{c, r}, out)
	})
	return s
}

// HTTP serves CONNECT. With a user it answers 407 unless Basic credentials
// match. A non-zero status is sent instead of connecting.
func HTTP(t testing.TB, user, pass string, status int) *Server {
	var s *Server
	s = listen(t, func(c net.Conn) {
		defer c.Close()
		r := bufio.NewReader(c)
		req, err := http.ReadRequest(r)
		if err != nil || req.Method != http.MethodConnect {
			return
		}
		s.record(req.RequestURI)
		code := status
		if user != "" && req.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)) {
			code = http.StatusProxyAuthRequired
		}
		if code != 0 {
			io.WriteString(c, "HTTP/1.1 "+strconv.Itoa(code)+" "+http.StatusText(code)+"\r\n\r\n")
			return
		}
		out, err := net.Dial("tcp4", req.RequestURI)
		if err != nil {
			io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
			return
		}
		defer out.Close()
		io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
		pipe(&readConn{c, r}, out)
	})
	return s
}

// readConn reads what the handshake's reader already buffered first.
type readConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *readConn) Read(b []byte) (int, error) { return c.r.Read(b) }

func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
	a.Close()
	b.Close()
	<-done
}

// Down returns the address of a port nothing listens on.
func Down(t testing.TB) (string, int) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return "127.0.0.1", port
}
