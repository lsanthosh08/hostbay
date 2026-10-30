package socks

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

const (
	testUser = "hb-user"
	testPass = "Socks-Secret-42!"
)

// recorder dials to whatever target a test sets and records every address the
// server asked for.
type recorder struct {
	mu     sync.Mutex
	target string
	asked  []string
}

func (r *recorder) dial(ctx context.Context, network, address string) (net.Conn, error) {
	r.mu.Lock()
	r.asked = append(r.asked, network+" "+address)
	target := r.target
	r.mu.Unlock()
	return (&net.Dialer{}).DialContext(ctx, network, target)
}

func (r *recorder) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.asked...)
}

func serve(t *testing.T, r *recorder) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go (&Server{User: testUser, Pass: testPass, Dial: r.dial}).Serve(ln)
	return ln.Addr().String()
}

// login opens a connection and runs the greeting and RFC 1929 login, returning
// the server's method choice and login status (0 when it got no further).
func login(t *testing.T, addr string, methods []byte, user, pass string) (net.Conn, byte, byte) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write(append([]byte{5, byte(len(methods))}, methods...))
	var choice [2]byte
	if _, err := io.ReadFull(c, choice[:]); err != nil {
		t.Fatalf("greeting reply: %v", err)
	}
	if choice[1] != 0x02 {
		return c, choice[1], 0
	}
	req := []byte{1, byte(len(user))}
	req = append(append(req, user...), byte(len(pass)))
	c.Write(append(req, pass...))
	var status [2]byte
	if _, err := io.ReadFull(c, status[:]); err != nil {
		t.Fatalf("login reply: %v", err)
	}
	return c, choice[1], status[1]
}

// request sends a request with a domain or IP address and returns the reply
// code and bound address.
func request(t *testing.T, c net.Conn, cmd byte, host string, port int) (byte, *net.UDPAddr) {
	t.Helper()
	req := append([]byte{5, cmd, 0}, addrBytes(host, port)...)
	c.Write(req)
	var rep [10]byte
	if _, err := io.ReadFull(c, rep[:]); err != nil {
		t.Fatalf("request reply: %v", err)
	}
	return rep[1], &net.UDPAddr{IP: net.IP(rep[4:8]), Port: int(binary.BigEndian.Uint16(rep[8:]))}
}

func addrBytes(host string, port int) []byte {
	var b []byte
	if ip := net.ParseIP(host).To4(); ip != nil {
		b = append([]byte{1}, ip...)
	} else {
		b = append([]byte{3, byte(len(host))}, host...)
	}
	return binary.BigEndian.AppendUint16(b, uint16(port))
}

func tcpEcho(t *testing.T) string {
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

func udpEcho(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(buf[:n], from)
		}
	}()
	return pc.LocalAddr().String()
}

func TestConnectWithLogin(t *testing.T) {
	r := &recorder{target: tcpEcho(t)}
	c, method, status := login(t, serve(t, r), []byte{0x00, 0x02}, testUser, testPass)
	if method != 0x02 || status != 0 {
		t.Fatalf("method %#x status %#x, want user/password accepted", method, status)
	}
	if code, _ := request(t, c, 1, "echo.corp.example", 7); code != 0 {
		t.Fatalf("CONNECT reply %#x", code)
	}
	// The name reaches the engine unresolved, so the tunnel's DNS answers it.
	if got := r.calls(); len(got) != 1 || got[0] != "tcp echo.corp.example:7" {
		t.Fatalf("dialled %q", got)
	}
	c.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo %q %v", buf, err)
	}
}

func TestBadLoginRefusedBeforeDial(t *testing.T) {
	r := &recorder{target: tcpEcho(t)}
	addr := serve(t, r)

	for _, c := range []struct {
		name       string
		user, pass string
	}{
		{"wrong password", testUser, "nope"},
		{"wrong user", "someone", testPass},
		{"empty login", "", ""},
	} {
		conn, method, status := login(t, addr, []byte{0x02}, c.user, c.pass)
		if method != 0x02 || status == 0 {
			t.Errorf("%s: method %#x status %#x, want refused", c.name, method, status)
		}
		// The server hangs up rather than reading a request.
		conn.Write(append([]byte{5, 1, 0}, addrBytes("10.0.0.2", 7)...))
		if n, _ := conn.Read(make([]byte, 1)); n != 0 {
			t.Errorf("%s: server answered a request after a failed login", c.name)
		}
	}

	_, method, _ := login(t, addr, []byte{0x00}, "", "")
	if method != 0xff {
		t.Errorf("no-auth client got method %#x, want 0xff", method)
	}
	if got := r.calls(); len(got) != 0 {
		t.Fatalf("dialled %q before a good login", got)
	}
}

func TestUDPAssociate(t *testing.T) {
	r := &recorder{target: udpEcho(t)}
	addr := serve(t, r)

	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	stranger, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer stranger.Close()

	c, _, status := login(t, addr, []byte{0x02}, testUser, testPass)
	if status != 0 {
		t.Fatalf("login status %#x", status)
	}
	code, relay := request(t, c, 3, "127.0.0.1", client.LocalAddr().(*net.UDPAddr).Port)
	if code != 0 {
		t.Fatalf("UDP ASSOCIATE reply %#x", code)
	}

	head := append([]byte{0, 0, 0}, addrBytes("mosh.corp.example", 60001)...)

	// Another local socket is not the client the association is bound to.
	stranger.WriteToUDP(append(head, "stray"...), relay)

	client.WriteToUDP(append(head, "hello"...), relay)
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 2048)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if want := append(head, "hello"...); !bytes.Equal(buf[:n], want) {
		t.Fatalf("reply %q, want %q", buf[:n], want)
	}
	if got := r.calls(); len(got) != 1 || got[0] != "udp mosh.corp.example:60001" {
		t.Fatalf("dialled %q, want only the client's destination", got)
	}

	// The association ends with its TCP connection.
	c.Close()
	time.Sleep(100 * time.Millisecond)
	client.WriteToUDP(append(head, "late"...), relay)
	client.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := client.Read(buf); err == nil {
		t.Fatal("relay still answered after the TCP connection closed")
	}
}

func TestUDPAssociateLocksToFirstSender(t *testing.T) {
	r := &recorder{target: udpEcho(t)}
	addr := serve(t, r)
	c, _, _ := login(t, addr, []byte{0x02}, testUser, testPass)
	_, relay := request(t, c, 3, "0.0.0.0", 0)

	first, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer first.Close()
	second, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer second.Close()
	head := append([]byte{0, 0, 0}, addrBytes("10.0.0.2", 7)...)

	buf := make([]byte, 2048)
	first.WriteToUDP(append(head, "one"...), relay)
	first.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := first.Read(buf); err != nil {
		t.Fatal(err)
	}
	second.WriteToUDP(append(head, "two"...), relay)
	second.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := second.Read(buf); err == nil {
		t.Fatal("a second sender used an association bound to the first")
	}
}
