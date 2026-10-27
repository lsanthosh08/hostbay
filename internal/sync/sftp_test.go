package sync

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"hostbay/internal/apperr"
	"hostbay/internal/sftp/sftptest"
	"hostbay/internal/sshx"
)

func knownHostsFile(t *testing.T, line string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(p, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func sftpTarget(t *testing.T, addr, dir string) Target {
	host, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)
	return Target{Kind: "sftp", Host: host, Port: p, User: "u", Path: dir}
}

func TestSFTPStoresConverge(t *testing.T) {
	srv := sftptest.New(t, "pw")
	dialer := &sshx.Dialer{KnownHostsPath: knownHostsFile(t, srv.KnownHostsLine())}
	target := sftpTarget(t, srv.Addr, filepath.Join(t.TempDir(), "sync"))
	connect := func() *device {
		b, err := Connect(context.Background(), target, "pw", Deps{Dialer: dialer})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { b.Close() })
		return newDevice(t, b)
	}
	a, b := connect(), connect()
	ida := a.addHost(t, "from-a", "")
	a.sync(t)
	b.sync(t)
	idb := b.addHost(t, "from-b", "")
	b.sync(t)
	a.sync(t)
	for _, d := range []*device{a, b} {
		if got := aliases(t, d); got[ida] != "from-a" || got[idb] != "from-b" {
			t.Fatalf("got %v", got)
		}
	}
}

// A changed host key stops the dial before any credential is offered.
// The SFTP calls take no context, so the operation's context closes the
// link: a stalled server cannot hold sync past its deadline.
func TestSFTPEndsWithTheOperationsContext(t *testing.T) {
	srv := sftptest.New(t, "pw")
	dialer := &sshx.Dialer{KnownHostsPath: knownHostsFile(t, srv.KnownHostsLine())}
	ctx, cancel := context.WithCancel(context.Background())
	b, err := Connect(ctx, sftpTarget(t, srv.Addr, filepath.Join(t.TempDir(), "sync")), "pw", Deps{Dialer: dialer})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, _, err := b.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	cancel()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, _, err := b.Fetch(context.Background()); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the link outlived its context")
		}
	}
}

func TestSFTPWithChangedHostKeySendsNoCredentials(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	var attempts atomic.Int32
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			attempts.Add(1)
			return nil, nil
		},
	}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go ssh.NewServerConn(nc, cfg)
		}
	}()

	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := ssh.NewSignerFromKey(otherPriv)
	known := knownhosts.Line([]string{knownhosts.Normalize(ln.Addr().String())}, other.PublicKey())
	dialer := &sshx.Dialer{KnownHostsPath: knownHostsFile(t, known)}
	_, err = Connect(context.Background(), sftpTarget(t, ln.Addr().String(), "/tmp"), "pw", Deps{Dialer: dialer})
	if apperr.CodeOf(err) != apperr.HostKeyChanged {
		t.Fatalf("Connect = %v, want HostKeyChanged", err)
	}
	if PauseCode(err) != CodeHostKeyChanged {
		t.Fatalf("pause code = %q", PauseCode(err))
	}
	if n := attempts.Load(); n != 0 {
		t.Fatalf("the server received %d authentication attempts", n)
	}
}
