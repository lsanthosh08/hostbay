package sshx

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// bindTo pins a socket to ifi with SO_BINDTODEVICE before it connects;
// Linux 5.7 and later allow it without admin rights (KTD7).
func bindTo(ifi *net.Interface) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var err error
		cerr := c.Control(func(fd uintptr) {
			err = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, ifi.Name)
		})
		if cerr != nil {
			return cerr
		}
		return err
	}
}
