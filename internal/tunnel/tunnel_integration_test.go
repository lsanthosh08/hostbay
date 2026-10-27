package tunnel

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"hostbay/internal/sftp/sftptest"
	"hostbay/internal/sshx"
)

// These run the tunnels end to end over a two-hop chain of in-process SSH
// servers (jump, then target) through the shared connection manager, the way
// the app uses them.

type chain struct {
	jump, target *sftptest.Server
	lease        *sshx.Lease
}

func newChain(t *testing.T) *chain {
	t.Helper()
	jump, target := sftptest.New(t, "jump-pw"), sftptest.New(t, "target-pw")
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(kh, []byte(jump.KnownHostsLine()+"\n"+target.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jh, jp := splitAddr(t, jump.Addr)
	th, tp := splitAddr(t, target.Addr)
	m := sshx.NewManager(&sshx.Dialer{KnownHostsPath: kh})
	l, err := m.Acquire(context.Background(), sshx.Target{Key: "target@1", Hops: []sshx.Hop{
		{Host: jh, Port: jp, User: "u", Auth: sshx.Auth{Password: "jump-pw"}},
		{Host: th, Port: tp, User: "u", Auth: sshx.Auth{Password: "target-pw"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Release)
	return &chain{jump: jump, target: target, lease: l}
}

func (c *chain) open(t *testing.T, s Spec) *Tunnel {
	t.Helper()
	tn, err := Open(c.lease.Client().Client, s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tn.Close)
	return tn
}

func TestLocalForwardThroughJumpChainReachesTargetService(t *testing.T) {
	c := newChain(t)
	echo := echoServer(t)
	host, port := splitAddr(t, echo)
	tn := c.open(t, Spec{Kind: Local, TargetHost: host, TargetPort: port})
	if ip := tn.Addr().(*net.TCPAddr).IP; !ip.IsLoopback() {
		t.Fatalf("listens on %v, want loopback by default", ip)
	}

	conn, err := net.Dial("tcp", tn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	roundTrip(t, conn, "hello through the chain")
	if !slices.Contains(c.target.Dialed(), echo) {
		t.Fatalf("target dialled %v, want %s (the last hop must dial)", c.target.Dialed(), echo)
	}
}

// socksConnect asks the SOCKS proxy at addr for host:port by name and
// returns the connection and the reply code.
func socksConnect(t *testing.T, addr, host string, port uint16) (net.Conn, byte) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	req := append([]byte{5, 1, 0, 5, 1, 0, 3, byte(len(host))}, host...)
	conn.Write(binary.BigEndian.AppendUint16(req, port))
	reply := make([]byte, 12)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("SOCKS reply: %v", err)
	}
	if !bytes.Equal(reply[:2], []byte{5, 0}) {
		t.Fatalf("method reply %v", reply[:2])
	}
	conn.SetDeadline(time.Time{})
	return conn, reply[3]
}

func TestSocksResolvesNamesOnTheRemoteSide(t *testing.T) {
	c := newChain(t)
	c.target.Resolve("db.internal", echoServer(t)) // a name only the target knows
	tn := c.open(t, Spec{Kind: Dynamic})

	conn, code := socksConnect(t, tn.Addr().String(), "db.internal", 5432)
	if code != 0 {
		t.Fatalf("SOCKS reply code %d, want success", code)
	}
	roundTrip(t, conn, "select 1")
	if !slices.Contains(c.target.Dialed(), "db.internal:5432") {
		t.Fatalf("target was asked for %v, want the name unresolved", c.target.Dialed())
	}

	if _, code := socksConnect(t, tn.Addr().String(), "127.0.0.1", 1); code == 0 {
		t.Fatal("a refused destination got a success reply")
	}
}

func TestRemoteForwardCarriesHostConnectionsBack(t *testing.T) {
	c := newChain(t)
	host, port := splitAddr(t, echoServer(t))
	tn := c.open(t, Spec{Kind: Remote, TargetHost: host, TargetPort: port})
	if tn.Addr().(*net.TCPAddr).Port == 0 {
		t.Fatal("remote port 0 was not resolved to the port the host chose")
	}
	conn, err := net.Dial("tcp", tn.Addr().String()) // the host's listener
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	roundTrip(t, conn, "from the host")

	tn.Close()
	if c, err := net.DialTimeout("tcp", tn.Addr().String(), time.Second); err == nil {
		c.Close()
		t.Fatal("remote listener still open after Close")
	}
}

// Dropping the connection marks the tunnel dropped and frees its port.
func TestDropEndsTunnelWithReasonAndFreesPort(t *testing.T) {
	c := newChain(t)
	host, port := splitAddr(t, echoServer(t))
	tn := c.open(t, Spec{Kind: Local, TargetHost: host, TargetPort: port})
	conn, err := net.Dial("tcp", tn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	roundTrip(t, conn, "before")

	c.jump.DropAll() // the first hop goes away
	select {
	case <-tn.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("drop not noticed")
	}
	if tn.Err() == nil {
		t.Fatal("dropped tunnel reports no reason")
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("in-flight connection survived the drop")
	}
	ln, err := net.Listen("tcp", tn.Addr().String())
	if err != nil {
		t.Fatalf("port not freed after drop: %v", err)
	}
	ln.Close()
}
