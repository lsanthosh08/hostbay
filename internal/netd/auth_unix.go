//go:build darwin || linux

package netd

import (
	"context"
	"io/fs"
	"net"
	"os"
	"strconv"
	"syscall"
)

func fileOwner(fi fs.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

func currentUID() (string, error) { return strconv.Itoa(os.Getuid()), nil }

// listen opens the helper's socket: owned by the installing user, mode 0600,
// in a root-owned directory, so no other user can even connect. The peer
// check (peerOf) then admits only the paired Hostbay.
func listen(cfg Config) (net.Listener, error) { return listenAt(socketPath(), cfg) }

func listenAt(path string, cfg Config) (net.Listener, error) {
	uid, err := strconv.Atoi(cfg.UID)
	if err != nil {
		return nil, err
	}
	os.Remove(path)             // a socket left by a previous run
	old := syscall.Umask(0o177) // never a moment with a wider mode
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	if err := os.Chown(path, uid, -1); err != nil {
		ln.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// peerCred runs f on a Unix connection's descriptor.
func peerCred(c net.Conn, f func(fd int) error) error {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return syscall.EINVAL
	}
	rc, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) { ferr = f(int(fd)) }); err != nil {
		return err
	}
	return ferr
}

func dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", socketPath())
}
