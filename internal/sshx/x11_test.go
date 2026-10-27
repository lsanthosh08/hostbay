//go:build !windows

package sshx

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeX is a local X server on a Unix socket, named by DISPLAY for the
// test, whose real cookie is in an XAUTHORITY file. It records each
// connection's setup and answers "XSERVEOK".
type fakeX struct {
	mu     sync.Mutex
	setups [][]byte
}

var realCookie = []byte("the-REAL-cookie!")

func newFakeX(t *testing.T) *fakeX {
	t.Helper()
	dir, err := os.MkdirTemp("", "hbx") // short: socket paths are capped near 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "x:0") // as XQuartz names its socket
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	x := &fakeX{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 12+pad4(len(x11Proto))+pad4(len(realCookie)))
				n, _ := io.ReadAtLeast(c, buf, 12)
				x.mu.Lock()
				x.setups = append(x.setups, buf[:n])
				x.mu.Unlock()
				c.Write([]byte("XSERVEOK"))
			}()
		}
	}()
	t.Setenv("DISPLAY", sock)
	t.Setenv("XAUTHORITY", writeXauth(t,
		xauthEntry(familyLocal, "otherbox", "1", x11Proto, []byte("wrong-display-ck")),
		xauthEntry(familyLocal, "mybox", "0", x11Proto, realCookie)))
	return x
}

func (x *fakeX) got() [][]byte {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([][]byte(nil), x.setups...)
}

func dialX11(t *testing.T, s *testServer, on bool) *Client {
	t.Helper()
	d := &Dialer{KnownHostsPath: writeKnownHosts(t, s.knownLine(t))}
	hop := s.hop()
	hop.X11 = on
	c, err := d.Dial(context.Background(), Target{Key: "k", Hops: []Hop{hop}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// x11Session opens a session that asked for X11 and runs cmd on it.
func x11Session(t *testing.T, c *Client, cmd string) string {
	t.Helper()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	release, err := c.RequestX11(sess)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	out, err := sess.Output(cmd)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// x11-req goes out only for a host that forwards X11, with a fresh random
// cookie per session that is never the local X server's own (KTD20).
func TestX11RequestOnlyWhenEnabled(t *testing.T) {
	newFakeX(t)
	s := newTestServer(t, serverOpts{})
	if got := x11Session(t, dialX11(t, s, false), "xcookie"); got != "" {
		t.Fatalf("a host without X11 sent x11-req with cookie %q", got)
	}
	on := dialX11(t, s, true)
	a, b := x11Session(t, on, "xcookie"), x11Session(t, on, "xcookie")
	if len(a) != 32 || len(b) != 32 || a == b {
		t.Fatalf("cookies %q and %q, want two different 16-byte hex cookies", a, b)
	}
	if strings.Contains(a+b, hex.EncodeToString(realCookie)) {
		t.Fatal("the real local cookie was sent to the server")
	}
}

// Covers AE18. A channel presenting a wrong cookie, or none, is closed and
// the local X server sees no connection; the session's own cookie reaches
// it with the real cookie swapped in.
func TestX11ChannelNeedsTheSessionsCookie(t *testing.T) {
	x := newFakeX(t)
	c := dialX11(t, newTestServer(t, serverOpts{}), true)
	for _, which := range []string{hex.EncodeToString([]byte("guessed-cookie!!")), "none"} {
		if got := x11Session(t, c, "xopen "+which); got != "closed" {
			t.Fatalf("cookie %s: X client got %q", which, got)
		}
	}
	if n := len(x.got()); n != 0 {
		t.Fatalf("the local X server saw %d connections from bad cookies", n)
	}

	if got := x11Session(t, c, "xopen session"); got != "XSERVEOK" {
		t.Fatalf("X client got %q", got)
	}
	setups := x.got()
	if len(setups) != 1 || !bytes.Equal(setups[0], testSetup('l', x11Proto, realCookie)) {
		t.Fatalf("local X server got %q, want the setup with the real cookie", setups)
	}
}

// With forwarding off for the host no handler exists, and with it on but no
// session asking, the channel is refused outright (KTD20).
func TestX11ChannelRefusedWithoutAnAskingSession(t *testing.T) {
	x := newFakeX(t)
	s := newTestServer(t, serverOpts{})
	off := dialX11(t, s, false)
	if got := x11Session(t, off, "xopen session"); got != "refused" {
		t.Fatalf("forwarding off: X client got %q", got)
	}

	on := dialX11(t, s, true)
	sess, err := on.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	// This session never asked for X11; an earlier one did and ended.
	x11Session(t, on, "xcookie")
	if out, _ := sess.Output("xopen " + hex.EncodeToString([]byte("guessed-cookie!!"))); string(out) != "refused" {
		t.Fatalf("no X11 session open: X client got %q", out)
	}
	if n := len(x.got()); n != 0 {
		t.Fatalf("the local X server saw %d connections", n)
	}
}

// A big-endian client's setup is rewritten in its own byte order.
func TestSetupWithKeepsByteOrder(t *testing.T) {
	in := testSetup('B', x11Proto, []byte("fake-cookie-1234"))
	if got := setupWith(in[:12], realCookie); !bytes.Equal(got, testSetup('B', x11Proto, realCookie)) {
		t.Fatalf("got %q", got)
	}
	if got := setupWith(in[:12], nil); !bytes.Equal(got, testSetup('B', "", nil)) {
		t.Fatalf("no cookie: got %q", got)
	}
}

// The Xvnc command line listens for neither X nor VNC on TCP, keeps the
// VNC socket and cookie file private, and carries no cookie (KTD20).
func TestXvncScriptCommandLine(t *testing.T) {
	sc := xvncScript("startxfce4; echo 'hi'")
	for _, want := range []string{
		`Xvnc ":$n" -auth "$dir/auth" -nolisten tcp -rfbport -1 -rfbunixpath "$dir/rfb" -rfbunixmode 0600 -SecurityTypes None`,
		"umask 077\n",
		`dir=$(mktemp -d `,
		`IFS= read -r auth`,
		`desk='startxfce4; echo '\''hi'\'''`,
	} {
		if !strings.Contains(sc, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	if strings.Index(sc, "umask 077") > strings.Index(sc, "mktemp") || strings.Index(sc, "mktemp") > strings.Index(sc, `printf "$auth"`) {
		t.Error("the cookie file is written before the private directory exists")
	}
	if got := printfEscape([]byte{0, 'A', 255}); got != `\000\101\377` {
		t.Fatalf("printfEscape = %q", got)
	}
}
