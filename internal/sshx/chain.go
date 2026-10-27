package sshx

import (
	"context"
	"errors"
	"time"

	"golang.org/x/crypto/ssh"
)

// Hop is one SSH server in a chain.
type Hop struct {
	Host string
	Port int // 0 means 22
	User string
	// HostKeyAlias, when set, is the known_hosts name checked and written
	// instead of Host, for internal addresses that repeat behind different
	// bastions (KTD4).
	HostKeyAlias string
	Auth         Auth
	// Proxy, on the first hop only, is what that hop is dialled through.
	Proxy *Proxy
	// Algorithms are legacy algorithms this hop also offers (R52).
	Algorithms Algorithms
	// ForwardAgent, on the last hop only, lets the server use this
	// computer's SSH agent through sessions that ask for it (R50).
	ForwardAgent bool
	// X11, on the last hop only, lets sessions that ask for it open X11
	// programs on this computer's X server (R47, KTD20).
	X11 bool
}

// Target describes a connection: jump hosts in order, then the destination
// last. The caller builds it from a stored or ad-hoc host; this package never
// reads the host store.
type Target struct {
	// Key identifies the connection the Manager shares: host ID plus config
	// revision for a stored host, a synthetic value for an ad-hoc connect.
	Key  string
	Hops []Hop
	// Keepalive is how often the Manager probes the connection; 0 means
	// every 30 seconds (R52).
	Keepalive time.Duration
}

func (h Hop) port() int {
	if h.Port == 0 {
		return 22
	}
	return h.Port
}

func (h Hop) knownHostsName() string {
	if h.HostKeyAlias != "" {
		return h.HostKeyAlias
	}
	return h.Host
}

// Client is a connection to a Target's last hop. Close also closes the jump
// hops it was tunnelled through.
type Client struct {
	*ssh.Client
	jumps []*ssh.Client // nearest first
	// ForwardAgent is set when the last hop may use the local agent: a
	// session that wants it calls agent.RequestAgentForwarding (R50).
	ForwardAgent bool
	x11          *x11Fwd // nil unless the last hop forwards X11
}

func (c *Client) Close() error {
	err := c.Client.Close()
	for i := len(c.jumps) - 1; i >= 0; i-- {
		c.jumps[i].Close()
	}
	return err
}

// Dial connects to t's last hop, dialling each hop through the previous one.
// Every hop's credentials are checked for a missing secret before anything
// is sent, and every hop's host key is verified before the next hop is
// dialled over it (KTD4).
func (d *Dialer) Dial(ctx context.Context, t Target) (*Client, error) {
	if len(t.Hops) == 0 {
		return nil, errors.New("sshx: target has no hops")
	}
	auths := make([][]ssh.AuthMethod, len(t.Hops))
	for i, h := range t.Hops {
		m, done, err := authMethods(i, h)
		if err != nil {
			return nil, err
		}
		defer done()
		auths[i] = m
	}

	var open []*ssh.Client
	for i, h := range t.Hops {
		var prev *ssh.Client
		if i > 0 {
			prev = open[i-1]
		}
		c, err := d.dialHop(ctx, prev, i, h, auths[i])
		if err != nil {
			for j := len(open) - 1; j >= 0; j-- {
				open[j].Close()
			}
			return nil, err
		}
		open = append(open, c)
	}
	last := t.Hops[len(t.Hops)-1]
	if last.ForwardAgent {
		forwardAgent(open[len(open)-1])
	}
	c := &Client{Client: open[len(open)-1], jumps: open[:len(open)-1], ForwardAgent: last.ForwardAgent}
	// Without X11 no handler is registered, so x/crypto/ssh refuses the
	// server's x11 channels.
	if last.X11 {
		c.x11 = forwardX11(c.Client)
	}
	return c, nil
}
