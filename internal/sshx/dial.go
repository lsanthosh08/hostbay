// Package sshx dials SSH connections through jump chains with known_hosts
// verification at every hop, and shares one connection per host between
// consumers (KTD4, KTD5).
package sshx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"hostbay/internal/apperr"
)

// DefaultTimeout caps each hop's TCP connect and SSH handshake, as
// session.rs did, so an unreachable host fails fast.
const DefaultTimeout = 20 * time.Second

// Dialer opens connections, checking host keys against KnownHostsPath.
type Dialer struct {
	KnownHostsPath string
	Timeout        time.Duration // 0 means DefaultTimeout
}

// dialHop connects to hop i, directly or through prev, and authenticates.
func (d *Dialer) dialHop(ctx context.Context, prev *ssh.Client, i int, h Hop, auth []ssh.AuthMethod) (*ssh.Client, error) {
	timeout := d.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	addr := net.JoinHostPort(h.Host, strconv.Itoa(h.port()))
	var nc net.Conn
	var err error
	if prev == nil {
		nc, err = DialTCP(ctx, h.Proxy, addr)
	} else {
		nc, err = prev.DialContext(ctx, "tcp", addr)
	}
	if apperr.CodeOf(err) == apperr.ProxyFailed {
		return nil, err
	}
	if err != nil {
		return nil, unreachable(i, h, err)
	}

	check, algos, err := hostKeyCallback(d.KnownHostsPath, i, h.knownHostsName(), h.port())
	if err != nil {
		nc.Close()
		return nil, err
	}
	// Channel conns through a jump host ignore deadlines, so closing the
	// conn is what bounds the handshake.
	stop := context.AfterFunc(ctx, func() { nc.Close() })
	cfg := &ssh.ClientConfig{
		User:              h.User,
		Auth:              auth,
		HostKeyCallback:   check,
		HostKeyAlgorithms: algos,
	}
	h.Algorithms.apply(cfg)
	sc, chans, reqs, err := ssh.NewClientConn(nc, addr, cfg)
	if !stop() {
		if err == nil {
			sc.Close()
		}
		return nil, unreachable(i, h, ctx.Err())
	}
	if err != nil {
		nc.Close()
		var hk *HostKeyError
		if errors.As(err, &hk) {
			return nil, hk
		}
		if strings.Contains(err.Error(), "no common algorithm") {
			return nil, fmt.Errorf("%s (hop %d): the server offers only algorithms this host does not allow; "+
				"if it is a legacy server, allow its legacy algorithms in the host's settings: %w", addr, i, err)
		}
		return nil, fmt.Errorf("%s (hop %d): %w", addr, i, err)
	}
	return ssh.NewClient(sc, chans, reqs), nil
}

func unreachable(i int, h Hop, err error) error {
	return apperr.New(apperr.Unreachable, fmt.Sprintf("Cannot reach %s port %d: %v", h.Host, h.port(), err)).
		With("hop", strconv.Itoa(i)).
		With("host", h.Host).
		With("port", strconv.Itoa(h.port()))
}
