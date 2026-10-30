package netd

import (
	"net"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// peerOf asks the kernel who is on the other end: the uid from
// LOCAL_PEERCRED and the executable of the LOCAL_PEERPID process from
// proc_pidpath, which the kernel derives from the process's text vnode and
// the process cannot rewrite.
//
// shortcut: the pid is read at accept and its path looked up after, so a
// caller that exits and has its pid reused in between is judged by the new
// process; replace the path check with a code-signing requirement once
// releases are signed (KTD8).
func peerOf(c net.Conn) (Peer, error) {
	var uid uint32
	var pid int
	err := peerCred(c, func(fd int) error {
		cred, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil {
			return err
		}
		uid = cred.Uid
		pid, err = unix.GetsockoptInt(fd, unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		return err
	})
	if err != nil {
		return Peer{}, err
	}
	exe, err := pidPath(pid)
	if err != nil {
		return Peer{}, err
	}
	return Peer{UID: strconv.FormatUint(uint64(uid), 10), Exe: exe}, nil
}

// pidPath is libproc's proc_pidpath without cgo: proc_info with
// PROC_INFO_CALL_PIDINFO (2) and PROC_PIDPATHINFO (11).
func pidPath(pid int) (string, error) {
	buf := make([]byte, 4*1024) // PROC_PIDPATHINFO_MAXSIZE
	_, _, errno := syscall.Syscall6(unix.SYS_PROC_INFO, 2, uintptr(pid), 11, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if errno != 0 {
		return "", errno
	}
	return unix.ByteSliceToString(buf), nil
}
