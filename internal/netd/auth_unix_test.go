//go:build darwin || linux

package netd

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// conn returns the server end of a Unix connection this test process made
// to itself.
func conn(t *testing.T) net.Conn {
	t.Helper()
	// Short: a Unix socket's path is at most 104 bytes on macOS.
	dir, err := os.MkdirTemp("/tmp", "nd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	c, err := net.Dial("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func self(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// The kernel names this test process as the peer of its own connection.
func TestPeerOfNamesTheCaller(t *testing.T) {
	p, err := peerOf(conn(t))
	if err != nil {
		t.Fatal(err)
	}
	want := Peer{UID: strconv.Itoa(os.Getuid()), Exe: self(t)}
	if p != want {
		t.Errorf("peerOf = %+v, want %+v", p, want)
	}
	if err := authorize(p, Config{UID: want.UID, App: "/Applications/Hostbay.app/Contents/MacOS/hostbay"}); errCode(err) != CodeDenied {
		t.Errorf("a test binary passed as Hostbay: %v", err)
	}
}

// Serve's caller check admits the paired executable and denies any other.
func TestAuthForAdmitsOnlyThePairedHostbay(t *testing.T) {
	me := strconv.Itoa(os.Getuid())
	if err := authFor(Config{UID: me, App: self(t)})(conn(t)); err != nil {
		t.Errorf("the paired executable was refused: %v", err)
	}
	if err := authFor(Config{UID: me, App: "/Applications/Hostbay.app/Contents/MacOS/hostbay"})(conn(t)); errCode(err) != CodeDenied {
		t.Errorf("an unpaired executable got through: %v", err)
	}
}

// The helper's socket is the installing user's alone, mode 0600.
func TestListenAtHandsTheSocketToTheInstallingUser(t *testing.T) {
	// Short: a Unix socket's path is at most 104 bytes on macOS.
	dir, err := os.MkdirTemp("/tmp", "nd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	ln, err := listenAt(path, Config{UID: strconv.Itoa(os.Getuid())})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v, want a 0600 socket", fi.Mode())
	}
	if uid, _ := fileOwner(fi); uid != os.Getuid() {
		t.Errorf("socket owner = %d, want %d", uid, os.Getuid())
	}
	if os.Getuid() == 0 {
		return
	}
	// Unprivileged, the chown to another user fails, which shows it is made.
	if ln, err := listenAt(filepath.Join(dir, "o"), Config{UID: strconv.Itoa(os.Getuid() + 1)}); err == nil {
		ln.Close()
		t.Error("the socket was not handed to the installing user")
	}
}

// A pairing the user could have written is refused.
func TestLoadConfigRefusesAUserOwnedPairing(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root owns whatever the test writes")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"uid":"501","app":"/usr/bin/python3"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Error("a user-owned helper config was accepted")
	}
}
