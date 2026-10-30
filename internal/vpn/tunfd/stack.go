// Package tunfd puts a gVisor network stack on a datagram socket that
// carries one raw IP packet per datagram, as openconnect's --script-tun
// VPNFD does (KTD3). The stack lives in hostbay-vpn's own process, so it
// changes no system route, DNS setting or interface (R14). Only the helpers
// (cmd/hostbay-vpn and cmd/hostbay-netd) import it, which keeps gVisor out
// of the main binary.
package tunfd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"

	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"hostbay/internal/vpn/stackdial"
)

// Stack is a network stack whose packets travel over one datagram socket.
type Stack struct {
	conn   net.Conn
	dev    tun.Device
	net    *netstack.Net
	search []string
	once   sync.Once
}

// New takes ownership of f, a datagram socket, and starts moving packets
// between it and a stack with the given addresses, DNS servers, search
// domains and MTU.
func New(f *os.File, addrs, dns []netip.Addr, search []string, mtu int) (*Stack, error) {
	conn, err := net.FileConn(f)
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("Cannot use the VPN packet socket: %w", err)
	}
	dev, n, err := netstack.CreateNetTUN(addrs, dns, mtu)
	if err != nil {
		conn.Close()
		return nil, err
	}
	s := &Stack{conn: conn, dev: dev, net: n, search: search}
	go s.in()
	go s.out()
	return s, nil
}

// in hands each datagram from the socket to the stack.
func (s *Stack) in() {
	defer s.Close()
	buf, bufs := make([]byte, 65535), [][]byte{nil}
	for {
		n, err := s.conn.Read(buf)
		if err != nil {
			return
		}
		bufs[0] = buf[:n]
		s.dev.Write(bufs, 0) // a packet that is not IP is dropped
	}
}

// out sends each packet the stack emits as one datagram.
func (s *Stack) out() {
	defer s.Close()
	bufs, sizes := [][]byte{make([]byte, 65535)}, []int{0}
	for {
		if _, err := s.dev.Read(bufs, sizes, 0); err != nil {
			return
		}
		if _, err := s.conn.Write(bufs[0][:sizes[0]]); err != nil && errors.Is(err, net.ErrClosed) {
			return
		}
	}
}

// Close stops the stack and closes the socket.
func (s *Stack) Close() error {
	s.once.Do(func() {
		s.conn.Close()
		s.dev.Close()
	})
	return nil
}

// Dial opens a TCP connection, or a connected UDP socket for network "udp",
// through the stack. Names resolve through the stack's DNS servers with its
// search domains, never the system resolver.
func (s *Stack) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	return stackdial.Dialer{Net: s.net, Search: s.search}.Dial(ctx, network, address)
}
