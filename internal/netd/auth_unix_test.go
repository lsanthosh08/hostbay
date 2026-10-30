//go:build darwin || linux

package netd

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// The kernel names this test process as the peer of its own connection.
func TestPeerOfNamesTheCaller(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	p, err := peerOf(s)
	if err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	self, _ = filepath.EvalSymlinks(self)
	want := Peer{UID: strconv.Itoa(os.Getuid()), Exe: self}
	if p != want {
		t.Errorf("peerOf = %+v, want %+v", p, want)
	}
	if err := authorize(p, Config{UID: want.UID, App: "/Applications/Hostbay.app/Contents/MacOS/hostbay"}); errCode(err) != CodeDenied {
		t.Errorf("a test binary passed as Hostbay: %v", err)
	}
}
