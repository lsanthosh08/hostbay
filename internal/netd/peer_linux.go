package netd

import (
	"net"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// peerOf asks the kernel who is on the other end: SO_PEERCRED's uid and pid,
// and the pid's executable from /proc. A replaced binary reads as
// "<path> (deleted)" and fails the path check.
//
// shortcut: the pid is read at accept and its path looked up after, so a
// caller that exits and has its pid reused in between is judged by the new
// process; replace the path check with a signature check once releases are
// signed (KTD8).
func peerOf(c net.Conn) (Peer, error) {
	var cred *unix.Ucred
	err := peerCred(c, func(fd int) error {
		var err error
		cred, err = unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
		return err
	})
	if err != nil {
		return Peer{}, err
	}
	exe, err := os.Readlink("/proc/" + strconv.Itoa(int(cred.Pid)) + "/exe")
	if err != nil {
		return Peer{}, err
	}
	return Peer{UID: strconv.FormatUint(uint64(cred.Uid), 10), Exe: exe}, nil
}
