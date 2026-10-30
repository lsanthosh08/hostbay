package sshx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/proxy"

	"hostbay/internal/apperr"
)

// Proxy is a SOCKS5 or HTTP CONNECT proxy that a connection's first hop and
// every non-SSH dial go through (R49). Both send their credentials and the
// target's name in the clear to the proxy.
//
// A "vpn" proxy is an app-scoped VPN profile (KTD2): VPN is its ID, Host its
// name for messages, and Socks looks up its helper's loopback SOCKS5 address
// and login at each dial, so a connection that outlives a reconnect never
// dials a stale port. An external profile's "vpn" proxy has Iface instead
// (KTD7): it looks up the vendor tunnel's interface at each dial, and the
// DNS servers names resolve through, and the socket is pinned to that
// interface, so nothing leaves by another route.
type Proxy struct {
	Type     string // "socks5", "http" or "vpn"
	Host     string
	Port     int
	User     string
	Password string

	VPN   string
	Socks func() (addr, user, pass string, err error)
	Iface func(ctx context.Context) (iface string, dns []string, err error)
}

// maxProxyReply caps an HTTP proxy's reply headers.
const maxProxyReply = 16 << 10

// DialTCP connects to addr (host:port) through p, or directly when p is
// nil. The proxy is handed the host name to resolve. A proxy that cannot be
// reached or refuses is a ProxyFailed error with step "proxy", and a VPN's
// failure is VPNFailed, or the VPNDown its Socks returned; nothing is ever
// dialled directly instead (KTD22).
func DialTCP(ctx context.Context, p *Proxy, addr string) (net.Conn, error) {
	if p == nil {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	if p.Type == "vpn" && p.Iface != nil {
		return p.dialBound(ctx, "tcp", addr)
	}
	if p.Type == "vpn" {
		paddr, user, pass, err := p.Socks()
		if err != nil {
			return nil, err
		}
		c, err := dialSOCKS5(ctx, paddr, user, pass, addr)
		if err != nil {
			return nil, p.vpnErr(addr, err)
		}
		return c, nil
	}
	c, err := p.dial(ctx, addr)
	if err != nil {
		return nil, apperr.New(apperr.ProxyFailed, fmt.Sprintf("The proxy %s port %d failed: %v", p.Host, p.Port, err)).
			With("step", "proxy").
			With("host", p.Host).
			With("port", strconv.Itoa(p.Port))
	}
	return c, nil
}

func (p *Proxy) dial(ctx context.Context, addr string) (net.Conn, error) {
	paddr := net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
	switch p.Type {
	case "socks5":
		return dialSOCKS5(ctx, paddr, p.User, p.Password, addr)
	case "http":
		return p.connect(ctx, paddr, addr)
	}
	return nil, fmt.Errorf("unknown proxy type %q", p.Type)
}

func dialSOCKS5(ctx context.Context, paddr, user, pass, addr string) (net.Conn, error) {
	var auth *proxy.Auth
	if user != "" || pass != "" {
		auth = &proxy.Auth{User: user, Password: pass}
	}
	d, err := proxy.SOCKS5("tcp", paddr, auth, &net.Dialer{})
	if err != nil {
		return nil, err
	}
	return d.(proxy.ContextDialer).DialContext(ctx, "tcp", addr)
}

// dnsPort is where an external profile's DNS servers listen.
var dnsPort = "53"

// dialBound dials addr with the socket pinned to the external profile's
// tunnel interface (KTD7). A name resolves through the profile's DNS
// servers, over the same interface, when it names any: never the system's
// resolver, which would answer from outside the VPN.
func (p *Proxy) dialBound(ctx context.Context, network, addr string) (net.Conn, error) {
	iface, dns, err := p.Iface(ctx)
	if err != nil {
		return nil, err
	}
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, p.vpnErr(addr, fmt.Errorf("its interface %s is gone", iface))
	}
	d := &net.Dialer{Control: bindTo(ifi)}
	if len(dns) > 0 {
		d.Resolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var err error
			for _, s := range dns {
				var c net.Conn
				if c, err = (&net.Dialer{Control: bindTo(ifi)}).DialContext(ctx, network, net.JoinHostPort(s, dnsPort)); err == nil {
					return c, nil
				}
			}
			return nil, err
		}}
	}
	c, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, p.vpnErr(addr, err)
	}
	return c, nil
}

// vpnErr is a dial through the VPN that failed at or beyond its helper.
// Only the innermost cause is told: the outer ones name the helper's
// loopback address, which stays between Hostbay and the helper (KTD2).
func (p *Proxy) vpnErr(addr string, err error) error {
	var oe *net.OpError
	for errors.As(err, &oe) {
		err = oe.Err
	}
	return apperr.New(apperr.VPNFailed, fmt.Sprintf("VPN %s could not reach %s: %v", p.Host, addr, err)).
		With("step", "vpn").
		With("vpnId", p.VPN)
}

// connect opens an HTTP CONNECT tunnel to addr.
func (p *Proxy) connect(ctx context.Context, paddr, addr string) (net.Conn, error) {
	if strings.ContainsAny(addr, "\r\n") {
		return nil, errors.New("the target address holds a line break")
	}
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", paddr)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	tc, err := handshakeConnect(c, p, addr)
	if !stop() {
		c.Close()
		return nil, ctx.Err()
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return tc, nil
}

func handshakeConnect(c net.Conn, p *Proxy, addr string) (net.Conn, error) {
	req := "CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n"
	if p.User != "" || p.Password != "" {
		req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(p.User+":"+p.Password)) + "\r\n"
	}
	if _, err := io.WriteString(c, req+"\r\n"); err != nil {
		return nil, err
	}
	r := bufio.NewReader(c)
	status, read := "", 0
	for {
		line, err := r.ReadSlice('\n')
		read += len(line)
		if errors.Is(err, bufio.ErrBufferFull) || read > maxProxyReply {
			return nil, errors.New("the HTTP proxy sent too long a reply")
		}
		if err != nil {
			return nil, err
		}
		l := strings.TrimRight(string(line), "\r\n")
		if status == "" {
			status = l
			continue
		}
		if l == "" {
			break
		}
	}
	f := strings.SplitN(status, " ", 3)
	if len(f) < 2 || !strings.HasPrefix(f[0], "HTTP/") {
		return nil, fmt.Errorf("the HTTP proxy sent %q", status)
	}
	if code, _ := strconv.Atoi(f[1]); code < 200 || code > 299 {
		return nil, fmt.Errorf("the HTTP proxy answered %s", strings.Join(f[1:], " "))
	}
	return &bufConn{Conn: c, r: r}, nil
}

// bufConn reads what the reply's reader already buffered first: a server
// may send its first bytes in the same segment as the proxy's reply.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// DialUDP opens a datagram connection to addr through a VPN's helper, over
// SOCKS5 UDP ASSOCIATE, for mosh (KTD2). Only a "vpn" proxy carries UDP.
func (p *Proxy) DialUDP(ctx context.Context, addr *net.UDPAddr) (net.Conn, error) {
	if p.Type != "vpn" {
		return nil, errors.New("only a VPN profile carries UDP")
	}
	if p.Iface != nil {
		return p.dialBound(ctx, "udp", addr.String())
	}
	paddr, user, pass, err := p.Socks()
	if err != nil {
		return nil, err
	}
	c, err := associate(ctx, paddr, user, pass, addr)
	if err != nil {
		return nil, p.vpnErr(addr.String(), err)
	}
	return c, nil
}

// associate asks the SOCKS5 server at paddr to relay datagrams between a
// new loopback socket and to.
func associate(ctx context.Context, paddr, user, pass string, to *net.UDPAddr) (*socksUDP, error) {
	uc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	ctrl, err := (&net.Dialer{}).DialContext(ctx, "tcp", paddr)
	if err != nil {
		uc.Close()
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { ctrl.Close() })
	relay, err := handshakeAssociate(ctrl, user, pass, uc.LocalAddr().(*net.UDPAddr))
	if !stop() {
		err = ctx.Err()
	}
	if err != nil {
		ctrl.Close()
		uc.Close()
		return nil, err
	}
	return &socksUDP{UDPConn: uc, ctrl: ctrl, relay: relay, head: udpHead(to)}, nil
}

// handshakeAssociate logs in (RFC 1929) and asks for UDP ASSOCIATE from the
// local socket from, returning the relay's address.
func handshakeAssociate(c net.Conn, user, pass string, from *net.UDPAddr) (*net.UDPAddr, error) {
	req := append([]byte{5, 1, 2, 1, byte(len(user))}, user...)
	req = append(append(req, byte(len(pass))), pass...)
	req = append(append(req, 5, 3, 0), udpHead(from)[3:]...)
	if _, err := c.Write(req); err != nil {
		return nil, err
	}
	var r [8]byte // method choice, login status, then the reply's VER REP RSV ATYP
	if _, err := io.ReadFull(c, r[:]); err != nil {
		return nil, err
	}
	switch {
	case r[1] != 2 || r[3] != 0:
		return nil, errors.New("the VPN helper refused the login")
	case r[5] != 0:
		return nil, fmt.Errorf("the VPN helper refused UDP (reply %d)", r[5])
	case r[7] != 1 && r[7] != 4:
		return nil, fmt.Errorf("the VPN helper sent address type %d", r[7])
	}
	n := 4
	if r[7] == 4 {
		n = 16
	}
	a := make([]byte, n+2)
	if _, err := io.ReadFull(c, a); err != nil {
		return nil, err
	}
	relay := &net.UDPAddr{IP: net.IP(a[:n]), Port: int(binary.BigEndian.Uint16(a[n:]))}
	if relay.IP.IsUnspecified() {
		relay.IP = c.RemoteAddr().(*net.TCPAddr).IP
	}
	return relay, nil
}

// udpHead is a SOCKS5 UDP datagram's header for to: RSV, FRAG, ATYP,
// address and port.
func udpHead(to *net.UDPAddr) []byte {
	h, ip := []byte{0, 0, 0, 1}, to.IP.To4()
	if ip == nil {
		h[3], ip = 4, to.IP.To16()
	}
	return binary.BigEndian.AppendUint16(append(h, ip...), uint16(to.Port))
}

// socksUDP talks to one destination through a SOCKS5 relay. The
// association lasts while ctrl, its TCP connection, stays open.
type socksUDP struct {
	*net.UDPConn
	ctrl  net.Conn
	relay *net.UDPAddr
	head  []byte
}

func (u *socksUDP) Write(b []byte) (int, error) {
	if _, err := u.WriteToUDP(append(slices.Clip(u.head), b...), u.relay); err != nil {
		return 0, err
	}
	return len(b), nil
}

// Read returns the next datagram from the destination. The helper sends
// replies with the header as the client wrote it; anything else is dropped.
func (u *socksUDP) Read(b []byte) (int, error) {
	buf := make([]byte, len(u.head)+len(b))
	for {
		n, from, err := u.ReadFromUDP(buf)
		if err != nil {
			return 0, err
		}
		if from.IP.Equal(u.relay.IP) && from.Port == u.relay.Port && n >= len(u.head) && bytes.Equal(buf[:len(u.head)], u.head) {
			return copy(b, buf[len(u.head):n]), nil
		}
	}
}

func (u *socksUDP) Close() error {
	u.ctrl.Close()
	return u.UDPConn.Close()
}
