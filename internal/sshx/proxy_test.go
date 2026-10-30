package sshx

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// A VPN proxy looks up its helper's SOCKS endpoint and login at each dial,
// so a shared connection rebuilt after the VPN reconnected on a new port
// dials the new one (KTD2).
func TestVPNProxyDialsTheLiveSOCKSEndpoint(t *testing.T) {
	s := newTestServer(t, serverOpts{})
	a := proxytest.SOCKS5(t, "u1", "p1")
	b := proxytest.SOCKS5(t, "u2", "p2")
	var mu sync.Mutex
	live, user, pass := a, "u1", "p1"
	hop := s.hop()
	hop.Proxy = &Proxy{Type: "vpn", Host: "office", VPN: "v1", Socks: func() (string, string, string, error) {
		mu.Lock()
		defer mu.Unlock()
		return net.JoinHostPort(live.Host, strconv.Itoa(live.Port)), user, pass, nil
	}}
	m := NewManager(&Dialer{KnownHostsPath: writeKnownHosts(t, s.knownLine(t))})
	ctx := context.Background()
	l, err := m.Acquire(ctx, Target{Key: "k", Hops: []Hop{hop}})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	want := net.JoinHostPort(s.host, strconv.Itoa(s.port))
	if !slices.Equal(a.Targets(), []string{want}) {
		t.Fatalf("first endpoint carried %v, want %s", a.Targets(), want)
	}

	mu.Lock()
	live, user, pass = b, "u2", "p2"
	mu.Unlock()
	s.dropAll()
	cl, err := l.Reconnect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := run(t, cl, "echo hi"); got != "hi\n" {
		t.Fatalf("exec = %q", got)
	}
	if !slices.Equal(b.Targets(), []string{want}) || len(a.Targets()) != 1 {
		t.Fatalf("after the reconnect: first carried %v, second %v", a.Targets(), b.Targets())
	}
}

// A VPN that is down, or whose endpoint fails, fails the connect with a VPN
// code naming the profile, never ProxyFailed, and the target never sees a
// direct connection (KTD2, KTD22).
func TestFailedVPNNeverFallsBackToDirect(t *testing.T) {
	downHost, downPort := proxytest.Down(t)
	refusing := proxytest.SOCKS5(t, "u", "p")
	for _, tc := range []struct {
		name  string
		socks func() (string, string, string, error)
		code  apperr.Code
	}{
		{"not connected", func() (string, string, string, error) {
			return "", "", "", apperr.New(apperr.VPNDown, "VPN office is not connected").With("vpnId", "v1")
		}, apperr.VPNDown},
		{"endpoint gone", func() (string, string, string, error) {
			return net.JoinHostPort(downHost, strconv.Itoa(downPort)), "u", "p", nil
		}, apperr.VPNFailed},
		{"stale login", func() (string, string, string, error) {
			return net.JoinHostPort(refusing.Host, strconv.Itoa(refusing.Port)), "u", "old", nil
		}, apperr.VPNFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, serverOpts{})
			hop := s.hop()
			hop.Proxy = &Proxy{Type: "vpn", Host: "office", VPN: "v1", Socks: tc.socks}
			d := &Dialer{KnownHostsPath: writeKnownHosts(t, s.knownLine(t))}
			_, err := d.Dial(context.Background(), Target{Key: "k", Hops: []Hop{hop}})
			var ae *apperr.Error
			if !errors.As(err, &ae) || ae.Code != tc.code || ae.Details["vpnId"] != "v1" || !strings.Contains(err.Error(), "office") {
				t.Fatalf("err = %v (%+v), want %s naming the profile", err, ae, tc.code)
			}
			if strings.Contains(err.Error(), strconv.Itoa(downPort)) || strings.Contains(err.Error(), strconv.Itoa(refusing.Port)) {
				t.Fatalf("err = %v names the helper's port", err)
			}
			if n := s.conns.Load(); n != 0 {
				t.Fatalf("target saw %d direct connections", n)
			}
		})
	}
}

// A system-wide profile's dial goes to exactly the address its Route
// checked, from the tunnel's address, never to addr resolved again: that
// could pick an address the tunnel does not carry (KTD22).
func TestRoutedVPNDialsOnlyTheCheckedAddress(t *testing.T) {
	checked, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer checked.Close()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	local := netip.MustParseAddr("127.0.0.1")
	var asked []string
	p := &Proxy{Type: "vpn", Host: "office", VPN: "v1", Route: func(_ context.Context, addr string) (netip.AddrPort, netip.Addr, error) {
		asked = append(asked, addr)
		return checked.Addr().(*net.TCPAddr).AddrPort(), local, nil
	}}
	c, err := DialTCP(context.Background(), p, "unchecked.invalid:22")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if c.RemoteAddr().String() != checked.Addr().String() || c.LocalAddr().(*net.TCPAddr).AddrPort().Addr() != local {
		t.Fatalf("dialed %s from %s, want %s from %s", c.RemoteAddr(), c.LocalAddr(), checked.Addr(), local)
	}
	// mosh's UDP takes the same path.
	p.Route = func(_ context.Context, addr string) (netip.AddrPort, netip.Addr, error) {
		asked = append(asked, addr)
		return pc.LocalAddr().(*net.UDPAddr).AddrPort(), local, nil
	}
	u, err := p.DialUDP(context.Background(), &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 60001})
	if err != nil {
		t.Fatal(err)
	}
	u.Close()
	if u.RemoteAddr().String() != pc.LocalAddr().String() {
		t.Fatalf("UDP dialed %s, want %s", u.RemoteAddr(), pc.LocalAddr())
	}
	if !slices.Equal(asked, []string{"unchecked.invalid:22", "192.0.2.1:60001"}) {
		t.Fatalf("Route asked about %v", asked)
	}
}
