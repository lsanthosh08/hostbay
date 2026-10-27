//go:build !windows

package app

import (
	"strings"
	"testing"
)

func TestLocalShellOpensUnderItsOwnerAndClosesOnDisconnect(t *testing.T) {
	c, _ := newConnService(t)
	ev := &termEvents{}
	c.emit = ev.emit
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("HOME", t.TempDir())

	shells := c.ListLocalShells()
	if len(shells) != 1 {
		t.Fatalf("shells %+v", shells)
	}
	id, err := c.PtyOpenLocal("tab-1", shells[0].ID, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PtyAttach(id); err != nil {
		t.Fatal(err)
	}
	if err := c.PtyWrite(id, "echo hi-$((6*7))\n"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the command output", func() bool { return strings.Contains(ev.joined(PtyDataEvent(id)), "hi-42") })

	c.Disconnect("tab-1")
	waitUntil(t, "the closed event", func() bool { return ev.joined(PtyClosedEvent(id)) != "" })
}

func TestSerialRefusesUnlistedPort(t *testing.T) {
	c, _ := newConnService(t)
	if _, err := c.PtyOpenSerial("tab-1", "/etc/passwd", 9600, 8, "none", "1", false); err == nil {
		t.Fatal("opened a path that is not a serial port")
	}
}
