package app

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
)

func TestTelnetHostOpensTerminalUnderItsOwner(t *testing.T) {
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
		if err != nil {
			return
		}
		conn.Write([]byte("Router login: "))
		// Hold the connection until the client closes it.
		conn.Read(make([]byte, 1))
		conn.Close()
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(port)
	telnet := "telnet"
	h, err := s.SaveHost(Host{Alias: "switch", Hostname: "127.0.0.1", Port: p, User: "admin", Kind: &telnet})
	if err != nil {
		t.Fatal(err)
	}

	id, err := c.PtyOpenTelnet(context.Background(), "tab-1", h.ID, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PtyAttach(id); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the login prompt", func() bool { return strings.Contains(ev.joined(PtyDataEvent(id)), "Router login: ") })

	c.Disconnect("tab-1")
	waitUntil(t, "the closed event", func() bool { return ev.joined(PtyClosedEvent(id)) != "" })
	if hosts, _ := s.ListHosts(); hosts[0].LastConnected == "" {
		t.Fatal("opening the terminal did not record the connection time")
	}
}

func TestMoshRefusesHostBehindJumpHost(t *testing.T) {
	c, s := newConnService(t)
	bastion, _ := s.SaveHost(Host{Alias: "bastion", Hostname: "10.0.0.1", Port: 22, User: "u"})
	mosh := "mosh"
	web, err := s.SaveHost(Host{Alias: "web", Hostname: "10.0.0.2", Port: 22, User: "u", Kind: &mosh, JumpHostID: &bastion.ID})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.PtyOpenMosh(context.Background(), "any-session", web.ID, 80, 24)
	if err == nil || !strings.Contains(err.Error(), "jump host") {
		t.Fatalf("err = %v", err)
	}
}
