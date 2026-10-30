package sshx

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"

	"hostbay/internal/apperr"
)

// loopback is this computer's loopback interface: lo0 on macOS, lo on Linux.
func loopback(t *testing.T) string {
	t.Helper()
	ifs, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagLoopback != 0 && ifi.Flags&net.FlagUp != 0 {
			return ifi.Name
		}
	}
	t.Skip("no loopback interface")
	return ""
}

// external is an external profile's dial path pinned to iface (KTD7).
func external(iface string, dns ...string) *Proxy {
	return &Proxy{Type: "vpn", Host: "Office SASE", VPN: "v1", Iface: func(context.Context) (string, []string, error) { return iface, dns, nil }}
}

func echoListener(t *testing.T) net.Listener {
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
			c.Write([]byte("hi"))
			c.Close()
		}
	}()
	return ln
}

func readHi(t *testing.T, c net.Conn) {
	t.Helper()
	defer c.Close()
	b := make([]byte, 2)
	if _, err := c.Read(b); err != nil || string(b) != "hi" {
		t.Fatalf("read %q, %v", b, err)
	}
}

func TestBoundDialReachesAListenerOnItsInterface(t *testing.T) {
	ln := echoListener(t)
	c, err := DialTCP(context.Background(), external(loopback(t)), ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	readHi(t, c)
}

func TestBoundDialOnAMissingInterfaceFailsNamingTheProfile(t *testing.T) {
	ln := echoListener(t)
	_, err := DialTCP(context.Background(), external("hostbay-none0"), ln.Addr().String())
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.VPNFailed || ae.Details["vpnId"] != "v1" || !strings.Contains(ae.Message, "Office SASE") {
		t.Fatalf("err = %v, want VPNFailed naming Office SASE", err)
	}
}

func TestBoundDialResolvesThroughTheProfilesDNS(t *testing.T) {
	ln := echoListener(t)
	asked := make(chan string, 4)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go serveDNS(pc, asked)
	old := dnsPort
	_, dnsPort, _ = net.SplitHostPort(pc.LocalAddr().String())
	t.Cleanup(func() { dnsPort = old })

	_, port, _ := net.SplitHostPort(ln.Addr().String())
	// The name exists only in the profile's DNS.
	c, err := DialTCP(context.Background(), external(loopback(t), "127.0.0.1"), net.JoinHostPort("db.hostbay.test", port))
	if err != nil {
		t.Fatal(err)
	}
	readHi(t, c)
	if q := <-asked; q != "db.hostbay.test." {
		t.Fatalf("asked %q", q)
	}
}

func TestBoundUDPGoesOutItsInterface(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		b := make([]byte, 16)
		n, from, err := pc.ReadFrom(b)
		if err == nil {
			pc.WriteTo(b[:n], from)
		}
	}()
	c, err := external(loopback(t)).DialUDP(context.Background(), pc.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("hi"))
	b := make([]byte, 2)
	if _, err := c.Read(b); err != nil || string(b) != "hi" {
		t.Fatalf("read %q, %v", b, err)
	}
}

// serveDNS answers every A question with 127.0.0.1 and AAAA with nothing,
// sending each name asked on asked.
func serveDNS(pc net.PacketConn, asked chan<- string) {
	b := make([]byte, 512)
	for {
		n, from, err := pc.ReadFrom(b)
		if err != nil {
			return
		}
		var m dnsmessage.Message
		if m.Unpack(b[:n]) != nil || len(m.Questions) != 1 {
			continue
		}
		q := m.Questions[0]
		m.Header.Response, m.Header.Authoritative = true, true
		if q.Type == dnsmessage.TypeA {
			select {
			case asked <- q.Name.String():
			default:
			}
			m.Answers = []dnsmessage.Resource{{
				Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
				Body:   &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}},
			}}
		}
		if out, err := m.Pack(); err == nil {
			pc.WriteTo(out, from)
		}
	}
}
