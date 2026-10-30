package wg

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"hostbay/internal/vpn/proto"
	"hostbay/internal/vpn/socks"
)

type keypair struct{ priv, pub []byte }

func newKey(t *testing.T) keypair {
	t.Helper()
	priv := make([]byte, 32)
	rand.Read(priv)
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		t.Fatal(err)
	}
	return keypair{priv, pub}
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// server is the far peer: a raw netstack WireGuard device at 10.0.0.2 with a
// TCP and a UDP echo on port 7.
type server struct {
	dev  *device.Device
	port int
}

func newServer(t *testing.T, key keypair, client []byte) *server {
	t.Helper()
	tunDev, tnet, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.0.0.2")}, nil, 1420)
	if err != nil {
		t.Fatal(err)
	}
	dev := device.NewDevice(tunDev, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))
	t.Cleanup(dev.Close)
	if err := dev.IpcSet("private_key=" + hex.EncodeToString(key.priv) + "\nlisten_port=0\npublic_key=" +
		hex.EncodeToString(client) + "\nallowed_ip=10.0.0.1/32\n"); err != nil {
		t.Fatal(err)
	}
	if err := dev.Up(); err != nil {
		t.Fatal(err)
	}
	get, _ := dev.IpcGet()
	var port int
	for line := range strings.Lines(get) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "listen_port="); ok {
			port, _ = strconv.Atoi(v)
		}
	}

	ln, err := tnet.ListenTCPAddrPort(netip.MustParseAddrPort("10.0.0.2:7"))
	if err != nil {
		t.Fatal(err)
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
	pc, err := tnet.ListenUDPAddrPort(netip.MustParseAddrPort("10.0.0.2:7"))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(buf[:n], from)
		}
	}()
	return &server{dev: dev, port: port}
}

func clientConf(key keypair, server []byte, endpoint string) string {
	return "[Interface]\nPrivateKey = " + b64(key.priv) + "\nAddress = 10.0.0.1/32\n\n" +
		"[Peer]\nPublicKey = " + b64(server) + "\nEndpoint = " + endpoint + "\nAllowedIPs = 10.0.0.0/24\n"
}

// run starts e and returns its state messages as they come.
func run(t *testing.T, e *Engine) <-chan proto.Msg {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	states := make(chan proto.Msg, 16)
	done := make(chan struct{})
	go func() {
		e.Run(ctx, func(m proto.Msg) { states <- m })
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return states
}

func waitState(t *testing.T, states <-chan proto.Msg, want string, within time.Duration) proto.Msg {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case m := <-states:
			if m.State == want {
				return m
			}
			if m.State != proto.Connecting {
				t.Fatalf("state %q (%s), want %q", m.State, m.Reason, want)
			}
		case <-deadline:
			t.Fatalf("no %q state within %v", want, within)
		}
	}
}

func socksLogin(t *testing.T, addr, user, pass string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	// Generous: under a loaded parallel test run, TCP through the tunnel backs off.
	c.SetDeadline(time.Now().Add(30 * time.Second))
	c.Write([]byte{5, 1, 2})
	c.Write(append(append(append([]byte{1, byte(len(user))}, user...), byte(len(pass))), pass...))
	var rep [4]byte
	if _, err := io.ReadFull(c, rep[:]); err != nil || rep[1] != 2 || rep[3] != 0 {
		t.Fatalf("login %v %v", rep, err)
	}
	return c
}

func socksRequest(t *testing.T, c net.Conn, cmd byte, ip [4]byte, port uint16) (byte, *net.UDPAddr) {
	t.Helper()
	c.Write(binary.BigEndian.AppendUint16(append([]byte{5, cmd, 0, 1}, ip[:]...), port))
	var rep [10]byte
	if _, err := io.ReadFull(c, rep[:]); err != nil {
		t.Fatal(err)
	}
	return rep[1], &net.UDPAddr{IP: net.IP(rep[4:8]), Port: int(binary.BigEndian.Uint16(rep[8:]))}
}

// connect brings up a far peer and an engine dialing it, and waits for
// connected.
func connect(t *testing.T) (*server, *Engine, <-chan proto.Msg, proto.Msg) {
	t.Helper()
	ck, sk := newKey(t), newKey(t)
	srv := newServer(t, sk, ck.pub)
	e, err := New(clientConf(ck, sk.pub, "127.0.0.1:"+strconv.Itoa(srv.port)), nil)
	if err != nil {
		t.Fatal(err)
	}
	states := run(t, e)
	// Keepalive starts the handshake before any traffic.
	return srv, e, states, waitState(t, states, proto.Connected, 15*time.Second)
}

func TestTwoPeersThroughSOCKS(t *testing.T) {
	defer func(p time.Duration) { pollEvery = p }(pollEvery)
	pollEvery = 50 * time.Millisecond

	_, e, _, m := connect(t)
	if !slices.Equal(m.Address, []string{"10.0.0.1/32"}) || !slices.Equal(m.Routes, []string{"10.0.0.0/24"}) {
		t.Fatalf("connected with address %q routes %q", m.Address, m.Routes)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go (&socks.Server{User: "u", Pass: "p", Dial: e.Dial}).Serve(ln)

	c := socksLogin(t, ln.Addr().String(), "u", "p")
	if code, _ := socksRequest(t, c, 1, [4]byte{10, 0, 0, 2}, 7); code != 0 {
		t.Fatalf("CONNECT reply %#x", code)
	}
	c.Write([]byte("across"))
	buf := make([]byte, 2048)
	if _, err := io.ReadFull(c, buf[:6]); err != nil || string(buf[:6]) != "across" {
		t.Fatalf("TCP echo %q %v", buf[:6], err)
	}

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	ctl := socksLogin(t, ln.Addr().String(), "u", "p")
	code, relay := socksRequest(t, ctl, 3, [4]byte{127, 0, 0, 1}, uint16(udp.LocalAddr().(*net.UDPAddr).Port))
	if code != 0 {
		t.Fatalf("UDP ASSOCIATE reply %#x", code)
	}
	// UDP has no retransmission, so resend until the echo comes back.
	head := []byte{0, 0, 0, 1, 10, 0, 0, 2, 0, 7}
	var n int
	for try := 0; ; try++ {
		udp.WriteToUDP(append(head, "datagram"...), relay)
		udp.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		if n, err = udp.Read(buf); err == nil || try == 40 {
			break
		}
	}
	if err != nil || string(buf[len(head):n]) != "datagram" {
		t.Fatalf("UDP echo %q %v", buf[:n], err)
	}
}

func TestDroppedWhenHandshakeTooOld(t *testing.T) {
	defer func(d, p time.Duration) { dropAfter, pollEvery = d, p }(dropAfter, pollEvery)
	dropAfter, pollEvery = time.Second, 50*time.Millisecond

	srv, _, states, _ := connect(t)
	// With the far peer gone no handshake follows, and the tunnel drops.
	srv.dev.Close()
	if m := waitState(t, states, proto.Dropped, 10*time.Second); m.Reason == "" {
		t.Fatal("dropped without a reason")
	}
}

func TestUnreachableEndpointFails(t *testing.T) {
	defer func(c, p time.Duration) { connectTimeout, pollEvery = c, p }(connectTimeout, pollEvery)
	connectTimeout, pollEvery = 300*time.Millisecond, 50*time.Millisecond

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := pc.LocalAddr().String()
	pc.Close() // nothing answers here now

	ck, sk := newKey(t), newKey(t)
	e, err := New(clientConf(ck, sk.pub, endpoint), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := waitState(t, run(t, e), proto.Failed, 5*time.Second)
	if !strings.Contains(m.Reason, endpoint) {
		t.Fatalf("reason %q does not name %s", m.Reason, endpoint)
	}
}

func TestParseConf(t *testing.T) {
	ck, sk, psk := newKey(t), newKey(t), newKey(t)
	conf := `# exported from the gateway
[Interface]
PrivateKey = ` + b64(ck.priv) + `
Address = 10.0.0.1/32, fd00::1/128
DNS = 10.0.0.2, corp.example
MTU = 1380
PostUp = echo never run

[Peer]
PublicKey = ` + b64(sk.pub) + `
PresharedKey = ` + b64(psk.priv) + `
Endpoint = vpn.example.com:51820
AllowedIPs = 10.0.0.0/24, 192.168.5.0/24
PersistentKeepalive = 0
`
	cfg, err := parse(conf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.privateKey != hex.EncodeToString(ck.priv) {
		t.Errorf("private key %q", cfg.privateKey)
	}
	if got := strings.Join(fmtAll(cfg.addresses), ","); got != "10.0.0.1/32,fd00::1/128" {
		t.Errorf("addresses %s", got)
	}
	if !slices.Equal(cfg.dns, []netip.Addr{netip.MustParseAddr("10.0.0.2")}) || !slices.Equal(cfg.search, []string{"corp.example"}) {
		t.Errorf("dns %v search %v", cfg.dns, cfg.search)
	}
	if cfg.mtu != 1380 || len(cfg.peers) != 1 {
		t.Fatalf("mtu %d peers %d", cfg.mtu, len(cfg.peers))
	}
	p := cfg.peers[0]
	if p.publicKey != hex.EncodeToString(sk.pub) || p.presharedKey != hex.EncodeToString(psk.priv) {
		t.Errorf("peer keys %q %q", p.publicKey, p.presharedKey)
	}
	if p.endpoint != "vpn.example.com:51820" {
		t.Errorf("endpoint %q", p.endpoint)
	}
	if got := strings.Join(fmtAll(p.allowedIPs), ","); got != "10.0.0.0/24,192.168.5.0/24" {
		t.Errorf("allowed IPs %s", got)
	}
}

// Hostbay keeps the keys sealed apart from the .conf and sends them as secrets.
func TestParseSecretsFillKeys(t *testing.T) {
	ck, sk, psk := newKey(t), newKey(t), newKey(t)
	conf := "[Interface]\nAddress = 10.0.0.1/32\n[Peer]\nPublicKey = " + b64(sk.pub) + "\nEndpoint = 192.0.2.1:51820\nAllowedIPs = 10.0.0.0/24\n"
	cfg, err := parse(conf, map[string]string{"privateKey": b64(ck.priv), "presharedKey": b64(psk.priv)})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.privateKey != hex.EncodeToString(ck.priv) || cfg.peers[0].presharedKey != hex.EncodeToString(psk.priv) {
		t.Fatalf("keys not taken from secrets: %q %q", cfg.privateKey, cfg.peers[0].presharedKey)
	}
}

func TestParseErrorsNeverEchoKeys(t *testing.T) {
	const bad = "SECRETVALUEnotbase64!!"
	sk := newKey(t)
	for _, conf := range []string{
		"[Interface]\nPrivateKey = " + bad + "\nAddress = 10.0.0.1/32\n[Peer]\nPublicKey = " + b64(sk.pub) + "\nEndpoint = 192.0.2.1:1\nAllowedIPs = 10.0.0.0/24\n",
		"[Interface]\nPrivateKey = " + b64(sk.priv) + "\nAddress = 10.0.0.1/32\n[Peer]\nPublicKey = " + b64(sk.pub) + "\nPresharedKey = " + bad + "\nEndpoint = 192.0.2.1:1\nAllowedIPs = 10.0.0.0/24\n",
		"[Interface]\nPrivateKey " + bad + "\n",
	} {
		_, err := parse(conf, nil)
		if err == nil {
			t.Fatalf("parsed a bad key")
		}
		if strings.Contains(err.Error(), "SECRETVALUE") {
			t.Fatalf("error echoes the key: %v", err)
		}
	}
}

func TestEndpointResolvesBeforeStart(t *testing.T) {
	ck, sk := newKey(t), newKey(t)
	e, err := New(clientConf(ck, sk.pub, "localhost:51820"), nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := e.uapi(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u, "endpoint=127.0.0.1:51820\n") && !strings.Contains(u, "endpoint=[::1]:51820\n") {
		t.Fatalf("endpoint not resolved:\n%s", u)
	}
	if !strings.Contains(u, "persistent_keepalive_interval=25\n") {
		t.Fatalf("no 25 s keepalive:\n%s", u)
	}

	e, _ = New(clientConf(ck, sk.pub, "no-such-host.invalid:51820"), nil)
	if _, err := e.uapi(context.Background()); err == nil || !strings.Contains(err.Error(), "no-such-host.invalid:51820") {
		t.Fatalf("unresolvable endpoint: %v", err)
	}
}

func fmtAll(ps []netip.Prefix) []string {
	var s []string
	for _, p := range ps {
		s = append(s, p.String())
	}
	return s
}
