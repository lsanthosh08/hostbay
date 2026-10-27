package app

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"hostbay/internal/sftp/sftptest"
	"hostbay/internal/sshx"
)

// newFilesSession connects to an in-process server as a password host and
// returns the files service and the session's lease.
func newFilesSession(t *testing.T, s *sftptest.Server) (*FilesService, *sshx.Lease) {
	t.Helper()
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(kh, []byte(s.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(s.Addr)
	p, _ := strconv.Atoi(port)
	mgr := sshx.NewManager(&sshx.Dialer{KnownHostsPath: kh})
	l, err := mgr.Acquire(context.Background(), sshx.Target{Key: "h1@1", Hops: []sshx.Hop{
		{Host: host, Port: p, User: "u", Auth: sshx.Auth{Password: s.Password}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Release)
	return NewFilesService(mgr), l
}

func TestSudoSaveReusesTheStoredLoginPassword(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test server runs commands with /bin/sh")
	}
	s := sftptest.New(t, "login-pw")
	f, l := newFilesSession(t, s)
	p := filepath.Join(t.TempDir(), "nginx.conf")
	if err := os.WriteFile(p, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Protect(t, p)

	if err := f.SftpWrite(l.ID(), p, "new\n", true, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "new\n" {
		t.Fatalf("content = %q", b)
	}
}

func TestFilesReuseOneSFTPClientUntilTheConnectionIsRebuilt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test server runs commands with /bin/sh")
	}
	s := sftptest.New(t, "login-pw")
	f, l := newFilesSession(t, s)
	dir := t.TempDir()
	for range 2 {
		if _, err := f.SftpList(l.ID(), dir); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.SFTPOpens(); n != 1 {
		t.Fatalf("SFTP opened %d times for two calls", n)
	}

	s.DropAll()
	deadline := time.Now().Add(2 * time.Second)
	for l.Client() != nil {
		if time.Now().After(deadline) {
			t.Fatal("drop not noticed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := f.SftpList(l.ID(), dir); err != nil {
		t.Fatal(err)
	}
	if l.Generation() != 2 || s.SFTPOpens() != 2 {
		t.Fatalf("after drop: generation %d, SFTP opens %d", l.Generation(), s.SFTPOpens())
	}
}

func TestFilesRejectsUnknownSession(t *testing.T) {
	f := NewFilesService(sshx.NewManager(&sshx.Dialer{}))
	if _, err := f.SftpList("nope", "/"); err == nil {
		t.Fatal("unknown session listed")
	}
}
