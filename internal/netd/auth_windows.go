package netd

import (
	"context"
	"errors"
	"fmt"
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

// pipeAccess is what the installing user may do with the helper's pipe:
// read and write it, but not FILE_CREATE_PIPE_INSTANCE (in GENERIC_WRITE),
// which would let any of their processes serve instances beside the helper.
const pipeAccess = windows.FILE_GENERIC_READ | windows.FILE_WRITE_DATA | windows.FILE_WRITE_ATTRIBUTES // 0x12018b

// listen opens the helper's named pipe, owned by SYSTEM (which Hostbay
// checks before trusting it), with a DACL that lets only the installing user
// (and SYSTEM, which runs the helper) open it. The peer check (peerOf) then
// admits only the paired Hostbay.
func listen(cfg Config) (net.Listener, error) {
	if _, err := windows.StringToSid(cfg.UID); err != nil {
		return nil, err
	}
	return winio.ListenPipe(windowsPipe, &winio.PipeConfig{
		SecurityDescriptor: fmt.Sprintf("O:SYD:P(A;;GA;;;SY)(A;;0x%x;;;%s)", pipeAccess, cfg.UID),
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

// dial opens the helper's pipe and refuses it unless SYSTEM owns it: while
// the service is down any program can create a pipe of that name, and would
// be handed every secret Hostbay sends. Only a privileged process can make
// SYSTEM an object's owner. The owner is checked rather than the server's
// token, which a user's process cannot open on a SYSTEM service.
func dial(ctx context.Context) (net.Conn, error) {
	c, err := winio.DialPipeAccess(ctx, windowsPipe, pipeAccess)
	if err != nil {
		return nil, err
	}
	if err := checkPipeOwner(c); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func checkPipeOwner(c net.Conn) error {
	f, ok := c.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("not a named pipe")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("checking who serves the helper's pipe: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("checking who serves the helper's pipe: %w", err)
	}
	if !owner.IsWellKnown(windows.WinLocalSystemSid) {
		return fmt.Errorf("the helper's pipe belongs to %s, not SYSTEM; another program may be posing as the helper", owner)
	}
	return nil
}
