package terminal

import (
	"io"
	"net"
	"testing"
)

func tcpServer(t *testing.T, serve func(net.Conn)) net.Conn {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err == nil {
			serve(c)
			c.Close()
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func readN(t *testing.T, r io.Reader, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A raw TCP tab to an echo server shows what was typed, as the server
// sent it back (R48).
func TestRawTCPEchoes(t *testing.T) {
	tr := OpenTCP(tcpServer(t, func(c net.Conn) { io.Copy(c, c) }), false)
	defer tr.Close()
	if _, err := tr.Write([]byte("hi\xff\r\n")); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, tr, 5); got != "hi\xff\r\n" {
		t.Fatalf("read %q; bytes must pass unchanged", got)
	}
}

// With local echo, typed text shows even when the server sends nothing,
// and the end of the connection is reported.
func TestRawTCPLocalEchoAndEnd(t *testing.T) {
	got, echoed := make(chan string, 1), make(chan struct{})
	tr := OpenTCP(tcpServer(t, func(c net.Conn) {
		b := make([]byte, 3)
		io.ReadFull(c, b)
		got <- string(b)
		<-echoed
	}), true)
	defer tr.Close()
	go tr.Write([]byte("abc"))
	if s := readN(t, tr, 3); s != "abc" {
		t.Fatalf("local echo = %q", s)
	}
	close(echoed)
	if s := <-got; s != "abc" {
		t.Fatalf("server got %q", s)
	}
	if _, err := io.ReadAll(tr); err != nil {
		t.Fatal(err)
	}
	if r := tr.(Ender).EndReason(); r != "connection closed by the host" {
		t.Fatalf("end reason = %q", r)
	}
}
