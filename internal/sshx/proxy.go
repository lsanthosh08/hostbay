package sshx

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"golang.org/x/net/proxy"

	"hostbay/internal/apperr"
)

// Proxy is a SOCKS5 or HTTP CONNECT proxy that a connection's first hop and
// every non-SSH dial go through (R49). Both send their credentials and the
// target's name in the clear to the proxy.
type Proxy struct {
	Type     string // "socks5" or "http"
	Host     string
	Port     int
	User     string
	Password string
}

// maxProxyReply caps an HTTP proxy's reply headers.
const maxProxyReply = 16 << 10

// DialTCP connects to addr (host:port) through p, or directly when p is
// nil. The proxy is handed the host name to resolve. A proxy that cannot be
// reached or refuses is a ProxyFailed error with step "proxy"; nothing is
// ever dialled directly instead (KTD22).
func DialTCP(ctx context.Context, p *Proxy, addr string) (net.Conn, error) {
	if p == nil {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
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
		var auth *proxy.Auth
		if p.User != "" || p.Password != "" {
			auth = &proxy.Auth{User: p.User, Password: p.Password}
		}
		d, err := proxy.SOCKS5("tcp", paddr, auth, &net.Dialer{})
		if err != nil {
			return nil, err
		}
		return d.(proxy.ContextDialer).DialContext(ctx, "tcp", addr)
	case "http":
		return p.connect(ctx, paddr, addr)
	}
	return nil, fmt.Errorf("unknown proxy type %q", p.Type)
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
