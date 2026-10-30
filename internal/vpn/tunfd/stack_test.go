//go:build !windows

package tunfd

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"slices"
	"syscall"
	"testing"
	"time"
)

// pair is a datagram socket pair like the one openconnect makes for
// --script-tun: the stack gets one end, the test plays openconnect on the
// other.
func pair(t *testing.T) (*os.File, net.Conn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fds[1]), "far")
	far, err := net.FileConn(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { far.Close() })
	return os.NewFile(uintptr(fds[0]), "vpnfd"), far
}

// udp4 is a raw IPv4 UDP packet, with no checksum on the UDP part (allowed
// for IPv4).
func udp4(src, dst netip.AddrPort, payload []byte) []byte {
	p := make([]byte, 28+len(payload))
	p[0], p[8], p[9] = 0x45, 64, 17
	binary.BigEndian.PutUint16(p[2:], uint16(len(p)))
	s, d := src.Addr().As4(), dst.Addr().As4()
	copy(p[12:], s[:])
	copy(p[16:], d[:])
	var sum uint32
	for i := 0; i < 20; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(p[i:]))
	}
	for sum > 0xffff {
		sum = sum&0xffff + sum>>16
	}
	binary.BigEndian.PutUint16(p[10:], ^uint16(sum))
	binary.BigEndian.PutUint16(p[20:], src.Port())
	binary.BigEndian.PutUint16(p[22:], dst.Port())
	binary.BigEndian.PutUint16(p[24:], uint16(8+len(payload)))
	copy(p[28:], payload)
	return p
}

func TestPacketsCrossTheSocket(t *testing.T) {
	vpnfd, far := pair(t)
	local := netip.MustParseAddr("10.9.0.2")
	s, err := New(vpnfd, []netip.Addr{local}, nil, nil, 1400)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	// A packet written to the socket reaches the stack.
	ln, err := s.net.ListenUDPAddrPort(netip.AddrPortFrom(local, 5300))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if _, err := far.Write(udp4(netip.MustParseAddrPort("10.9.0.1:4000"), netip.AddrPortFrom(local, 5300), []byte("ping"))); err != nil {
		t.Fatal(err)
	}
	ln.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, from, err := ln.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "ping" || from.String() != "10.9.0.1:4000" {
		t.Fatalf("stack read %q from %v: %v", buf[:n], from, err)
	}

	// A dial on the stack sends a packet out of the socket: the TCP SYN.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Dial(ctx, "tcp", "10.9.0.1:80")
	far.SetReadDeadline(time.Now().Add(5 * time.Second))
	pkt := make([]byte, 2048)
	n, err = far.Read(pkt)
	if err != nil {
		t.Fatal(err)
	}
	pkt = pkt[:n]
	if pkt[0]>>4 != 4 || pkt[9] != 6 || netip.AddrFrom4([4]byte(pkt[16:20])) != netip.MustParseAddr("10.9.0.1") ||
		binary.BigEndian.Uint16(pkt[22:]) != 80 || pkt[33]&0x02 == 0 {
		t.Fatalf("got packet % x, want a TCP SYN to 10.9.0.1:80", pkt)
	}
}

func TestSearchNames(t *testing.T) {
	for _, c := range []struct {
		name string
		want []string
	}{
		{"db", []string{"db.corp.example", "db"}},
		{"db.lan", []string{"db.lan", "db.lan.corp.example"}},
		{"db.lan.", []string{"db.lan"}},
	} {
		if got := searchNames(c.name, []string{"corp.example"}); !slices.Equal(got, c.want) {
			t.Errorf("searchNames(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}
