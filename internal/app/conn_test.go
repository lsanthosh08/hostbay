package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"hostbay/internal/sftp/sftptest"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"hostbay/internal/apperr"
	"hostbay/internal/secrets"
	"hostbay/internal/sshx"
)

func newConnService(t *testing.T) (*ConnService, *HostService) {
	t.Helper()
	s, creds, _ := newHostService(t, fakeVerifier(secrets.Passed))
	kh := filepath.Join(t.TempDir(), "known_hosts")
	trust := NewHostKeyTrust(sshx.NewKnownHostAppender(kh))
	mgr := sshx.NewManager(&sshx.Dialer{KnownHostsPath: kh, Timeout: sshx.DefaultTimeout})
	return NewConnService(trust, creds, mgr), s
}

func TestTargetListsJumpHostsOutermostFirst(t *testing.T) {
	c, s := newConnService(t)
	pw, auth := "pw", "password"
	bastion, err := s.SaveHost(Host{Alias: "bastion", Hostname: "10.0.0.1", Port: 22, User: "jump", AuthType: &auth, Password: &pw})
	if err != nil {
		t.Fatal(err)
	}
	web, err := s.SaveHost(Host{Alias: "web", Hostname: "10.0.0.2", Port: 2222, User: "root", AuthType: &auth, Password: &pw, JumpHostID: &bastion.ID})
	if err != nil {
		t.Fatal(err)
	}
	tg, err := c.target(web.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tg.Hops) != 2 || tg.Hops[0].Host != "10.0.0.1" || tg.Hops[1].Host != "10.0.0.2" || tg.Hops[1].Port != 2222 {
		t.Fatalf("hops = %+v", tg.Hops)
	}
	if tg.Hops[0].Auth.Password != "pw" {
		t.Fatal("jump host password was not unsealed")
	}
}

func TestTargetRefusesJumpLoop(t *testing.T) {
	c, s := newConnService(t)
	a, _ := s.SaveHost(Host{Alias: "a", Hostname: "10.0.0.1", Port: 22, User: "u"})
	b, _ := s.SaveHost(Host{Alias: "b", Hostname: "10.0.0.2", Port: 22, User: "u", JumpHostID: &a.ID})
	a.JumpHostID = &b.ID
	if _, err := s.SaveHost(a); err != nil {
		t.Fatal(err)
	}
	if _, err := c.target(a.ID); err == nil {
		t.Fatal("a jump loop was accepted")
	}
}

// An unknown host key comes back as a trust prompt with a request ID, and
// accepting it writes the key so the next connect gets past the check.
func TestUnknownHostKeyBecomesTrustPrompt(t *testing.T) {
	c, s := newConnService(t)
	addr := serveHandshakeOnly(t)
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	pw, auth := "pw", "password"
	h, err := s.SaveHost(Host{Alias: "local", Hostname: host, Port: port, User: "root", AuthType: &auth, Password: &pw})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.Connect(context.Background(), h.ID)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.HostKeyUnknown || ae.Details["requestId"] == "" || ae.Details["hop"] != "0" {
		t.Fatalf("err = %v", err)
	}
	if err := c.TrustHostKey(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	_, err = c.Connect(context.Background(), h.ID)
	if apperr.CodeOf(err) == apperr.HostKeyUnknown {
		t.Fatalf("key still unknown after trusting it: %v", err)
	}
}

// serveHandshakeOnly runs an SSH server that completes key exchange and then
// rejects every authentication attempt.
func serveHandshakeOnly(t *testing.T) string {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
		return nil, errors.New("denied")
	}}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _, _, _ = ssh.NewServerConn(conn, cfg); conn.Close() }()
		}
	}()
	return ln.Addr().String()
}

// Connecting records the MAC the host reports for Wake-on-LAN and stamps
// the host as just connected (R10).
func TestConnectRecordsMACAndLastConnected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test server runs commands with /bin/sh")
	}
	bin := t.TempDir()
	ip := "#!/bin/sh\nif [ \"$1\" = route ]; then echo '1.1.1.1 via 10.0.0.1 dev eth0 src 10.0.0.5'; " +
		"else echo '2: eth0: <UP> mtu 1500 link/ether aa:bb:cc:dd:ee:01 brd ff:ff:ff:ff:ff:ff'; fi\n"
	if err := os.WriteFile(filepath.Join(bin, "ip"), []byte(ip), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	srv := sftptest.New(t, "pw")
	s, creds, _ := newHostService(t, fakeVerifier(secrets.Passed))
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(kh, []byte(srv.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewConnService(NewHostKeyTrust(sshx.NewKnownHostAppender(kh)), creds, sshx.NewManager(&sshx.Dialer{KnownHostsPath: kh}))
	host, portStr, _ := net.SplitHostPort(srv.Addr)
	port, _ := strconv.Atoi(portStr)
	pw, auth := "pw", "password"
	h, err := s.SaveHost(Host{Alias: "web", Hostname: host, Port: port, User: "u", AuthType: &auth, Password: &pw})
	if err != nil {
		t.Fatal(err)
	}

	id, err := c.Connect(context.Background(), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Disconnect(id)
	d, err := creds.st.Data()
	if err != nil {
		t.Fatal(err)
	}
	got := d.Host(h.ID)
	if got.Mac == nil || *got.Mac != "aa:bb:cc:dd:ee:01" {
		t.Fatalf("mac = %v, want aa:bb:cc:dd:ee:01", got.Mac)
	}
	if got.LastConnected == "" {
		t.Fatal("lastConnected not stamped")
	}
}

// A host that reports no MAC is not asked again on the next connect.
func TestConnectSkipsMACDetectionAfterNothingUsable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test server runs commands with /bin/sh")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ip"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	srv := sftptest.New(t, "pw")
	s, creds, _ := newHostService(t, fakeVerifier(secrets.Passed))
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(kh, []byte(srv.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewConnService(NewHostKeyTrust(sshx.NewKnownHostAppender(kh)), creds, sshx.NewManager(&sshx.Dialer{KnownHostsPath: kh}))
	host, portStr, _ := net.SplitHostPort(srv.Addr)
	port, _ := strconv.Atoi(portStr)
	pw, auth := "pw", "password"
	h, err := s.SaveHost(Host{Alias: "web", Hostname: host, Port: port, User: "u", AuthType: &auth, Password: &pw})
	if err != nil {
		t.Fatal(err)
	}

	for range 2 {
		id, err := c.Connect(context.Background(), h.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Disconnect(id)
	}
	n := 0
	for _, cmd := range srv.Commands() {
		if cmd == detectMACCommand {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("MAC detection ran %d times, want 1", n)
	}
}

// Two connects to one saved host share its connection: stamping it as just
// connected is not an edit, so its revision, and the connection's key, stay.
func TestConnectsToOneHostShareTheConnection(t *testing.T) {
	srv := sftptest.New(t, "pw")
	s, creds, _ := newHostService(t, fakeVerifier(secrets.Passed))
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(kh, []byte(srv.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr := sshx.NewManager(&sshx.Dialer{KnownHostsPath: kh})
	c := NewConnService(NewHostKeyTrust(sshx.NewKnownHostAppender(kh)), creds, mgr)
	host, portStr, _ := net.SplitHostPort(srv.Addr)
	port, _ := strconv.Atoi(portStr)
	pw, auth, mac := "pw", "password", "aa:bb:cc:dd:ee:01" // a known MAC skips detection
	h, err := s.SaveHost(Host{Alias: "web", Hostname: host, Port: port, User: "u", AuthType: &auth, Password: &pw, Mac: &mac})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := creds.st.Data()
	rev := d.Host(h.ID).Revision

	var clients []*sshx.Client
	for range 2 {
		id, err := c.Connect(context.Background(), h.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Disconnect(id)
		clients = append(clients, mgr.Lease(id).Client())
	}
	if clients[0] == nil || clients[0] != clients[1] {
		t.Fatal("the second connect dialled a new connection")
	}
	d, _ = creds.st.Data()
	if got := d.Host(h.ID); got.LastConnected == "" || got.Revision != rev {
		t.Fatalf("lastConnected %q, revision %d; want a stamp at revision %d", got.LastConnected, got.Revision, rev)
	}
}

// changedKeyHost saves a host whose server presents a key other than the one
// known_hosts holds for it, beside an unrelated entry on the same port, and
// returns the HostKeyChanged refusal a connect to it gets.
func changedKeyHost(t *testing.T, ver secrets.Verifier) (*ConnService, string, string, *apperr.Error) {
	t.Helper()
	s, creds, _ := newHostService(t, ver)
	kh := filepath.Join(t.TempDir(), "known_hosts")
	c := NewConnService(NewHostKeyTrust(sshx.NewKnownHostAppender(kh)), creds, sshx.NewManager(&sshx.Dialer{KnownHostsPath: kh}))
	host, portStr, _ := net.SplitHostPort(serveHandshakeOnly(t))
	port, _ := strconv.Atoi(portStr)
	old, _, _ := ed25519.GenerateKey(rand.Reader)
	pub, err := ssh.NewPublicKey(old)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{host, "other.example"} {
		if err := sshx.NewKnownHostAppender(kh)(name, port, pub.Type(), pub.Marshal()); err != nil {
			t.Fatal(err)
		}
	}
	pw, auth := "pw", "password"
	h, err := s.SaveHost(Host{Alias: "rebuilt", Hostname: host, Port: port, User: "root", AuthType: &auth, Password: &pw})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Connect(context.Background(), h.ID)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.HostKeyChanged || ae.Details["removeId"] == "" || ae.Details["host"] != host {
		t.Fatalf("err = %v, want HostKeyChanged with a removeId", err)
	}
	return c, h.ID, kh, ae
}

// The old key stays in known_hosts unless the typed name matches the refused
// host and OS verification passes (R13).
func TestRemoveHostKeyNeedsTypedNameAndVerification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ver   secrets.Result
		typed func(host string) string
	}{
		{"wrong name", secrets.Passed, func(string) string { return "localhost" }},
		{"empty name", secrets.Passed, func(string) string { return "" }},
		{"verification cancelled", secrets.Cancelled, func(h string) string { return h }},
		{"verification failed", secrets.Failed, func(h string) string { return h }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, kh, refusal := changedKeyHost(t, fakeVerifier(tc.ver))
			before, _ := os.ReadFile(kh)
			err := c.RemoveHostKey(context.Background(), refusal.Details["removeId"], tc.typed(refusal.Details["host"]))
			if err == nil || (tc.ver != secrets.Passed) != (apperr.CodeOf(err) == apperr.VerifyFailed) {
				t.Fatalf("RemoveHostKey = %v", err)
			}
			if after, _ := os.ReadFile(kh); string(after) != string(before) {
				t.Fatalf("known_hosts changed:\n%s", after)
			}
		})
	}
}

// Removing the old key drops only that host's line, so the next connect asks
// to trust the new key instead of refusing it.
func TestRemoveHostKeyLetsTheNewKeyBeTrusted(t *testing.T) {
	c, hostID, kh, refusal := changedKeyHost(t, fakeVerifier(secrets.Passed))
	id := refusal.Details["removeId"]
	if err := c.RemoveHostKey(context.Background(), "nope", refusal.Details["host"]); err == nil {
		t.Fatal("RemoveHostKey with an unissued ID = nil, want error")
	}
	if err := c.RemoveHostKey(context.Background(), id, refusal.Details["host"]); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(kh)
	if lines := strings.Split(strings.TrimSpace(string(data)), "\n"); len(lines) != 1 || !strings.HasPrefix(lines[0], "[other.example]:") {
		t.Fatalf("known_hosts =\n%s\nwant only the other.example line", data)
	}
	if _, err := c.Connect(context.Background(), hostID); apperr.CodeOf(err) != apperr.HostKeyUnknown {
		t.Fatalf("connect after removal: err = %v, want HostKeyUnknown", err)
	}
	if err := c.RemoveHostKey(context.Background(), id, refusal.Details["host"]); err == nil {
		t.Fatal("a used removeId was accepted again")
	}
}

// Settings lists known_hosts and removes an entry only when the typed name
// matches and OS verification passes; otherwise the file is unchanged (R13).
func TestRemoveKnownHostFromSettings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ver   secrets.Result
		typed string
	}{
		{"mistyped name", secrets.Passed, "other.exampl"},
		{"empty name", secrets.Passed, ""},
		{"verification cancelled", secrets.Cancelled, "other.example"},
		{"verification failed", secrets.Failed, "other.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, kh, _ := changedKeyHost(t, fakeVerifier(tc.ver))
			before, _ := os.ReadFile(kh)
			err := c.RemoveKnownHost(context.Background(), "other.example", 22, tc.typed)
			if err == nil || (tc.ver != secrets.Passed) != (apperr.CodeOf(err) == apperr.VerifyFailed) {
				t.Fatalf("RemoveKnownHost = %v", err)
			}
			if after, _ := os.ReadFile(kh); string(after) != string(before) {
				t.Fatalf("known_hosts changed:\n%s", after)
			}
		})
	}

	c, _, _, refusal := changedKeyHost(t, fakeVerifier(secrets.Passed))
	list, err := c.ListKnownHosts()
	if err != nil || len(list) != 2 || list[1].Host != "other.example" {
		t.Fatalf("ListKnownHosts = %+v, %v; want the refused host and other.example", list, err)
	}
	port := list[1].Port
	if err := c.RemoveKnownHost(context.Background(), "other.example", port, "other.example"); err != nil {
		t.Fatal(err)
	}
	if list, _ := c.ListKnownHosts(); len(list) != 1 || list[0].Host != refusal.Details["host"] {
		t.Fatalf("after removal: %+v; want only the refused host", list)
	}
}

// Reconnect rebuilds a session's shared connection from the banner; a
// session that has ended is an error, not a silent success.
func TestReconnect(t *testing.T) {
	srv := sftptest.New(t, "pw")
	s, creds, _ := newHostService(t, fakeVerifier(secrets.Passed))
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(kh, []byte(srv.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewConnService(NewHostKeyTrust(sshx.NewKnownHostAppender(kh)), creds, sshx.NewManager(&sshx.Dialer{KnownHostsPath: kh}))
	host, portStr, _ := net.SplitHostPort(srv.Addr)
	port, _ := strconv.Atoi(portStr)
	pw, auth, mac := "pw", "password", "aa:bb:cc:dd:ee:02"
	h, err := s.SaveHost(Host{Alias: "web", Hostname: host, Port: port, User: "u", AuthType: &auth, Password: &pw, Mac: &mac})
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.Connect(context.Background(), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Reconnect(context.Background(), id); err != nil {
		t.Fatalf("Reconnect on a live session = %v", err)
	}
	c.Disconnect(id)
	if err := c.Reconnect(context.Background(), id); err == nil {
		t.Fatal("Reconnect on a closed session = nil, want error")
	}
}
