package terminal

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

// telnetServer is one scripted Telnet peer: the test writes raw bytes to
// conn and checks what the client sent back with expect.
type telnetServer struct {
	t    *testing.T
	conn net.Conn
}

// dialTelnet opens a client against an in-process TCP server and returns
// both ends.
func dialTelnet(t *testing.T, cols, rows int) (Transport, *telnetServer) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	tr := OpenTelnet(c, cols, rows)
	srv := &telnetServer{t: t, conn: <-accepted}
	t.Cleanup(func() { tr.Close(); srv.conn.Close() })
	return tr, srv
}

func (s *telnetServer) send(b ...byte) {
	s.t.Helper()
	if _, err := s.conn.Write(b); err != nil {
		s.t.Fatal(err)
	}
}

// expect reads exactly len(want) bytes from the client and compares them.
func (s *telnetServer) expect(want ...byte) {
	s.t.Helper()
	s.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, len(want))
	if _, err := io.ReadFull(s.conn, got); err != nil {
		s.t.Fatalf("waiting for % x: %v (got % x)", want, err, got)
	}
	if !bytes.Equal(got, want) {
		s.t.Fatalf("client sent % x, want % x", got, want)
	}
}

// readAll keeps reading the client's output (which also drives negotiation)
// and returns it on the channel once the transport ends.
func readAll(tr Transport) <-chan string {
	out := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(tr)
		out <- string(b)
	}()
	return out
}

func TestTelnetSendsWindowSizeAfterResize(t *testing.T) {
	tr, srv := dialTelnet(t, 80, 24)
	readAll(tr)

	srv.send(iac, do, optNAWS)
	srv.expect(iac, will, optNAWS, iac, sb, optNAWS, 0, 80, 0, 24, iac, se)

	if err := tr.Resize(132, 43); err != nil {
		t.Fatal(err)
	}
	srv.expect(iac, sb, optNAWS, 0, 132, 0, 43, iac, se)

	// A dimension byte of 255 is doubled inside the subnegotiation.
	if err := tr.Resize(255, 50); err != nil {
		t.Fatal(err)
	}
	srv.expect(iac, sb, optNAWS, 0, 255, 255, 0, 50, iac, se)
}

func TestTelnetHoldsSizeUntilServerAsksForNAWS(t *testing.T) {
	tr, srv := dialTelnet(t, 80, 24)
	readAll(tr)

	// Resizing before DO NAWS sends nothing on the wire...
	if err := tr.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	// ...and the agreement then carries the latest size.
	srv.send(iac, do, optNAWS)
	srv.expect(iac, will, optNAWS, iac, sb, optNAWS, 0, 100, 0, 30, iac, se)
}

func TestTelnetRefusesUnknownOptionsAndContinues(t *testing.T) {
	tr, srv := dialTelnet(t, 80, 24)
	out := readAll(tr)

	const newEnviron, linemode = 39, 34
	srv.send(iac, do, newEnviron)
	srv.expect(iac, wont, newEnviron)
	srv.send(iac, will, linemode)
	srv.expect(iac, dont, linemode)
	// The peer's acknowledgement of the refusal needs no reply.
	srv.send(iac, wont, linemode)
	srv.send([]byte("still here")...)
	srv.conn.Close()

	if got := <-out; got != "still here" {
		t.Fatalf("output = %q", got)
	}
}

func TestTelnetAcceptsEchoSGAAndTerminalType(t *testing.T) {
	tr, srv := dialTelnet(t, 80, 24)
	readAll(tr)

	srv.send(iac, will, optEcho)
	srv.expect(iac, do, optEcho)
	srv.send(iac, will, optSGA)
	srv.expect(iac, do, optSGA)
	srv.send(iac, do, optSGA)
	srv.expect(iac, will, optSGA)
	// The client does not echo locally.
	srv.send(iac, do, optEcho)
	srv.expect(iac, wont, optEcho)

	srv.send(iac, do, optTTYPE)
	srv.expect(iac, will, optTTYPE)
	srv.send(iac, sb, optTTYPE, ttypeSend, iac, se)
	srv.expect(append(append([]byte{iac, sb, optTTYPE, ttypeIs}, "xterm-256color"...), iac, se)...)

	// A repeated request for an option already on gets no reply, so the
	// next thing on the wire is the answer to the request after it.
	srv.send(iac, will, optEcho)
	srv.send(iac, do, 99)
	srv.expect(iac, wont, 99)
}

func TestTelnetEscapesIACBothWays(t *testing.T) {
	tr, srv := dialTelnet(t, 80, 24)
	out := readAll(tr)

	if _, err := tr.Write([]byte{'x', 0xff, 'y'}); err != nil {
		t.Fatal(err)
	}
	srv.expect('x', 0xff, 0xff, 'y')

	srv.send('a', iac, iac, 'b', iac, nop, 'c')
	srv.conn.Close()
	if got := <-out; got != "a\xffbc" {
		t.Fatalf("output = %q", got)
	}
}

func TestTelnetSendsEnterAsCRLF(t *testing.T) {
	tr, srv := dialTelnet(t, 80, 24)
	readAll(tr)

	if _, err := tr.Write([]byte("ls\r")); err != nil {
		t.Fatal(err)
	}
	srv.expect('l', 's', '\r', '\n')
}

// net.Pipe hands each write to one read, so a command split across writes
// arrives split across reads.
func TestTelnetParsesCommandsSplitAcrossReads(t *testing.T) {
	client, server := net.Pipe()
	tr := newTelnet(client, 80, 24)
	defer tr.Close()
	out := readAll(tr)

	replies := make(chan []byte, 1)
	go func() {
		b := make([]byte, 3)
		io.ReadFull(server, b)
		replies <- b
	}()
	for _, chunk := range [][]byte{{'o', iac}, {will}, {optEcho, 'k', iac}, {iac}, {iac, sb, optTTYPE}, {ttypeSend, iac}, {se, '!'}} {
		server.Write(chunk)
	}
	if got := <-replies; !bytes.Equal(got, []byte{iac, do, optEcho}) {
		t.Fatalf("reply = % x", got)
	}
	server.Close()
	// TTYPE was never agreed, so its subnegotiation is ignored.
	if got := <-out; got != "ok\xff!" {
		t.Fatalf("output = %q", got)
	}
}

func TestTelnetEndReason(t *testing.T) {
	tr, srv := dialTelnet(t, 80, 24)
	out := readAll(tr)
	srv.conn.Close()
	<-out
	if got := tr.(Ender).EndReason(); got != "connection closed by the host" {
		t.Fatalf("EndReason = %q", got)
	}
}
