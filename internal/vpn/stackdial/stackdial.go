// Package stackdial dials through a userspace network stack, resolving
// names with the stack's own DNS servers and search domains. Only
// internal/vpn/wg and internal/vpn/tunfd import it, which keeps gVisor out
// of the main binary.
package stackdial

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"golang.zx2c4.com/wireguard/tun/netstack"
)

// Dialer dials through Net, trying names with the Search domains.
type Dialer struct {
	Net    *netstack.Net
	Search []string
}

// Dial opens a TCP connection, or a connected UDP socket for network "udp",
// through the stack. Names resolve through the stack's DNS servers with its
// search domains, never the system resolver.
func (d Dialer) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("port %q is not a number", port)
	}
	addr, err := d.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	to := netip.AddrPortFrom(addr, uint16(p))
	switch network {
	case "tcp":
		return d.Net.DialContextTCPAddrPort(ctx, to)
	case "udp":
		return d.Net.DialUDPAddrPort(netip.AddrPort{}, to)
	}
	return nil, fmt.Errorf("network %q is not supported", network)
}

func (d Dialer) resolve(ctx context.Context, host string) (netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return a, nil
	}
	var err error
	for _, name := range searchNames(host, d.Search) {
		var addrs []string
		if addrs, err = d.Net.LookupContextHost(ctx, name); err == nil {
			return netip.ParseAddr(addrs[0])
		}
	}
	return netip.Addr{}, err
}

// searchNames lists the names to try for name, as a resolver with ndots 1
// does: a name with a dot is tried as given first, a single label after the
// search domains, and a name ending in a dot only as given.
func searchNames(name string, search []string) []string {
	if abs, ok := strings.CutSuffix(name, "."); ok {
		return []string{abs} // netstack adds the root dot itself
	}
	var names []string
	for _, d := range search {
		names = append(names, name+"."+d)
	}
	if strings.Contains(name, ".") {
		return append([]string{name}, names...)
	}
	return append(names, name)
}
