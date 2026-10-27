package wol

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"hostbay/internal/sftp/sftptest"
)

var wantPacket = append(bytes.Repeat([]byte{0xff}, 6),
	bytes.Repeat([]byte{0xaa, 0xbb, 0xcc, 0x0d, 0xee, 0xff}, 16)...)

func TestParseMAC(t *testing.T) {
	for _, s := range []string{"aa:bb:cc:0d:ee:ff", "AA-BB-CC-0D-EE-FF", " aa:bb:cc:d:ee:ff "} {
		mac, err := parseMAC(s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if got := magicPacket(mac); !bytes.Equal(got, wantPacket) || len(got) != 102 {
			t.Errorf("%q: packet = %x", s, got)
		}
	}
	for _, s := range []string{"", "aa:bb:cc:dd:ee", "aa:bb:cc:dd:ee:ff:00", "aa:bb:cc:dd:ee:gg",
		"aa:bb:cc:dd:ee:fff", "aa:bb:cc:dd:ee:", "aa:bb:cc:dd:ee:+f", "aa:bb:cc:dd:ee: f",
		"aa:bb:cc:dd:ee:ff;reboot", "aa:bb:cc:dd:ee:$(id)", "aabbccddeeff"} {
		if _, err := parseMAC(s); err == nil {
			t.Errorf("parseMAC(%q) accepted", s)
		}
	}
}

func TestWakeSendsMagicPacket(t *testing.T) {
	ln, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := wakeTo("aa-bb-cc-0d-ee-ff", ln.LocalAddr().String()); err != nil {
		t.Fatal(err)
	}
	ln.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 200)
	n, _, err := ln.ReadFrom(buf)
	if err != nil || !bytes.Equal(buf[:n], wantPacket) {
		t.Fatalf("got %x, %v", buf[:n], err)
	}
	if Wake("not a mac") == nil {
		t.Error("Wake accepted a bad MAC")
	}
}

func TestWakeViaUsesWakeonlan(t *testing.T) {
	skipOnWindows(t)
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	if err := os.WriteFile(filepath.Join(dir, "wakeonlan"), []byte("#!/bin/sh\necho \"$@\" > '"+args+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir) // the test server adds only its fake sudo
	srv := sftptest.New(t, "pw")
	if err := WakeVia(context.Background(), srv.Dial(t), "AA-BB-CC-D-EE-FF"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(args); string(b) != "aa:bb:cc:0d:ee:ff\n" {
		t.Errorf("wakeonlan got %q", b)
	}
}

func TestWakeViaWithoutToolsSaysWhatItTried(t *testing.T) {
	skipOnWindows(t)
	t.Setenv("PATH", t.TempDir())
	srv := sftptest.New(t, "pw")
	err := WakeVia(context.Background(), srv.Dial(t), "aa:bb:cc:dd:ee:ff")
	if err == nil {
		t.Fatal("wake with no tool succeeded")
	}
	for _, tool := range []string{"wakeonlan", "etherwake", "perl"} {
		if !strings.Contains(err.Error(), tool) {
			t.Errorf("error %q does not name %s", err, tool)
		}
	}
}

func TestWakeViaRejectsBadMACBeforeRunning(t *testing.T) {
	skipOnWindows(t)
	srv := sftptest.New(t, "pw")
	if err := WakeVia(context.Background(), srv.Dial(t), "aa:bb:cc:dd:ee:ff;reboot"); err == nil {
		t.Fatal("bad MAC accepted")
	}
	if got := srv.Commands(); len(got) != 0 {
		t.Fatalf("commands ran: %q", got)
	}
}

func skipOnWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test server runs commands with /bin/sh")
	}
}
