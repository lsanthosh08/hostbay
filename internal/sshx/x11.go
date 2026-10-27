package sshx

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// x11Proto is the only X authorization Hostbay forwards (KTD20).
const x11Proto = "MIT-MAGIC-COOKIE-1"

// x11SetupTimeout bounds how long a forwarded X client may take to send
// its connection setup, so a silent one cannot hold a channel open.
const x11SetupTimeout = 10 * time.Second

// xDisplay is a parsed $DISPLAY: how to dial the X server, and what its
// xauth entries are filed under.
type xDisplay struct {
	network, addr string
	host          string // as written; "" for a socket
	number        string // the display number, as xauth stores it
	screen        int
	local         bool // on this computer: xauth's local entries apply
}

// parseDisplay reads a $DISPLAY as goos's X libraries would: a path (as
// XQuartz sets) is that socket, ":N" and "unix:N" the socket
// /tmp/.X11-unix/XN (loopback TCP on Windows, where X servers have no
// socket), and "host:N" TCP port 6000+N.
func parseDisplay(s, goos string) (xDisplay, error) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return xDisplay{}, fmt.Errorf("DISPLAY %q has no display number", s)
	}
	num, scr, _ := strings.Cut(s[i+1:], ".")
	d := xDisplay{host: s[:i], number: num}
	n, err := strconv.Atoi(num)
	if err == nil && scr != "" {
		d.screen, err = strconv.Atoi(scr)
	}
	if err != nil || n < 0 || n > 59535 || d.screen < 0 {
		return xDisplay{}, fmt.Errorf("DISPLAY %q is not host:display.screen", s)
	}
	switch {
	case strings.HasPrefix(s, "/"):
		d.network, d.addr, d.host, d.local = "unix", s, "", true
	case (d.host == "" || d.host == "unix") && goos == "windows":
		d.network, d.addr, d.local = "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(6000+n)), true
	case d.host == "" || d.host == "unix":
		d.network, d.addr, d.host, d.local = "unix", "/tmp/.X11-unix/X"+num, "", true
	default:
		d.network, d.addr = "tcp", net.JoinHostPort(d.host, strconv.Itoa(6000+n))
		ip := net.ParseIP(d.host)
		d.local = d.host == "localhost" || ip != nil && ip.IsLoopback()
	}
	return d, nil
}

// installHint names the X server to install on this OS (KTD20).
func installHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "install XQuartz (xquartz.org) and open it"
	case "windows":
		return "install and start VcXsrv or X410"
	}
	return "start an Xorg or XWayland session; DISPLAY must be set"
}

// localDisplay is this computer's X server: $DISPLAY, else the lowest
// socket in /tmp/.X11-unix (an app started from the Dock or Finder has
// no DISPLAY even with XQuartz running), else :0 on Windows.
func localDisplay() (xDisplay, error) {
	s := os.Getenv("DISPLAY")
	if s == "" && runtime.GOOS == "windows" {
		s = ":0"
	}
	if s == "" {
		ents, _ := os.ReadDir("/tmp/.X11-unix")
		var nums []int
		for _, e := range ents {
			if n, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "X")); err == nil && strings.HasPrefix(e.Name(), "X") {
				nums = append(nums, n)
			}
		}
		if len(nums) == 0 {
			return xDisplay{}, errors.New("no X server is running on this computer; " + installHint())
		}
		s = ":" + strconv.Itoa(slices.Min(nums))
	}
	return parseDisplay(s, runtime.GOOS)
}

func dialDisplay(d xDisplay) (net.Conn, error) {
	c, err := net.DialTimeout(d.network, d.addr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the X server at %s; %s: %w", d.addr, installHint(), err)
	}
	return c, nil
}

// LocalXServer checks that this computer's X server answers, and returns
// where it is. Its error says which X server to install.
func LocalXServer() (string, error) {
	d, err := localDisplay()
	if err != nil {
		return "", err
	}
	c, err := dialDisplay(d)
	if err != nil {
		return "", err
	}
	c.Close()
	return d.addr, nil
}

// x11Fwd serves one connection's x11 channels (KTD20). It is registered
// once per connection at dial time, because the connection is shared
// between tabs (KTD5); each session that asks for X11 adds its fake
// cookie, and removes it when it ends.
type x11Fwd struct {
	mu      sync.Mutex
	cookies map[*[16]byte]struct{}
}

func forwardX11(c *ssh.Client) *x11Fwd {
	f := &x11Fwd{cookies: map[*[16]byte]struct{}{}}
	chans := c.HandleChannelOpen("x11")
	go func() {
		for nch := range chans {
			go f.serve(nch)
		}
	}()
	return f
}

// RequestX11 asks the server to forward X11 on s with a fresh random
// cookie, which the x11 channels s's programs open must present. The
// local X server's real cookie never leaves this computer. release ends
// the cookie's validity; call it when s ends. When the host does not
// forward X11, nothing is sent and the server's x11 channels are refused.
func (c *Client) RequestX11(s *ssh.Session) (release func(), err error) {
	release = func() {}
	if c.x11 == nil {
		return release, nil
	}
	screen := 0
	if d, err := localDisplay(); err == nil {
		screen = d.screen
	}
	cookie := new([16]byte)
	_, _ = rand.Read(cookie[:])
	c.x11.mu.Lock()
	c.x11.cookies[cookie] = struct{}{}
	c.x11.mu.Unlock()
	// sshd takes the cookie in hex, as xauth prints it.
	ok, err := s.SendRequest("x11-req", true, ssh.Marshal(struct {
		Single bool
		Proto  string
		Cookie string
		Screen uint32
	}{false, x11Proto, hex.EncodeToString(cookie[:]), uint32(screen)}))
	drop := func() {
		c.x11.mu.Lock()
		delete(c.x11.cookies, cookie)
		c.x11.mu.Unlock()
	}
	if err == nil && !ok {
		err = errors.New("the server refused X11 forwarding; its sshd may have X11Forwarding off")
	}
	if err != nil {
		drop()
		return release, err
	}
	return drop, nil
}

// serve relays one x11 channel to the local X server once its setup
// presents a cookie a live session sent, swapping in the real cookie. A
// channel with no session asking for X11, or with any other cookie, is
// closed and the local X server never sees it (AE18).
func (f *x11Fwd) serve(nch ssh.NewChannel) {
	f.mu.Lock()
	none := len(f.cookies) == 0
	f.mu.Unlock()
	if none {
		nch.Reject(ssh.Prohibited, "no session asked for X11 forwarding")
		return
	}
	ch, reqs, err := nch.Accept()
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	timer := time.AfterFunc(x11SetupTimeout, func() { ch.Close() })
	hdr, err := f.checkSetup(ch)
	if !timer.Stop() || err != nil {
		ch.Close()
		return
	}
	d, err := localDisplay()
	var real []byte
	if err == nil {
		real, err = readCookie(xauthFile(), d)
	}
	var xc net.Conn
	if err == nil {
		xc, err = dialDisplay(d)
	}
	if err != nil {
		ch.Close()
		return
	}
	if _, err := xc.Write(setupWith(hdr, real)); err != nil {
		xc.Close()
		ch.Close()
		return
	}
	pipe(ch, xc)
}

// checkSetup reads an X11 connection setup from r and returns its 12-byte
// header if it presents a live session's cookie. The comparison is
// constant time.
func (f *x11Fwd) checkSetup(r io.Reader) ([]byte, error) {
	hdr := make([]byte, 12)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	order, err := byteOrder(hdr[0])
	if err != nil {
		return nil, err
	}
	nameLen, dataLen := int(order.Uint16(hdr[6:])), int(order.Uint16(hdr[8:]))
	body := make([]byte, pad4(nameLen)+pad4(dataLen))
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	name, data := body[:nameLen], body[pad4(nameLen):pad4(nameLen)+dataLen]
	match := 0
	f.mu.Lock()
	for c := range f.cookies {
		match |= subtle.ConstantTimeCompare(c[:], data)
	}
	f.mu.Unlock()
	if string(name) != x11Proto || match != 1 {
		return nil, errors.New("x11: wrong cookie")
	}
	return hdr, nil
}

// setupWith is the setup hdr began, carrying cookie (none when nil).
func setupWith(hdr, cookie []byte) []byte {
	order, _ := byteOrder(hdr[0])
	name := []byte(x11Proto)
	if cookie == nil {
		name = nil
	}
	b := slices.Clone(hdr[:6])
	b = order.AppendUint16(b, uint16(len(name)))
	b = order.AppendUint16(b, uint16(len(cookie)))
	b = append(b, hdr[10:12]...)
	b = append(b, name...)
	b = append(b, make([]byte, pad4(len(name))-len(name))...)
	b = append(b, cookie...)
	return append(b, make([]byte, pad4(len(cookie))-len(cookie))...)
}

type byteOrderer interface {
	binary.ByteOrder
	binary.AppendByteOrder
}

func byteOrder(b byte) (byteOrderer, error) {
	switch b {
	case 'B':
		return binary.BigEndian, nil
	case 'l':
		return binary.LittleEndian, nil
	}
	return nil, errors.New("x11: not a connection setup")
}

func pad4(n int) int { return (n + 3) &^ 3 }

// StartXvnc starts an X11 desktop on c's host and returns a connection to
// its VNC server, which asks for no authentication (KTD20). Xvnc listens
// only on a 0600 Unix socket in a fresh 0700 directory, with no TCP
// listener for X or VNC, and admits X clients by a fresh cookie that
// travels on the session's stdin, never a command line others can read.
// desktop is the command to run on it; empty runs the first of
// startxfce4, mate-session or gnome-session found. Closing the connection
// ends the session, and with it Xvnc and the desktop; the script then
// removes the socket and cookie file.
func StartXvnc(ctx context.Context, c *ssh.Client, desktop string) (net.Conn, error) {
	sess, err := c.NewSession()
	if err != nil {
		return nil, err
	}
	stdin, err := sess.StdinPipe()
	var out io.Reader
	if err == nil {
		out, err = sess.StdoutPipe()
	}
	if err == nil {
		err = sess.Start("sh -c " + shQuote(xvncScript(desktop)))
	}
	cookie := make([]byte, 16)
	_, _ = rand.Read(cookie)
	if err == nil {
		_, err = io.WriteString(stdin, printfEscape(wildXauth(cookie))+"\n")
	}
	if err != nil {
		sess.Close()
		return nil, err
	}
	status := make(chan string, 1)
	go func() {
		r := bufio.NewReader(out)
		for {
			l, err := r.ReadString('\n')
			if s, ok := strings.CutPrefix(strings.TrimSpace(l), "HOSTBAY "); ok {
				status <- s
				_, _ = io.Copy(io.Discard, r) // a full window would stall the session
				return
			}
			if err != nil {
				status <- ""
				return
			}
		}
	}()
	var st string
	select {
	case st = <-status:
	case <-ctx.Done():
		sess.Close()
		return nil, ctx.Err()
	}
	path, ready := strings.CutPrefix(st, "ready ")
	if !ready {
		sess.Close()
		return nil, xvncError(st)
	}
	conn, err := c.Dial("unix", path)
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("cannot reach the X11 desktop's VNC socket: %w", err)
	}
	return &xvncConn{Conn: conn, stdin: stdin, sess: sess}, nil
}

func xvncError(status string) error {
	switch status {
	case "no-xvnc":
		return errors.New("this host has no Xvnc; install TigerVNC's Xvnc (package tigervnc-standalone-server or tigervnc-server)")
	case "no-unixpath":
		return errors.New("this host's Xvnc cannot serve VNC on a Unix socket (no -rfbunixpath); install TigerVNC 1.10 or later")
	case "no-desktop":
		return errors.New("found no desktop to run on this host (startxfce4, mate-session or gnome-session); install one or set the host's X11 desktop command")
	case "xvnc-failed":
		return errors.New("Xvnc did not start on this host")
	}
	return errors.New("the X11 desktop did not start: the host ended the session")
}

// xvncConn is the VNC connection to a desktop; closing it hangs up the
// session that runs Xvnc.
type xvncConn struct {
	net.Conn
	stdin io.Closer
	sess  *ssh.Session
}

func (c *xvncConn) Close() error {
	err := c.Conn.Close()
	c.stdin.Close() // EOF on the script's stdin ends the desktop
	c.sess.Close()
	return err
}

// xvncScript is the POSIX sh script StartXvnc runs. Its first stdin line
// is the xauth file as printf escapes; printf is a shell builtin, so the
// cookie never appears in a process's arguments. EOF on stdin, or a hang-up,
// ends Xvnc and the desktop and removes their directory.
func xvncScript(desktop string) string {
	return `IFS= read -r auth || exit 1
command -v Xvnc >/dev/null 2>&1 || { echo 'HOSTBAY no-xvnc'; exit 1; }
Xvnc -help 2>&1 | grep -q -- -rfbunixpath || { echo 'HOSTBAY no-unixpath'; exit 1; }
desk=` + shQuote(desktop) + `
if [ -z "$desk" ]; then
  for c in startxfce4 mate-session gnome-session; do
    if command -v "$c" >/dev/null 2>&1; then desk=$c; break; fi
  done
fi
[ -n "$desk" ] || { echo 'HOSTBAY no-desktop'; exit 1; }
umask 077
dir=$(mktemp -d "${TMPDIR:-/tmp}/hostbay-x11.XXXXXX") || exit 1
xpid= dpid=
end() { kill $dpid $xpid 2>/dev/null; rm -rf "$dir"; }
trap 'end; exit 1' HUP INT TERM
printf "$auth" >"$dir/auth"
n=10
while [ -e "/tmp/.X$n-lock" ] || [ -e "/tmp/.X11-unix/X$n" ]; do n=$((n+1)); done
Xvnc ":$n" -auth "$dir/auth" -nolisten tcp -rfbport -1 -rfbunixpath "$dir/rfb" -rfbunixmode 0600 -SecurityTypes None </dev/null >/dev/null 2>&1 &
xpid=$!
i=0
until [ -S "$dir/rfb" ]; do
  i=$((i+1))
  if [ $i -gt 150 ] || ! kill -0 $xpid 2>/dev/null; then echo 'HOSTBAY xvnc-failed'; end; exit 1; fi
  sleep 0.1
done
if command -v dbus-run-session >/dev/null 2>&1; then set -- dbus-run-session -- sh -c "$desk"; else set -- sh -c "$desk"; fi
DISPLAY=":$n" XAUTHORITY="$dir/auth" "$@" </dev/null >/dev/null 2>&1 &
dpid=$!
echo "HOSTBAY ready $dir/rfb"
cat >/dev/null
end
`
}

// printfEscape writes b as printf format escapes, three octal digits per
// byte, so any byte survives a line of text.
func printfEscape(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		fmt.Fprintf(&sb, `\%03o`, c)
	}
	return sb.String()
}

// shQuote makes s one POSIX shell word.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
