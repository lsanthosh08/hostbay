//go:build !windows

package app

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"hostbay/internal/remotedesktop/rfbtest"
	"hostbay/internal/sftp/sftptest"
)

// fakeXvnc stands in for TigerVNC's Xvnc on an sftptest host: it answers
// -help with the -rfbunixpath option and otherwise logs its pid and
// arguments to $XVNC_LOG and sleeps, so the test can serve RFB on the
// socket it was given.
const fakeXvnc = `#!/bin/sh
case "$1" in -help) echo '-rfbunixpath path  serve VNC on a Unix socket'; exit 0;; esac
echo "$$ $*" >"$XVNC_LOG"
exec sleep 600
`

// An X11 desktop opens in the VNC tab: Xvnc gets no TCP listener, a
// private 0600 socket and cookie file, the desktop runs on its display,
// the stream relays the desktop's VNC server, and closing the session
// ends Xvnc and removes the socket and cookie (KTD20).
func TestX11DesktopRelaysThroughTheVncStream(t *testing.T) {
	srv := sftptest.New(t, "pw")
	srv.Command(t, "Xvnc", fakeXvnc)
	srv.Command(t, "dbus-run-session", "#!/bin/sh\n[ \"$1\" = -- ] && shift\nexec \"$@\"\n") // a real one needs a system bus
	logs := t.TempDir()
	xlog, dlog := filepath.Join(logs, "xvnc"), filepath.Join(logs, "desk")
	t.Setenv("XVNC_LOG", xlog)
	t.Setenv("DESK_LOG", dlog)

	v, s := newVncService(t, srv.KnownHostsLine())
	x := NewX11Service(v.conns, v)
	host, port, _ := net.SplitHostPort(srv.Addr)
	p, _ := strconv.Atoi(port)
	auth, pw, desk := "password", "pw", `echo "$DISPLAY $XAUTHORITY" >"$DESK_LOG"; exec sleep 600`
	h, err := s.SaveHost(Host{Alias: "box", Hostname: host, Port: p, User: "u", AuthType: &auth, Password: &pw, X11Desktop: &desk})
	if err != nil {
		t.Fatal(err)
	}

	// Serve RFB, asking for no authentication, on the socket Xvnc was given.
	args := map[string]string{}
	var pid int
	started := make(chan struct{})
	go func() {
		defer close(started)
		var b []byte
		for end := time.Now().Add(5 * time.Second); len(b) == 0 && time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
			b, _ = os.ReadFile(xlog)
		}
		f := strings.Fields(string(b))
		if len(f) == 0 {
			return
		}
		pid, _ = strconv.Atoi(f[0])
		for i := 2; i < len(f); i++ {
			args[f[i-1]] = f[i]
		}
		if ln, err := net.Listen("unix", args["-rfbunixpath"]); err == nil {
			rfbtest.Serve(t, ln, "", 1)
		}
	}()

	id, err := x.X11DesktopOpen(context.Background(), h.ID)
	<-started
	if err != nil {
		t.Fatal(err)
	}
	sock, cookie := args["-rfbunixpath"], args["-auth"]
	if args["-nolisten"] != "tcp" || args["-rfbport"] != "-1" || args["-SecurityTypes"] != "None" || args["-rfbunixmode"] != "0600" {
		t.Fatalf("Xvnc arguments %v", args)
	}
	dir := filepath.Dir(sock)
	if filepath.Dir(cookie) != dir {
		t.Fatalf("socket %s and cookie %s are not in one directory", sock, cookie)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, cookie: 0o600} {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s: %v, %v; want mode %o", path, fi.Mode(), err, want)
		}
	}
	ck, _ := os.ReadFile(cookie)
	if len(ck) != 44 || ck[0] != 0xff || ck[1] != 0xff || !strings.Contains(string(ck), "MIT-MAGIC-COOKIE-1") {
		t.Fatalf("cookie file %q, want one wildcard MIT-MAGIC-COOKIE-1 entry", ck)
	}
	octal := ""
	for _, c := range ck[len(ck)-16:] {
		octal += fmt.Sprintf(`\%03o`, c)
	}
	if slices.ContainsFunc(srv.Commands(), func(c string) bool { return strings.Contains(c, octal) }) {
		t.Fatal("the cookie appeared on a command line")
	}
	waitUntil(t, "the desktop to start", func() bool {
		b, _ := os.ReadFile(dlog)
		return strings.HasPrefix(string(b), ":") && strings.Contains(string(b), " "+cookie)
	})

	page := rfbtest.NewPage()
	page.Feed([]byte(id))
	done := make(chan struct{})
	go func() { v.serve(page); close(done) }()
	if got := page.Read(t, 0, 12); string(got) != "RFB 003.008\n" {
		t.Fatalf("greeting %q", got)
	}
	page.Feed([]byte("RFB 003.008\n"))
	page.Feed([]byte{1}) // security type None
	page.Feed([]byte{1}) // ClientInit
	if got := page.Read(t, 14, 18+len(rfbtest.ServerInit)); string(got[4:]) != string(rfbtest.ServerInit) {
		t.Fatalf("ServerInit %q", got)
	}
	page.Feed([]byte("ping"))
	if got := page.Read(t, 18+len(rfbtest.ServerInit), 22+len(rfbtest.ServerInit)); string(got) != "ping" {
		t.Fatalf("echo %q", got)
	}

	page.Close()
	<-done
	waitUntil(t, "Xvnc to end and its directory to go", func() bool {
		_, err := os.Stat(dir)
		return os.IsNotExist(err) && syscall.Kill(pid, 0) != nil
	})
}
