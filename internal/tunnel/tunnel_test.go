package tunnel

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"hostbay/internal/sftp/sftptest"
)

// socksRequest builds a SOCKS5 greeting offering methods, then a request
// with cmd, address type atyp and raw address bytes.
func socksRequest(methods []byte, cmd, atyp byte, addr []byte, port uint16) []byte {
	b := append([]byte{5, byte(len(methods))}, methods...)
	b = append(b, 5, cmd, 0, atyp)
	b = append(b, addr...)
	return binary.BigEndian.AppendUint16(b, port)
}

// handshake runs socksHandshake on one end of a pipe after the client
// sends req, and returns its result and everything it wrote back.
func handshake(t *testing.T, req []byte) (string, []byte, error) {
	t.Helper()
	srv, cli := net.Pipe()
	defer cli.Close()
	type res struct {
		addr string
		err  error
	}
	done := make(chan res, 1)
	go func() {
		a, err := socksHandshake(srv)
		srv.Close()
		done <- res{a, err}
	}()
	go cli.Write(req)
	reply, _ := io.ReadAll(cli)
	r := <-done
	return r.addr, reply, r.err
}

func TestSocksConnectAddressTypes(t *testing.T) {
	v6 := net.ParseIP("2001:db8::7")
	for _, c := range []struct {
		name string
		atyp byte
		addr []byte
		want string
	}{
		{"ipv4", 1, []byte{10, 0, 0, 5}, "10.0.0.5:8080"},
		{"ipv6", 4, v6, "[2001:db8::7]:8080"},
		{"domain", 3, append([]byte{byte(len("db.internal"))}, "db.internal"...), "db.internal:8080"},
	} {
		got, reply, err := handshake(t, socksRequest([]byte{0}, 1, c.atyp, c.addr, 8080))
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
		if !bytes.Equal(reply, []byte{5, 0}) {
			t.Errorf("%s: wrote %v before the dial, want only the method choice", c.name, reply)
		}
	}
}

func TestSocksRejectsWhatItDoesNotSupport(t *testing.T) {
	for _, c := range []struct {
		name  string
		req   []byte
		reply []byte
	}{
		{"auth required", socksRequest([]byte{2}, 1, 1, []byte{1, 2, 3, 4}, 80), []byte{5, 0xff}},
		{"bind", socksRequest([]byte{0}, 2, 1, []byte{1, 2, 3, 4}, 80), []byte{5, 0, 5, 7}},
		{"udp associate", socksRequest([]byte{0}, 3, 1, []byte{1, 2, 3, 4}, 80), []byte{5, 0, 5, 7}},
		{"bad address type", socksRequest([]byte{0}, 1, 9, nil, 80), []byte{5, 0, 5, 8}},
		{"socks4", []byte{4, 1, 0, 80, 1, 2, 3, 4, 0}, nil},
	} {
		_, reply, err := handshake(t, c.req)
		if err == nil {
			t.Errorf("%s: accepted", c.name)
		}
		if !bytes.HasPrefix(reply, c.reply) || (c.reply == nil && len(reply) != 0) {
			t.Errorf("%s: replied %v, want prefix %v", c.name, reply, c.reply)
		}
	}
}

func TestSpecRefusesNonLoopbackUnlessAllowed(t *testing.T) {
	ok := []Spec{
		{Kind: Local, BindPort: 8080, TargetHost: "db", TargetPort: 5432},
		{Kind: Local, BindHost: "localhost", BindPort: 8080, TargetHost: "db", TargetPort: 5432},
		{Kind: Dynamic, BindHost: "::1", BindPort: 1080},
		{Kind: Remote, BindPort: 9000, TargetHost: "127.0.0.1", TargetPort: 3000},
		{Kind: Local, BindHost: "0.0.0.0", BindPort: 8080, TargetHost: "db", TargetPort: 5432, AllowExternal: true},
	}
	for _, s := range ok {
		if err := s.Validate(); err != nil {
			t.Errorf("%+v: %v", s, err)
		}
	}
	bad := []Spec{
		{Kind: Local, BindHost: "0.0.0.0", BindPort: 8080, TargetHost: "db", TargetPort: 5432},
		{Kind: Dynamic, BindHost: "192.168.1.4", BindPort: 1080},
		{Kind: Remote, BindHost: "0.0.0.0", BindPort: 9000, TargetHost: "127.0.0.1", TargetPort: 3000},
		{Kind: Local, BindPort: 8080}, // no target
		{Kind: Local, BindPort: 70000, TargetHost: "db", TargetPort: 1},
		{Kind: "x", BindPort: 1},
	}
	for _, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("%+v accepted", s)
		}
	}
}

// Covers AE12: a local port already in use fails with the reason.
func TestLocalPortInUseFailsWithReason(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port

	s := sftptest.New(t, "pw")
	_, err = Open(s.Dial(t), Spec{Kind: Local, BindPort: port, TargetHost: "127.0.0.1", TargetPort: 1})
	if err == nil {
		t.Fatal("opened on a port in use")
	}
	if !strings.Contains(err.Error(), busy.Addr().String()) {
		t.Fatalf("error %q does not name the address", err)
	}
}

func TestOpenRefusesNonLoopbackBind(t *testing.T) {
	s := sftptest.New(t, "pw")
	_, err := Open(s.Dial(t), Spec{Kind: Dynamic, BindHost: "0.0.0.0", BindPort: 0})
	if !errors.Is(err, ErrExternalBind) {
		t.Fatalf("err = %v, want ErrExternalBind", err)
	}
}

// echoServer answers every connection by echoing it, and returns its address.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

// roundTrip writes msg on c and reads it back.
func roundTrip(t *testing.T, c net.Conn, msg string) {
	t.Helper()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, msg); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != msg {
		t.Fatalf("read %q, %v; want %q", buf, err, msg)
	}
}

func TestCloseCutsOpenConnectionsAndFreesThePort(t *testing.T) {
	s := sftptest.New(t, "pw")
	host, port := splitAddr(t, echoServer(t))
	tn, err := Open(s.Dial(t), Spec{Kind: Local, TargetHost: host, TargetPort: port})
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", tn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, c, "ping")

	tn.Close()
	select {
	case <-tn.Done():
	default:
		t.Fatal("Done not closed after Close")
	}
	if tn.Err() != nil {
		t.Fatalf("Err after Close = %v, want nil", tn.Err())
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("in-flight connection survived Close")
	}
	ln, err := net.Listen("tcp", tn.Addr().String())
	if err != nil {
		t.Fatalf("port not freed: %v", err)
	}
	ln.Close()
}

func splitAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(p)
	return h, port
}
