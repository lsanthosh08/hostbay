package sshx

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"

	"hostbay/internal/apperr"
	"hostbay/internal/sshx/proxytest"
)

// SSH reaches the test server through each proxy type, and the proxy is
// handed the host name to resolve, not an address resolved here.
func TestSSHThroughSOCKS5AndHTTPProxies(t *testing.T) {
	for _, tc := range []struct {
		name string
		srv  func() *proxytest.Server
		typ  string
	}{
		{"socks5", func() *proxytest.Server { return proxytest.SOCKS5(t, "pu", "pp") }, "socks5"},
		{"http", func() *proxytest.Server { return proxytest.HTTP(t, "pu", "pp", 0) }, "http"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, serverOpts{})
			p := tc.srv()
			hop := s.hop()
			hop.Host = "localhost"
			hop.Proxy = &Proxy{Type: tc.typ, Host: p.Host, Port: p.Port, User: "pu", Password: "pp"}
			d := &Dialer{KnownHostsPath: writeKnownHosts(t, khLine("localhost", s.port, s.hostKeys[0].PublicKey()))}
			c, err := d.Dial(context.Background(), Target{Key: "k", Hops: []Hop{hop}})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if got := run(t, c, "echo hi"); got != "hi\n" {
				t.Fatalf("exec = %q", got)
			}
			if want := "localhost:" + strconv.Itoa(s.port); !slices.Equal(p.Targets(), []string{want}) {
				t.Fatalf("proxy was asked for %v, want %s", p.Targets(), want)
			}
		})
	}
}

// A proxy that is down or refuses fails the connect at the proxy step, and
// the target never sees a direct connection (KTD22).
func TestFailedProxyNeverFallsBackToDirect(t *testing.T) {
	downHost, downPort := proxytest.Down(t)
	for _, tc := range []struct {
		name  string
		proxy func() *Proxy
		want  string
	}{
		{"down", func() *Proxy { return &Proxy{Type: "socks5", Host: downHost, Port: downPort} }, ""},
		{"socks refuses credentials", func() *Proxy {
			p := proxytest.SOCKS5(t, "pu", "pp")
			return &Proxy{Type: "socks5", Host: p.Host, Port: p.Port, User: "pu", Password: "wrong"}
		}, ""},
		{"http 407", func() *Proxy {
			p := proxytest.HTTP(t, "pu", "pp", 0)
			return &Proxy{Type: "http", Host: p.Host, Port: p.Port}
		}, "407"},
		{"http 502", func() *Proxy {
			p := proxytest.HTTP(t, "", "", 502)
			return &Proxy{Type: "http", Host: p.Host, Port: p.Port}
		}, "502"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, serverOpts{})
			hop := s.hop()
			hop.Proxy = tc.proxy()
			d := &Dialer{KnownHostsPath: writeKnownHosts(t, s.knownLine(t))}
			_, err := d.Dial(context.Background(), Target{Key: "k", Hops: []Hop{hop}})
			var ae *apperr.Error
			if !errors.As(err, &ae) || ae.Code != apperr.ProxyFailed || ae.Details["step"] != "proxy" {
				t.Fatalf("err = %v, want ProxyFailed at the proxy step", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to name %q", err, tc.want)
			}
			if n := s.conns.Load(); n != 0 {
				t.Fatalf("target saw %d direct connections", n)
			}
		})
	}
}

// The HTTP CONNECT target cannot smuggle extra request lines.
func TestHTTPProxyRefusesCRLFInTarget(t *testing.T) {
	p := proxytest.HTTP(t, "", "", 0)
	_, err := DialTCP(context.Background(), &Proxy{Type: "http", Host: p.Host, Port: p.Port}, "a\r\nX-Evil: 1:22")
	if apperr.CodeOf(err) != apperr.ProxyFailed || len(p.Targets()) != 0 {
		t.Fatalf("err = %v, proxy saw %v", err, p.Targets())
	}
}

// Bytes the server sends right after the proxy's 200 are not lost.
func TestHTTPProxyKeepsBytesSentWithTheReply(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 512)
		c.Read(buf)
		io.WriteString(c, "HTTP/1.1 200 OK\r\nVia: test\r\n\r\nSSH-2.0-banner\r\n")
		c.Close()
	}()
	a := ln.Addr().(*net.TCPAddr)
	c, err := DialTCP(context.Background(), &Proxy{Type: "http", Host: "127.0.0.1", Port: a.Port}, "target:22")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got, _ := io.ReadAll(c)
	if string(got) != "SSH-2.0-banner\r\n" {
		t.Fatalf("read %q", got)
	}
	if _, ok := c.RemoteAddr().(*net.TCPAddr); !ok {
		t.Fatal("RemoteAddr is not a TCP address; FTP needs one")
	}
}
