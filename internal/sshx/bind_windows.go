package sshx

import (
	"encoding/binary"
	"net"
	"syscall"

	"golang.org/x/sys/windows"
)

// IP_UNICAST_IF and IPV6_UNICAST_IF in ws2ipdef.h; x/sys/windows lacks them.
const unicastIF = 31

// bindTo pins a socket to ifi with IP_UNICAST_IF, or IPV6_UNICAST_IF for
// IPv6, before it connects (KTD7). IPv4 takes the index in network byte
// order, IPv6 in host order.
func bindTo(ifi *net.Interface) func(network, address string, c syscall.RawConn) error {
	return func(network, _ string, c syscall.RawConn) error {
		var err error
		cerr := c.Control(func(fd uintptr) {
			if network == "tcp6" || network == "udp6" {
				err = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IPV6, unicastIF, ifi.Index)
			} else {
				var be [4]byte
				binary.BigEndian.PutUint32(be[:], uint32(ifi.Index))
				err = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, unicastIF, int(binary.NativeEndian.Uint32(be[:])))
			}
		})
		if cerr != nil {
			return cerr
		}
		return err
	}
}
