package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/database"
	"hostbay/internal/sshx"
	"hostbay/internal/store"
)

// tcpDialTimeout bounds a Telnet or raw TCP connect, as the Telnet client did.
const tcpDialTimeout = 15 * time.Second

// dialHost opens a TCP connection to h's port the way every non-SSH
// session to h must go: through its jump host's shared SSH connection when
// it has one, else through its VPN profile or proxy (KTD5, KTD22). A VPN
// profile that is down is connected first, outside the dial's timeout.
// Closing the connection releases the jump connection.
func (s *ConnService) dialHost(ctx context.Context, h *store.Host, port int) (net.Conn, error) {
	addr := net.JoinHostPort(h.Hostname, strconv.Itoa(port))
	if !h.Jumped() {
		d, err := s.creds.st.Data()
		if err != nil {
			return nil, err
		}
		p, err := s.creds.dialUp(ctx, d, h)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, tcpDialTimeout)
		defer cancel()
		return sshx.DialTCP(ctx, p, addr)
	}
	t, err := s.jumpTarget(ctx, h)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, tcpDialTimeout)
	defer cancel()
	l, err := s.acquireLease(ctx, t)
	if err != nil {
		return nil, err
	}
	cl, err := l.Live(ctx)
	var c net.Conn
	if err == nil {
		c, err = cl.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		l.Release()
		return nil, err
	}
	return &leasedConn{Conn: c, release: l.Release}, nil
}

// jumpTarget is the SSH chain to a jumped host h's jump host, with the
// first hop's VPN profile up. A jumped host that also names a proxy is
// refused: the jump host's chain carries no direct-dial fallback.
func (s *ConnService) jumpTarget(ctx context.Context, h *store.Host) (sshx.Target, error) {
	if h.Proxied() {
		return sshx.Target{}, jumpAndProxyErr(h.Alias)
	}
	t, err := s.targetVia(*h.JumpHostID, store.Deref(h.VPNID))
	if err != nil {
		return sshx.Target{}, err
	}
	if err := s.creds.vpnUp(ctx, t.Hops[0].Proxy); err != nil {
		return sshx.Target{}, err
	}
	return t, nil
}

// databaseDial is h's one dial function for a database driver (KTD24), on
// dialHost: through the jump host's lease, else the proxy, and directly
// only when the host has neither. Every connection the driver opens passes
// through it, pool redials and cancel requests included. It refuses every
// address except the host's own host and port, naming the refused one, so
// a server-sent redirect fails visibly instead of leaving the chain.
func (s *ConnService) databaseDial(h store.Host, port int) database.Dial {
	return allowlisted(h, port, func(ctx context.Context) (net.Conn, error) { return s.dialHost(ctx, &h, port) })
}

// allowlisted wraps dial so it opens only h's host and port. The host
// matches in any case: host names are case-insensitive, and the MongoDB
// driver lowercases the one it dials.
func allowlisted(h store.Host, port int, dial func(context.Context) (net.Conn, error)) database.Dial {
	want := net.JoinHostPort(h.Hostname, strconv.Itoa(port))
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, p, err := net.SplitHostPort(address)
		if !strings.HasPrefix(network, "tcp") || err != nil || p != strconv.Itoa(port) || !strings.EqualFold(host, h.Hostname) {
			return nil, fmt.Errorf("Hostbay refused a connection to %s: %s may only connect to %s, through its jump host or proxy", address, h.Alias, want)
		}
		return dial(ctx)
	}
}

// leasedConn releases its jump connection's lease when closed.
type leasedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *leasedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// testHost is the editor's unsaved form as a host to test: the saved host
// with the form's fields, keeping the saved password only while the form
// still matches what it was sealed to (KTD6).
func testHost(d *store.Data, host Host) (store.Host, error) {
	h := store.Host{Meta: store.Meta{ID: host.ID}}
	var saved *store.Host
	if host.ID != "" {
		saved = d.Host(host.ID)
	}
	if saved != nil {
		h = *saved
	}
	if err := checkExtras(d, host, h.ProxyID, h.VPNID); err != nil {
		return store.Host{}, err
	}
	fromForm(d, &h, host)
	h.Password = ""
	if saved != nil && saved.Password != "" && bytes.Equal(saved.SecretAAD(), h.SecretAAD()) {
		h.Password = saved.Password
	}
	return h, nil
}

// TestConnection checks an SSH, Telnet or raw TCP host from the editor's
// unsaved form, through its saved jump hosts and proxy, without saving
// anything or sharing the connection (R53). It reports the step that failed
// or "ok". An unknown host key comes back as the usual trust prompt. The
// form's typed password is used; otherwise the saved one, but only while
// the form still points where it was sealed (KTD6). After the address,
// port, user or kind changed, the saved password is never decrypted and a
// password host reports SecretNeeded.
func (s *ConnService) TestConnection(ctx context.Context, host Host) (ConnTest, error) {
	d, err := s.creds.st.Data()
	if err != nil {
		return ConnTest{}, err
	}
	h, err := testHost(d, host)
	if err != nil {
		return ConnTest{}, err
	}

	switch h.KindName() {
	case "ssh", "mosh":
		typed := ""
		if host.Password != nil {
			typed = *host.Password
		}
		c, err := s.creds.unseal(d, h, typed)
		if err != nil {
			return testResult(err), nil
		}
		t, err := s.chain(d, c, "")
		if err == nil {
			err = s.creds.vpnUp(ctx, t.Hops[0].Proxy)
		}
		if err != nil {
			return testResult(err), nil
		}
		cl, err := s.mgr.Dialer().Dial(ctx, t)
		if err != nil {
			return testResult(s.hostKeyPrompt(err)), nil
		}
		cl.Close()
	case "telnet", "tcp":
		port := h.Port
		if port == 0 && h.KindName() == "telnet" {
			port = 23
		}
		conn, err := s.testTCP(ctx, d, &h, port)
		if err != nil {
			return testResult(err), nil
		}
		conn.Close()
	default:
		return ConnTest{}, errors.New("test connection checks SSH, Telnet and raw TCP hosts")
	}
	return ConnTest{Step: "ok"}, nil
}

// testTCP is dialHost for TestConnection: the jump chain is dialled on its
// own rather than shared.
func (s *ConnService) testTCP(ctx context.Context, d *store.Data, h *store.Host, port int) (net.Conn, error) {
	addr := net.JoinHostPort(h.Hostname, strconv.Itoa(port))
	if !h.Jumped() {
		p, err := s.creds.dialUp(ctx, d, h)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, tcpDialTimeout)
		defer cancel()
		c, err := sshx.DialTCP(ctx, p, addr)
		if err != nil && apperr.CodeOf(err) == "" {
			err = apperr.New(apperr.Unreachable, "Cannot reach "+h.Hostname+" port "+strconv.Itoa(port)+": "+err.Error())
		}
		return c, err
	}
	t, err := s.jumpTarget(ctx, h)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, tcpDialTimeout)
	defer cancel()
	cl, err := s.mgr.Dialer().Dial(ctx, t)
	if err != nil {
		return nil, s.hostKeyPrompt(err)
	}
	c, err := cl.DialContext(ctx, "tcp", addr)
	if err != nil {
		cl.Close()
		return nil, apperr.New(apperr.Unreachable, "The jump host cannot reach "+addr+": "+err.Error())
	}
	return &leasedConn{Conn: c, release: func() { cl.Close() }}, nil
}

// testResult names the step err failed at.
func testResult(err error) ConnTest {
	var ae *apperr.Error
	errors.As(err, &ae)
	step := "ssh"
	switch {
	case ae != nil && ae.Code == apperr.ProxyFailed:
		step = "proxy"
	case ae != nil && (ae.Code == apperr.VPNFailed || ae.Code == apperr.VPNDown || ae.Code == apperr.VPNAmbiguous):
		step = "vpn"
	case ae != nil && ae.Code == apperr.Unreachable:
		step = "tcp"
	case ae != nil && (ae.Code == apperr.HostKeyUnknown || ae.Code == apperr.HostKeyChanged):
		step = "hostKey"
	case ae != nil && ae.Code == apperr.SecretNeeded, strings.Contains(err.Error(), "unable to authenticate"):
		step = "auth"
	}
	return ConnTest{Step: step, Message: err.Error(), Error: ae}
}
