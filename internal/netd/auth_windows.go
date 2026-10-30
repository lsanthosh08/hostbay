package netd

import (
	"context"
	"errors"
	"io/fs"
	"net"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func fileOwner(fs.FileInfo) (int, bool) { return 0, false }

func currentUID() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}

// listen opens the helper's named pipe with a DACL that lets only the
// installing user (and SYSTEM, which runs the helper) open it. The peer
// check (peerOf) then admits only the paired Hostbay.
func listen(cfg Config) (net.Listener, error) {
	if _, err := windows.StringToSid(cfg.UID); err != nil {
		return nil, err
	}
	return winio.ListenPipe(windowsPipe, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;SY)(A;;GRGW;;;" + cfg.UID + ")",
	})
}

// peerOf asks Windows for the pipe client's process, then for that
// process's user SID and image path.
//
// shortcut: the pid is looked up after the client connected, so a client
// that exits and has its pid reused in between is judged by the new
// process; replace the path check with an Authenticode check once releases
// are signed (KTD8).
func peerOf(c net.Conn) (Peer, error) {
	f, ok := c.(interface{ Fd() uintptr })
	if !ok {
		return Peer{}, errors.New("not a named pipe")
	}
	var pid uint32
	if err := windows.GetNamedPipeClientProcessId(windows.Handle(f.Fd()), &pid); err != nil {
		return Peer{}, err
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return Peer{}, err
	}
	defer windows.CloseHandle(h)
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err != nil {
		return Peer{}, err
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		return Peer{}, err
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return Peer{}, err
	}
	return Peer{UID: u.User.Sid.String(), Exe: windows.UTF16ToString(buf[:n])}, nil
}

func dial(ctx context.Context) (net.Conn, error) {
	return winio.DialPipeContext(ctx, windowsPipe)
}
