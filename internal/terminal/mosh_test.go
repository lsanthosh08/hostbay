package terminal

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mosh "github.com/unixshells/mosh-go"
	vt "github.com/unixshells/vt-go"
	"golang.org/x/crypto/ssh"

	"hostbay/internal/sftp/sftptest"
	"hostbay/internal/sshx"
	"hostbay/internal/vpn/socks"
)

const testKey = "4NeCCgvZFe2RnPgrcU1PQw" // 16 bytes, as mosh-server prints it

func TestParseMoshConnect(t *testing.T) {
	banner := "\r\nmosh-server (mosh 1.4.0) [build mosh 1.4.0]\r\nCopyright 2012 Keith Winstein\r\n"
	port, key, ok := parseMoshConnect(banner + "MOSH CONNECT 60001 " + testKey + "\r\n\r\n[mosh-server detached, pid = 4242]\r\n")
	if !ok || port != 60001 || len(key) != 16 {
		t.Fatalf("got port=%d key=%d bytes ok=%v", port, len(key), ok)
	}
	for _, bad := range []string{
		banner,
		"MOSH CONNECT 0 " + testKey,
		"MOSH CONNECT 70000 " + testKey,
		"MOSH CONNECT 60001 not-base64!",
		"MOSH CONNECT 60001 AAAA",
	} {
		if _, _, ok := parseMoshConnect(bad); ok {
			t.Errorf("parsed %q", bad)
		}
	}
}

// moshHost is the in-process SSH server with mosh-server replaced by
// script, or missing when script is "".
func moshHost(t *testing.T, script string) *ssh.Client {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the test server runs commands with /bin/sh")
	}
	dir := t.TempDir()
	if script != "" {
		if err := os.WriteFile(filepath.Join(dir, "mosh-server"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Only the fake (or nothing) is on the PATH, whatever this machine has.
	t.Setenv("PATH", dir)
	s := sftptest.New(t, "pw")
	return s.Dial(t)
}

func TestStartMoshServerReadsConnectLine(t *testing.T) {
	c := moshHost(t, `echo "mosh-server (mosh 1.4.0)" >&2; echo; echo "MOSH CONNECT 60007 `+testKey+`"`)
	port, key, err := startMoshServer(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if port != 60007 || len(key) != 16 {
		t.Fatalf("port=%d key=%d bytes", port, len(key))
	}
}

func TestStartMoshServerMissingGivesInstallHint(t *testing.T) {
	c := moshHost(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := startMoshServer(ctx, c)
	if err == nil || !strings.Contains(err.Error(), "install") {
		t.Fatalf("err = %v, want the install hint", err)
	}
	if ctx.Err() != nil {
		t.Fatal("waited for the timeout instead of failing at once")
	}
}

func TestStartMoshServerFailureShowsReasonButNotKey(t *testing.T) {
	c := moshHost(t, `echo "MOSH_KEY=`+testKey+`"; echo "mosh-server needs a UTF-8 native locale to run." >&2; exit 1`)
	_, _, err := startMoshServer(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "UTF-8 native locale") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatalf("error shows the session key: %v", err)
	}
}

// echoRW is the shell behind the test mosh server: it echoes its input.
type echoRW struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func newEcho() *echoRW { r, w := io.Pipe(); return &echoRW{r, w} }

func (e *echoRW) Read(b []byte) (int, error)  { return e.r.Read(b) }
func (e *echoRW) Write(b []byte) (int, error) { return e.w.Write(b) }
func (e *echoRW) Close() error                { e.w.Close(); return e.r.Close() }

// relay forwards datagrams between the client and the server. move gives
// it a new socket towards the server, which the server sees as the client
// changing address, as after a switch from Wi-Fi to cellular.
type relay struct {
	in     *net.UDPConn
	server *net.UDPAddr

	mu     sync.Mutex
	out    *net.UDPConn
	client *net.UDPAddr
}

func newRelay(t *testing.T, server *net.UDPAddr) *relay {
	in, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	r := &relay{in: in, server: server}
	t.Cleanup(func() { in.Close(); r.mu.Lock(); r.out.Close(); r.mu.Unlock() })
	r.move(t)
	go func() {
		b := make([]byte, 65536)
		for {
			n, from, err := in.ReadFromUDP(b)
			if err != nil {
				return
			}
			r.mu.Lock()
			r.client = from
			out := r.out
			r.mu.Unlock()
			out.WriteToUDP(b[:n], server)
		}
	}()
	return r
}

func (r *relay) move(t *testing.T) {
	out, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	old := r.out
	r.out = out
	r.mu.Unlock()
	if old != nil {
		old.Close()
	}
	go func() {
		b := make([]byte, 65536)
		for {
			n, _, err := out.ReadFromUDP(b)
			if err != nil {
				return
			}
			r.mu.Lock()
			client := r.client
			r.mu.Unlock()
			r.in.WriteToUDP(b[:n], client)
		}
	}()
}

func (r *relay) addr() *net.UDPAddr { return r.in.LocalAddr().(*net.UDPAddr) }

// waitScreen feeds a terminal's output to screen until it shows want.
// Mosh repaints changed cells, so the raw stream is not the text.
func waitScreen(t *testing.T, out <-chan []byte, screen *vt.Emulator, want string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !strings.Contains(screen.String(), want) {
		select {
		case b, ok := <-out:
			if !ok {
				t.Fatalf("terminal ended before %q; screen %q", want, screen.String())
			}
			screen.Write(b)
		case <-deadline:
			t.Fatalf("no %q on screen %q", want, screen.String())
		}
	}
}

func TestMoshSessionSurvivesAddressChange(t *testing.T) {
	srv, err := mosh.NewServer("", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	echo := newEcho()
	go srv.ServeRW(echo, func(uint16, uint16) {})
	t.Cleanup(func() { echo.Close() }) // ends ServeRW; Server.Close assumes a shell process
	port, key, ok := parseMoshConnect(srv.ConnectLine())
	if !ok {
		t.Fatalf("cannot parse %q", srv.ConnectLine())
	}
	r := newRelay(t, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})

	tr, err := dialMosh(context.Background(), r.addr(), key, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	out := make(chan []byte)
	go func() {
		defer close(out)
		for {
			b := make([]byte, 4096)
			n, err := tr.Read(b)
			if n > 0 {
				out <- b[:n]
			}
			if err != nil {
				return
			}
		}
	}()

	screen := newShadow(80, 24)
	tr.Write([]byte("hello"))
	waitScreen(t, out, screen, "hello")

	r.move(t)
	tr.Write([]byte(" again"))
	waitScreen(t, out, screen, "hello again")
}

func TestMoshWithoutUDPReplyFails(t *testing.T) {
	silent, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	defer func(d time.Duration) { moshReplyTimeout = d }(moshReplyTimeout)
	moshReplyTimeout = 300 * time.Millisecond

	key := make([]byte, 16)
	_, err = dialMosh(context.Background(), silent.LocalAddr().(*net.UDPAddr), key, 80, 24)
	if err == nil || !strings.Contains(err.Error(), "UDP") {
		t.Fatalf("err = %v, want a UDP hint", err)
	}
}

// A diff computed from an older state than the one on screen, as the server
// sends while its last update is unacknowledged, must not be appended.
func TestMoshScreenAppliesDiffToItsOwnBase(t *testing.T) {
	msg := func(s string) []byte { return mosh.MarshalHostMessage([]mosh.HostInstruction{{Hoststring: []byte(s)}}) }
	scr := newMoshScreen(20, 2)
	screen := newShadow(20, 2)
	screen.Write(scr.apply(msg("ab"), 0, 1, 0))
	screen.Write(scr.apply(msg("abc"), 0, 2, 0)) // state 2, from state 0
	screen.Write(scr.apply(msg("abc"), 0, 2, 0)) // a repeat changes nothing
	if got := strings.TrimSpace(strings.SplitN(screen.String(), "\n", 2)[0]); got != "abc" {
		t.Fatalf("screen shows %q, want %q", got, "abc")
	}
}

// Over a VPN the SSH client's remote address is the helper's loopback SOCKS
// endpoint, so mosh takes the server's address from $SSH_CONNECTION and
// sends its UDP through the helper's UDP ASSOCIATE (KTD2).
func TestMoshOverVPNUsesSSHConnectionAndTheHelpersUDP(t *testing.T) {
	srv, err := mosh.NewServer("", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	echo := newEcho()
	go srv.ServeRW(echo, func(uint16, uint16) {})
	t.Cleanup(func() { echo.Close() })
	port, _, ok := parseMoshConnect(srv.ConnectLine())
	if !ok {
		t.Fatalf("cannot parse %q", srv.ConnectLine())
	}
	// 10.77.0.5 is the server's address inside the VPN, reached only through it.
	t.Setenv("SSH_CONNECTION", "10.77.0.2 50022 10.77.0.5 22")
	c := moshHost(t, `echo "`+srv.ConnectLine()+`"`)

	var mu sync.Mutex
	var asked []string
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	inside := net.JoinHostPort("10.77.0.5", strconv.Itoa(port))
	go (&socks.Server{User: "vu", Pass: "vp", Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		asked = append(asked, network+" "+addr)
		mu.Unlock()
		if addr != inside {
			return nil, errors.New("not inside the VPN")
		}
		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	}}).Serve(ln)
	p := &sshx.Proxy{Type: sshx.TypeVPN, Host: "office", VPN: "v1", Socks: func() (string, string, string, error) {
		return ln.Addr().String(), "vu", "vp", nil
	}}

	tr, err := openMosh(context.Background(), c, p, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	out := make(chan []byte)
	go func() {
		defer close(out)
		for {
			b := make([]byte, 4096)
			n, err := tr.Read(b)
			if n > 0 {
				out <- b[:n]
			}
			if err != nil {
				return
			}
		}
	}()
	tr.Write([]byte("hello"))
	waitScreen(t, out, newShadow(80, 24), "hello")
	mu.Lock()
	defer mu.Unlock()
	if len(asked) == 0 || asked[0] != "udp "+inside {
		t.Fatalf("the helper was asked for %v, want udp %s", asked, inside)
	}
}
