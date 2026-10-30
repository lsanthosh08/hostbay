package sshx

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// bindTo pins a socket to ifi with IP_BOUND_IF, or IPV6_BOUND_IF for IPv6,
// before it connects; it needs no admin rights (KTD7).
func bindTo(ifi *net.Interface) func(network, address string, c syscall.RawConn) error {
	return func(network, _ string, c syscall.RawConn) error {
		var err error
		cerr := c.Control(func(fd uintptr) {
			if network == "tcp6" || network == "udp6" {
				err = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, ifi.Index)
			} else {
				err = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, ifi.Index)
			}
		})
		if cerr != nil {
			return cerr
		}
		return err
	}
}
