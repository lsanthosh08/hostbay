//go:build !windows

package openconnect

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/tun/netstack"

	"hostbay/internal/vpn/proto"
)

// The test binary is also the fake openconnect, and the tun-fd and
// browser children hostbay-vpn runs under it, so the real /bin/sh,
// socket pair and SCM_RIGHTS path all run.
const fakeEnv = "HOSTBAY_FAKE_OPENCONNECT" // the fake's mode
const fakeLog = "HOSTBAY_FAKE_LOG"         // where the fake writes each argv

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Getenv(handoffEnv) != "" {
		if err := Child(os.Args[1]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if mode := os.Getenv(fakeEnv); mode != "" {
		os.Exit(fakeOpenConnect(mode))
	}
	os.Exit(m.Run())
}

func fakeOpenConnect(mode string) int {
	args := os.Args[1:]
	if f, err := os.OpenFile(os.Getenv(fakeLog), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600); err == nil {
		fmt.Fprintf(f, "%q\n", args)
		f.Close()
	}
	if slices.Contains(args, "--version") {
		if mode == "old" {
			fmt.Println("OpenConnect version v9.01")
		} else {
			fmt.Println("OpenConnect version v9.12-unknown")
		}
		return 0
	}
	in := bufio.NewReader(os.Stdin)
	if slices.Contains(args, "--passwd-on-stdin") {
		// openconnect reads it while parsing options, before it connects.
		if pw, _ := in.ReadString('\n'); pw != testPass+"\n" {
			fmt.Fprintln(os.Stderr, "Login failed.")
			return 1
		}
	}
	switch mode {
	case "cert-changed":
		fmt.Fprintf(os.Stderr, "None of the 1 fingerprint(s) specified via --servercert match server's certificate: %s\n", pinB)
		fmt.Fprintln(os.Stderr, "Failed to open HTTPS connection to vpn.example.test")
		return 1
	case "cert-unknown":
		fmt.Fprintf(os.Stderr, "\nCertificate from VPN server \"vpn.example.test\" failed verification.\nReason: signer not found\n")
		fmt.Fprintf(os.Stderr, "To trust this server in future, perhaps add this to your command line:\n    --servercert %s\n", pinB)
		fmt.Fprintln(os.Stderr, "Failed to open HTTPS connection to vpn.example.test")
		return 1
	case "browser":
		var browser string
		for _, a := range args {
			if b, ok := strings.CutPrefix(a, "--external-browser="); ok {
				browser = b
			}
		}
		if err := exec.Command(browser, "https://sso.example.test/login?x=1").Run(); err != nil {
			fmt.Fprintln(os.Stderr, "browser:", err)
			return 1
		}
		in.ReadString('\n') // wait to be stopped
		return 0
	case "connect":
		fmt.Fprint(os.Stderr, "Response:")
		if code, _ := in.ReadString('\n'); code != "123456\n" {
			fmt.Fprintln(os.Stderr, "Login failed.")
			return 1
		}
		fmt.Fprintln(os.Stderr, "Connected as 10.9.0.2, using SSL")
		if err := scriptTun(args); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		in.ReadString('\n') // the test asks it to end with "bye"
		fmt.Fprintln(os.Stderr, "Server closed the connection")
		return 0
	}
	return 2
}

// scriptTun does what openconnect does for --script-tun: a datagram socket
// pair, the script run through /bin/sh -c with VPNFD and the tunnel's
// settings, and the far end of the pair as the gateway, here a netstack at
// 10.9.0.1 with a TCP echo on port 7.
func scriptTun(args []string) error {
	var script string
	for _, a := range args {
		if s, ok := strings.CutPrefix(a, "--script="); ok {
			script = s
		}
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "VPNFD=4", "INTERNAL_IP4_ADDRESS=10.9.0.2", "INTERNAL_IP4_MTU=1400",
		"CISCO_SPLIT_INC=1", "CISCO_SPLIT_INC_0_ADDR=10.9.0.0", "CISCO_SPLIT_INC_0_MASKLEN=24", "CISCO_BANNER=hi")
	cmd.ExtraFiles = []*os.File{os.NewFile(3, "handoff"), os.NewFile(uintptr(fds[1]), "vpnfd")}
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tun script: %w", err)
	}
	syscall.Close(fds[1])

	dev, tnet, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.9.0.1")}, nil, 1400)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fds[0]), "gateway")
	go func() {
		buf := make([]byte, 65535)
		for {
			n, err := f.Read(buf)
			if err != nil {
				return
			}
			dev.Write([][]byte{buf[:n]}, 0)
		}
	}()
	go func() {
		bufs, sizes := [][]byte{make([]byte, 65535)}, []int{0}
		for {
			if _, err := dev.Read(bufs, sizes, 0); err != nil {
				return
			}
			f.Write(bufs[0][:sizes[0]])
		}
	}()
	ln, err := tnet.ListenTCPAddrPort(netip.MustParseAddrPort("10.9.0.1:7"))
	if err != nil {
		return err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return nil
}

// run starts an engine against the fake in mode and returns what it
// reports. The engine believes hostbay-vpn lives in a directory whose name
// needs shell quoting.
func run(t *testing.T, mode, typ, conf string, secrets map[string]string) (*Engine, <-chan proto.Msg, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "Host Bay", "it's")
	os.MkdirAll(dir, 0o700)
	if err := os.Symlink(exe, filepath.Join(dir, "hostbay-vpn")); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "argv")
	t.Setenv(fakeEnv, mode)
	t.Setenv(fakeLog, log)
	oldPath, oldSelf := openconnectPath, selfPath
	openconnectPath = func() (string, error) { return exe, nil }
	selfPath = func() (string, error) { return filepath.Join(dir, "hostbay-vpn"), nil }
	t.Cleanup(func() { openconnectPath, selfPath = oldPath, oldSelf })

	e, err := New(typ, conf, secrets)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	msgs := make(chan proto.Msg, 32)
	done := make(chan struct{})
	go func() { e.Run(ctx, func(m proto.Msg) { msgs <- m }); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	return e, msgs, log
}

func next(t *testing.T, msgs <-chan proto.Msg) proto.Msg {
	t.Helper()
	select {
	case m := <-msgs:
		if strings.Contains(fmt.Sprint(m), testPass) {
			t.Fatalf("message carries the password: %+v", m)
		}
		return m
	case <-time.After(10 * time.Second):
		t.Fatal("no message from the engine")
		return proto.Msg{}
	}
}

func argvs(t *testing.T, log string) string {
	b, _ := os.ReadFile(log)
	return string(b)
}

// AE8: a changed gateway certificate stops the connect with both
// fingerprints, and openconnect, which checks the certificate in the TLS
// handshake before any sign-in form, never sends the password.
func TestChangedPinStops(t *testing.T) {
	_, msgs, _ := run(t, "cert-changed", proto.TypeAnyConnect,
		`{"gateway":"vpn.example.test","user":"alice","serverCert":"`+pinA+`"}`, map[string]string{"password": testPass})
	if m := next(t, msgs); m.State != proto.Connecting {
		t.Fatalf("got %+v", m)
	}
	m := next(t, msgs)
	if m.State != proto.Failed || m.Code != proto.CodeCertChanged || m.ServerCert != pinB ||
		!strings.Contains(m.Reason, pinA) || !strings.Contains(m.Reason, pinB) {
		t.Fatalf("got %+v, want failed cert-changed naming both pins", m)
	}
}

func TestUnknownCertStops(t *testing.T) {
	_, msgs, _ := run(t, "cert-unknown", proto.TypeGlobalProtect,
		`{"gateway":"vpn.example.test","user":"alice"}`, map[string]string{"password": testPass})
	next(t, msgs) // connecting
	m := next(t, msgs)
	if m.State != proto.Failed || m.Code != proto.CodeCertUnknown || m.ServerCert != pinB || !strings.Contains(m.Reason, pinB) {
		t.Fatalf("got %+v, want failed cert-unknown with its pin", m)
	}
}

func TestOldOpenConnectRefused(t *testing.T) {
	_, msgs, log := run(t, "old", proto.TypeF5, `{"gateway":"vpn.example.test"}`, map[string]string{"password": testPass})
	next(t, msgs) // connecting
	m := next(t, msgs)
	if m.State != proto.Failed || !strings.Contains(m.Reason, "9.12") || !strings.Contains(m.Reason, "9.01") {
		t.Fatalf("got %+v, want failed naming both versions", m)
	}
	if m.Code != proto.CodeOpenConnect {
		t.Fatalf("got code %q, want %q", m.Code, proto.CodeOpenConnect)
	}
	if got := argvs(t, log); got != "[\"--version\"]\n" {
		t.Fatalf("openconnect ran as %s, want only --version", got)
	}
}

func TestConnectDialAndDrop(t *testing.T) {
	e, msgs, log := run(t, "connect", proto.TypeSSLVPN,
		`{"gateway":"vpn.example.test","user":"alice","serverCert":"`+pinA+`"}`, map[string]string{"password": testPass})
	next(t, msgs) // connecting
	if m := next(t, msgs); m.T != proto.Need || m.Need != proto.NeedOTP {
		t.Fatalf("got %+v, want need otp", m)
	}
	e.Answer("123456")
	if m := next(t, msgs); m.State != proto.Connecting {
		t.Fatalf("got %+v, want connecting after the answer", m)
	}
	m := next(t, msgs)
	if m.State != proto.Connected || !slices.Equal(m.Address, []string{"10.9.0.2/32"}) || !slices.Equal(m.Routes, []string{"10.9.0.0/24"}) {
		t.Fatalf("got %+v, want connected", m)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := e.Dial(ctx, "tcp", "10.9.0.1:7")
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	c.Write([]byte("hello"))
	buf := make([]byte, 5)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("echo %q %v", buf, err)
	}
	c.Close()

	e.Answer("bye") // the fake ends the session
	next(t, msgs)   // connecting
	if m := next(t, msgs); m.State != proto.Dropped || !strings.Contains(m.Reason, "Server closed the connection") {
		t.Fatalf("got %+v, want dropped with openconnect's reason", m)
	}
	if _, err := e.Dial(ctx, "tcp", "10.9.0.1:7"); err == nil {
		t.Fatal("dial worked after the drop")
	}

	got := argvs(t, log)
	if strings.Contains(got, testPass) || !strings.Contains(got, `"--protocol=fortinet"`) || !strings.Contains(got, `"--non-inter"`) {
		t.Fatalf("argv:\n%s", got)
	}
}

func TestBrowserNeed(t *testing.T) {
	_, msgs, _ := run(t, "browser", proto.TypeAnyConnect, `{"gateway":"vpn.example.test","browser":true}`, nil)
	next(t, msgs) // connecting
	if m := next(t, msgs); m.T != proto.Need || m.Need != proto.NeedBrowser || m.URL != "https://sso.example.test/login?x=1" {
		t.Fatalf("got %+v, want need browser with the URL", m)
	}
}

// The descriptor the tun-fd child sends arrives in the parent and carries
// packets both ways.
func TestHandoffCarriesTheDescriptor(t *testing.T) {
	stream, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	dgram, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	child, parent := unixConn(t, stream[0]), unixConn(t, stream[1])
	far := os.NewFile(uintptr(dgram[0]), "far")
	defer far.Close()

	if err := send(child, handoff{Env: map[string]string{"INTERNAL_IP4_ADDRESS": "10.9.0.2"}}, dgram[1]); err != nil {
		t.Fatal(err)
	}
	syscall.Close(dgram[1]) // the child exits; the parent's copy keeps the socket open
	child.Close()

	var got *os.File
	var env map[string]string
	receive(parent, func(h handoff, f *os.File) { got, env = f, h.Env })
	if got == nil || env["INTERNAL_IP4_ADDRESS"] != "10.9.0.2" {
		t.Fatalf("got %v %v", got, env)
	}
	defer got.Close()
	far.Write([]byte("to parent"))
	buf := make([]byte, 64)
	if n, err := got.Read(buf); err != nil || string(buf[:n]) != "to parent" {
		t.Fatalf("read %q %v", buf[:n], err)
	}
	got.Write([]byte("to openconnect"))
	if n, err := far.Read(buf); err != nil || string(buf[:n]) != "to openconnect" {
		t.Fatalf("read %q %v", buf[:n], err)
	}
}

func unixConn(t *testing.T, fd int) *net.UnixConn {
	t.Helper()
	f := os.NewFile(uintptr(fd), "unix")
	c, err := net.FileConn(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	return c.(*net.UnixConn)
}
