package tunnel

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/tun/netstack"
	"golang.zx2c4.com/wireguard/tun/tuntest"

	"hostbay/internal/netd"
	"hostbay/internal/vpn/openvpn"
)

const handle = "AB12CD34EF"

// fakeProc is a client that runs until it is stopped.
type fakeProc struct {
	stopped chan struct{}
	once    sync.Once
	onStop  func()
}

func newProc() *fakeProc { return &fakeProc{stopped: make(chan struct{})} }

func (p *fakeProc) Wait() error { <-p.stopped; return nil }
func (p *fakeProc) Kill() error { return p.Stop() }
func (p *fakeProc) Stop() error {
	p.once.Do(func() {
		if p.onStop != nil {
			p.onStop()
		}
		close(p.stopped)
	})
	return nil
}

type started struct {
	path  string
	args  []string
	stdin string
}

// env is a Manager over a temporary helper directory holding fake client
// copies the current user owns, with every system call faked.
type env struct {
	m      *Manager
	dir    string
	starts chan started
	events chan netd.Msg
	// run, when set, plays the client: it gets the start and its stdout.
	run func(s started, stdout io.Writer, p *fakeProc)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	// Short: a Unix socket's path is at most 104 bytes on macOS.
	dir, err := os.MkdirTemp("/tmp", "nd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	os.Chmod(dir, 0o755)
	cdir := filepath.Dir(netd.ClientPath(dir, "openvpn"))
	os.MkdirAll(cdir, 0o755)
	for _, c := range []string{"openvpn", "openconnect", "openvpn-aws"} {
		os.WriteFile(netd.ClientPath(dir, c), []byte("#!/bin/false\n"), 0o755)
	}
	os.WriteFile(netd.OCScriptPath(dir), []byte("// openconnect's Windows script wrapper\n"), 0o644)
	e := &env{dir: dir, starts: make(chan started, 4), events: make(chan netd.Msg, 8)}
	e.m = New(netd.TunnelEnv{Dir: dir, User: strconv.Itoa(os.Getuid()), Owner: os.Getuid()}).(*Manager)
	e.m.start = func(path string, args []string, stdin io.Reader, stdout io.Writer) (process, error) {
		s := started{path: path, args: args}
		if stdin != nil {
			b, _ := io.ReadAll(stdin)
			s.stdin = string(b)
		}
		e.starts <- s
		p := newProc()
		if e.run != nil {
			e.run(s, stdout, p)
		}
		return p, nil
	}
	t.Cleanup(e.m.StopAll)
	return e
}

// A client starts with a fixed environment, inheriting none of the
// helper's, and with an OpenSSL configuration and GnuTLS priority file of
// the helper's own, so a Homebrew OpenSSL or GnuTLS running as root never
// reads its user-writable configuration or loads providers and engines
// from beside it.
func TestClientsInheritNoEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps the system variables processes need")
	}
	t.Setenv("OPENSSL_CONF", "/tmp/user-writable/openssl.cnf")
	t.Setenv("HB_SENTINEL", "x")
	dir := t.TempDir()
	m := New(netd.TunnelEnv{Dir: dir, User: strconv.Itoa(os.Getuid()), Owner: os.Getuid()}).(*Manager)
	var out strings.Builder
	p, err := m.start("/usr/bin/env", nil, nil, &out)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "run", "openssl.cnf")
	prio := filepath.Join(dir, "run", "gnutls.config")
	libs := filepath.Dir(netd.ClientPath(dir, "openvpn"))
	want := []string{"GNUTLS_SYSTEM_PRIORITY_FILE=" + prio, "LC_ALL=C", "OPENSSL_CONF=" + conf, "OPENSSL_ENGINES=" + libs, "OPENSSL_MODULES=" + libs, "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "XML_CATALOG_FILES= "}
	got := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("client environment = %q, want %q", got, want)
	}
	for _, f := range []string{conf, prio} {
		if fi, err := os.Stat(f); err != nil || fi.Size() != 0 || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v, want an empty file only root reads", f, err)
		}
	}
}

func (e *env) event(t *testing.T) netd.Msg {
	t.Helper()
	select {
	case m := <-e.events:
		return m
	case <-time.After(10 * time.Second):
		t.Fatal("no tunnel event")
	}
	return netd.Msg{}
}

func (e *env) start(m netd.Msg) (netd.Msg, *netd.Error) {
	m.Tunnel = handle
	return e.m.Start(m, func(ev netd.Msg) { e.events <- ev })
}

// The helper writes the configuration itself, readable by root alone, and
// puts the management interface on a socket in its root-owned directory
// that only the installing user may use (KTD8).
func TestOpenVPNManagementSocketIsTheInstallingUsers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses a loopback port")
	}
	e := newEnv(t)
	sock := filepath.Join(e.dir, "run", handle+".sock")
	e.run = func(s started, _ io.Writer, p *fakeProc) { // OpenVPN opens its socket first
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Error(err)
			return
		}
		p.onStop = func() { ln.Close() }
	}
	r, err := e.start(netd.Msg{T: netd.OpenVPNStart, OpenVPN: &openvpn.Profile{Remote: "vpn.example.test", UserPass: true, Extra: "peer-fingerprint AA:BB"}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Mgmt != sock {
		t.Fatalf("management at %q, want %q", r.Mgmt, sock)
	}
	s := <-e.starts
	conf := filepath.Join(e.dir, "run", handle+".conf")
	if s.path != netd.ClientPath(e.dir, "openvpn") || !slices.Equal(s.args, []string{"--config", conf}) {
		t.Fatalf("started %q %q", s.path, s.args)
	}
	b, _ := os.ReadFile(conf)
	me, _ := user.Current()
	for _, want := range []string{`management "` + sock + `" unix`, "management-client-user " + me.Username, "auth-user-pass\n"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("configuration lacks %q:\n%s", want, b)
		}
	}
	for path, mode := range map[string]os.FileMode{conf: 0o600, sock: 0o600} {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v, mode %v", path, err, fi.Mode())
		}
	}
	if err := e.m.Stop(handle); err != nil {
		t.Fatal(err)
	}
	if ev := e.event(t); ev.State != netd.TunnelEnded || ev.Tunnel != handle {
		t.Fatalf("event %+v", ev)
	}
	if _, err := os.Stat(conf); !os.IsNotExist(err) {
		t.Error("the configuration outlived its tunnel")
	}
}

// A profile carrying a script hook or raising script-security is refused
// by the helper itself, whatever Hostbay sent, and OpenVPN never starts.
func TestOpenVPNScriptsRefusedByTheHelper(t *testing.T) {
	e := newEnv(t)
	for _, extra := range []string{"up /bin/sh", "script-security 2", "proto udp\n<ca>\nx\n</ca>\nplugin /tmp/x.so"} {
		_, err := e.start(netd.Msg{T: netd.OpenVPNStart, OpenVPN: &openvpn.Profile{Remote: "vpn.example.test", Extra: extra}})
		if err == nil || err.Code != netd.CodeRefused {
			t.Errorf("%q: %v", extra, err)
		}
	}
	if len(e.starts) != 0 {
		t.Fatal("OpenVPN started")
	}
	if es, _ := os.ReadDir(filepath.Join(e.dir, "run")); len(es) != 2 || es[0].Name() != "gnutls.config" || es[1].Name() != "openssl.cnf" { // the clients' shared ones
		t.Errorf("a refused profile left %v behind", es)
	}
}

// An AWS SSO profile runs on the patched client; every other OpenVPN
// profile keeps the ordinary one.
func TestOpenVPNAWSRunsThePatchedClient(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses a loopback port")
	}
	for _, c := range []struct {
		p    openvpn.Profile
		want string
	}{
		{openvpn.Profile{AWS: true, Remote: "203.0.113.7", Port: 443, Extra: "peer-fingerprint AA:BB"}, "openvpn-aws"},
		{openvpn.Profile{Remote: "vpn.example.test", UserPass: true, Extra: "peer-fingerprint AA:BB"}, "openvpn"},
	} {
		e := newEnv(t)
		sock := filepath.Join(e.dir, "run", handle+".sock")
		e.run = func(s started, _ io.Writer, p *fakeProc) {
			ln, err := net.Listen("unix", sock)
			if err != nil {
				t.Error(err)
				return
			}
			p.onStop = func() { ln.Close() }
		}
		if _, err := e.start(netd.Msg{T: netd.OpenVPNStart, OpenVPN: &c.p}); err != nil {
			t.Fatal(err)
		}
		if s := <-e.starts; s.path != netd.ClientPath(e.dir, c.want) || s.args[0] != "--config" {
			t.Errorf("started %q %q, want %s", s.path, s.args, c.want)
		}
		e.m.StopAll()
	}
}

// The patched client is checked as the ordinary one is.
func TestUntrustedAWSClientRefused(t *testing.T) {
	e := newEnv(t)
	os.Chmod(netd.ClientPath(e.dir, "openvpn-aws"), 0o777)
	_, err := e.start(netd.Msg{T: netd.OpenVPNStart, OpenVPN: &openvpn.Profile{AWS: true, Remote: "203.0.113.7"}})
	if runtime.GOOS != "windows" && (err == nil || err.Code != netd.CodeRefused || len(e.starts) != 0) {
		t.Fatalf("err %v, starts %d", err, len(e.starts))
	}
}

// A client copy another user could have replaced is never started.
func TestUntrustedClientRefused(t *testing.T) {
	e := newEnv(t)
	os.Chmod(netd.ClientPath(e.dir, "openvpn"), 0o777)
	_, err := e.start(netd.Msg{T: netd.OpenVPNStart, OpenVPN: &openvpn.Profile{Remote: "vpn.example.test"}})
	if runtime.GOOS != "windows" && (err == nil || err.Code != netd.CodeRefused || len(e.starts) != 0) {
		t.Fatalf("err %v, starts %d", err, len(e.starts))
	}
}

// A system-wide WireGuard tunnel runs on the TUN the helper created, with
// the profile's addresses on it, and is up after its first handshake.
func TestSystemWireGuardUpAfterFirstHandshake(t *testing.T) {
	e := newEnv(t)
	client, server := key(t), key(t)
	port := peer(t, server, client.pub)
	var addrs []netip.Prefix
	e.m.createTUN = func(string, int) (tun.Device, error) { return tuntest.NewChannelTUN().TUN(), nil }
	e.m.setAddrs = func(_ tun.Device, _ string, a []netip.Prefix) error { addrs = a; return nil }
	conf := fmt.Sprintf("[Interface]\nAddress = 10.0.0.1/32\n[Peer]\nPublicKey = %s\nEndpoint = 127.0.0.1:%d\nAllowedIPs = 10.0.0.2/32\n", b64(server.pub), port)
	r, err := e.start(netd.Msg{T: netd.WireGuardStart, WireGuard: &netd.WireGuard{Conf: conf, PrivateKey: b64(client.priv)}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Iface != "loopbackTun1" || len(addrs) != 1 || addrs[0].String() != "10.0.0.1/32" {
		t.Fatalf("reply %+v, addresses %v", r, addrs)
	}
	if ev := e.event(t); ev.State != netd.TunnelUp || ev.Iface != "loopbackTun1" || !slices.Equal(ev.Address, []string{"10.0.0.1/32"}) {
		t.Fatalf("event %+v", ev)
	}
	e.m.Stop(handle)
	if ev := e.event(t); ev.State != netd.TunnelEnded {
		t.Fatalf("event %+v", ev)
	}
}

type keypair struct{ priv, pub []byte }

func key(t *testing.T) keypair {
	priv := make([]byte, 32)
	rand.Read(priv)
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		t.Fatal(err)
	}
	return keypair{priv, pub}
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// peer is the far end: a netstack WireGuard device on loopback; it returns
// its port.
func peer(t *testing.T, k keypair, client []byte) int {
	t.Helper()
	tunDev, _, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.0.0.2")}, nil, 1420)
	if err != nil {
		t.Fatal(err)
	}
	dev := device.NewDevice(tunDev, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))
	t.Cleanup(dev.Close)
	if err := dev.IpcSet("private_key=" + hex.EncodeToString(k.priv) + "\nlisten_port=0\npublic_key=" + hex.EncodeToString(client) + "\nallowed_ip=10.0.0.1/32\n"); err != nil {
		t.Fatal(err)
	}
	if err := dev.Up(); err != nil {
		t.Fatal(err)
	}
	get, _ := dev.IpcGet()
	for line := range strings.Lines(get) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "listen_port="); ok {
			port, _ := strconv.Atoi(v)
			return port
		}
	}
	t.Fatal("no port")
	return 0
}
