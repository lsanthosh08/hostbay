package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"

	"hostbay/internal/apperr"
	"hostbay/internal/secrets"
	"hostbay/internal/sshx"
	"hostbay/internal/sshx/proxytest"
	"hostbay/internal/store"
)

// countingServer accepts user "u" with password "pw" and counts the TCP
// connections and password attempts it sees.
type countingServer struct {
	host  string
	port  int
	key   ssh.PublicKey
	conns atomic.Int32
	auths atomic.Int32
}

func newCountingServer(t *testing.T) *countingServer {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	s := &countingServer{key: signer.PublicKey()}
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
		s.auths.Add(1)
		if c.User() == "u" && string(pw) == "pw" {
			return nil, nil
		}
		return nil, errors.New("denied")
	}}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s.host, s.port = "127.0.0.1", ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			s.conns.Add(1)
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
				if err != nil {
					nc.Close()
					return
				}
				go ssh.DiscardRequests(reqs)
				for nch := range chans {
					nch.Reject(ssh.Prohibited, "no channels here")
				}
			}()
		}
	}()
	return s
}

// connWithKnownHosts is newConnService with known_hosts holding lines.
func connWithKnownHosts(t *testing.T, lines ...string) (*ConnService, *HostService, string) {
	t.Helper()
	s, creds, dir := newHostService(t, fakeVerifier(secrets.Passed))
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(kh, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return NewConnService(NewHostKeyTrust(sshx.NewKnownHostAppender(kh)), creds, sshx.NewManager(&sshx.Dialer{KnownHostsPath: kh})), s, dir
}

func (s *countingServer) knownLine() string {
	return "[" + s.host + "]:" + strconv.Itoa(s.port) + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.key)))
}

func (s *countingServer) form(pw string) Host {
	auth := "password"
	return Host{Alias: "web", Hostname: s.host, Port: s.port, User: "u", AuthType: &auth, Password: &pw}
}

// A saved host reaches its server through its saved proxy, whose password
// is sealed and never shown; a proxy that is down fails the connect at the
// proxy step and the server sees no direct connection (R49, KTD22).
func TestConnectGoesThroughTheHostsProxyOnly(t *testing.T) {
	srv := newCountingServer(t)
	c, s, dir := connWithKnownHosts(t, srv.knownLine())
	p := proxytest.SOCKS5(t, "pu", "proxy-secret-9")
	ppw := "proxy-secret-9"
	px, err := s.SaveProxy(Proxy{Name: "office", Type: "socks5", Host: p.Host, Port: p.Port, User: "pu", Password: &ppw})
	if err != nil {
		t.Fatal(err)
	}
	if !px.HasPassword || *px.Password != "" {
		t.Fatalf("SaveProxy returned password %q", *px.Password)
	}
	form := srv.form("pw")
	form.ProxyID = &px.ID
	h, err := s.SaveHost(form)
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.Connect(context.Background(), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	c.Disconnect(id)
	if want := net.JoinHostPort(srv.host, strconv.Itoa(srv.port)); !slices.Contains(p.Targets(), want) {
		t.Fatalf("proxy carried %v, want %s", p.Targets(), want)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "proxies.json"))
	if strings.Contains(string(raw), ppw) {
		t.Fatal("proxies.json holds the proxy password in the clear")
	}

	down, port := proxytest.Down(t)
	before := srv.conns.Load()
	dead, err := s.SaveProxy(Proxy{Name: "dead", Type: "http", Host: down, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	h.ProxyID = &dead.ID
	if _, err := s.SaveHost(h); err != nil {
		t.Fatal(err)
	}
	_, err = c.Connect(context.Background(), h.ID)
	if apperr.CodeOf(err) != apperr.ProxyFailed {
		t.Fatalf("err = %v, want the proxy step to fail", err)
	}
	if srv.conns.Load() != before {
		t.Fatal("the server saw a direct connection")
	}

	// A deleted proxy is a failure too, never a reason to dial directly.
	if err := s.DeleteProxy(dead.ID); err != nil {
		t.Fatal(err)
	}
	_, err = c.Connect(context.Background(), h.ID)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.ProxyFailed || ae.Details["reason"] != "missing" {
		t.Fatalf("err = %v, want ProxyFailed reason missing", err)
	}
	if srv.conns.Load() != before {
		t.Fatal("the server saw a direct connection after its proxy was deleted")
	}
}

// TestConnection reports ok, a trust prompt for an unknown key and a
// failed sign-in, and saves nothing (R53).
func TestTestConnectionReportsEachStep(t *testing.T) {
	srv := newCountingServer(t)
	c, s, _ := connWithKnownHosts(t, srv.knownLine())
	if r, err := c.TestConnection(context.Background(), srv.form("pw")); err != nil || r.Step != "ok" {
		t.Fatalf("good host: %+v, %v", r, err)
	}
	if r, _ := c.TestConnection(context.Background(), srv.form("wrong")); r.Step != "auth" {
		t.Fatalf("wrong password: %+v", r)
	}
	other := newCountingServer(t)
	r, _ := c.TestConnection(context.Background(), other.form("pw"))
	if r.Step != "hostKey" || r.Error == nil || r.Error.Code != apperr.HostKeyUnknown || r.Error.Details["requestId"] == "" {
		t.Fatalf("unknown key: %+v", r)
	}
	host, port := proxytest.Down(t)
	r, _ = c.TestConnection(context.Background(), Host{Alias: "x", Hostname: host, Port: port, User: "u", AuthType: new("password"), Password: new("pw")})
	if r.Step != "tcp" {
		t.Fatalf("closed port: %+v", r)
	}
	if hosts, _ := s.ListHosts(); len(hosts) != 0 {
		t.Fatalf("testing saved %d hosts", len(hosts))
	}
}

// Pointing a saved host elsewhere and testing without retyping the
// password never decrypts the saved one or sends it to the new address
// (KTD6).
func TestTestConnectionNeverSendsTheSavedPasswordElsewhere(t *testing.T) {
	srv, other := newCountingServer(t), newCountingServer(t)
	c, s, _ := connWithKnownHosts(t, srv.knownLine(), other.knownLine())
	h, err := s.SaveHost(srv.form("pw"))
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := c.TestConnection(context.Background(), h); r.Step != "ok" {
		t.Fatalf("unchanged host with its saved password: %+v", r)
	}
	h.Port = other.port
	r, _ := c.TestConnection(context.Background(), h)
	if r.Step != "auth" || r.Error == nil || r.Error.Code != apperr.SecretNeeded {
		t.Fatalf("moved host: %+v", r)
	}
	if n := other.auths.Load(); n != 0 {
		t.Fatalf("the new address saw %d password attempts", n)
	}
}

// A raw TCP host's terminal echoes through an echo server (R48).
func TestRawTCPHostOpensTerminal(t *testing.T) {
	c, s := newConnService(t)
	ev := &termEvents{}
	c.emit = ev.emit
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			io.Copy(conn, conn)
		}
	}()
	h, err := s.SaveHost(Host{Alias: "echo", Hostname: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, Kind: new("tcp")})
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.PtyOpenTCP(context.Background(), "tab-1", h.ID)
	if err != nil {
		t.Fatal(err)
	}
	c.PtyAttach(id)
	c.PtyWrite(id, "ping\n")
	waitUntil(t, "the echo", func() bool { return strings.Contains(ev.joined(PtyDataEvent(id)), "ping") })
	c.Disconnect("tab-1")
}

// The host's extras reach its hop: agent and X11 forwarding only when the
// host turns them on, the sign-in method, legacy algorithms and keepalive.
func TestTargetCarriesTheHostsExtras(t *testing.T) {
	c, s := newConnService(t)
	plain, err := s.SaveHost(Host{Alias: "a", Hostname: "10.0.0.1", Port: 22, User: "u", AuthType: new("agent")})
	if err != nil {
		t.Fatal(err)
	}
	tg, _ := c.target(plain.ID)
	if hop := tg.Hops[0]; hop.ForwardAgent || hop.X11 || !hop.Auth.Agent || tg.Keepalive != 0 {
		t.Fatalf("defaults: %+v keepalive %v", hop, tg.Keepalive)
	}
	algos := &store.Algorithms{KeyExchanges: []string{ssh.InsecureKeyExchangeDH1SHA1}}
	on, err := s.SaveHost(Host{Alias: "b", Hostname: "10.0.0.2", Port: 22, User: "u", AuthType: new("kerberos"),
		ForwardAgent: new(true), X11: new(true), Algorithms: algos, Keepalive: new(5)})
	if err != nil {
		t.Fatal(err)
	}
	tg, _ = c.target(on.ID)
	hop := tg.Hops[0]
	if !hop.ForwardAgent || !hop.X11 || !hop.Auth.Kerberos || !reflect.DeepEqual(hop.Algorithms.KeyExchanges, algos.KeyExchanges) || tg.Keepalive.Seconds() != 5 {
		t.Fatalf("opted in: %+v keepalive %v", hop, tg.Keepalive)
	}
	if _, err := s.SaveHost(Host{Alias: "c", Hostname: "h", Port: 22, User: "u", Algorithms: &store.Algorithms{Ciphers: []string{"rot13"}}}); err == nil {
		t.Fatal("an unknown cipher was saved")
	}
}
