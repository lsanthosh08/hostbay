package netd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
)

// ErrClosed is returned for requests on a connection that has ended.
var ErrClosed = errors.New("the connection to hostbay-netd has closed")

// Client is Hostbay's connection to the helper. The helper sweeps every
// route this connection made when it closes.
type Client struct {
	conn net.Conn
	enc  *json.Encoder

	// Build is what the helper said its executable is (Msg.Build).
	Build string

	mu      sync.Mutex
	nextID  uint64
	pending map[uint64]chan Msg
	tunnels map[string]func(Msg) // handle: its events' receiver

	events chan struct{}
	done   chan struct{}
}

// Dial connects to the installed helper. A *Error with CodeVersion means the
// helper is from another Hostbay version and wants reinstalling; CodeDenied
// means it is paired with another Hostbay copy or user and wants re-pairing.
func Dial(ctx context.Context) (*Client, error) {
	conn, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	c, err := newClient(conn)
	if !stop() {
		if c != nil {
			c.Close()
		}
		return nil, ctx.Err()
	}
	return c, err
}

// newClient reads the helper's hello, or its refusal, and starts reading
// replies and events.
func newClient(conn net.Conn) (*Client, error) {
	dec := json.NewDecoder(conn)
	var hello Msg
	if err := dec.Decode(&hello); err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading the helper's hello: %w", err)
	}
	if hello.Code != "" {
		conn.Close()
		return nil, &Error{hello.Code, hello.Reason}
	}
	if hello.T != Hello || hello.V != Version {
		conn.Close()
		return nil, &Error{CodeVersion, fmt.Sprintf("The helper speaks protocol %d and Hostbay %d; reinstall the helper.", hello.V, Version)}
	}
	c := &Client{
		Build:   hello.Build,
		conn:    conn,
		enc:     json.NewEncoder(conn),
		pending: map[uint64]chan Msg{},
		tunnels: map[string]func(Msg){},
		events:  make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	go c.read(dec)
	return c, nil
}

func (c *Client) read(dec *json.Decoder) {
	defer func() {
		c.mu.Lock()
		close(c.done)
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()
	}()
	for {
		var m Msg
		if dec.Decode(&m) != nil {
			return
		}
		switch m.T {
		case Event:
			select {
			case c.events <- struct{}{}:
			default:
			}
		case TunnelEvent:
			c.mu.Lock()
			on := c.tunnels[m.Tunnel]
			if m.State == TunnelEnded {
				delete(c.tunnels, m.Tunnel)
			}
			c.mu.Unlock()
			if on != nil {
				on(m)
			}
		case Reply:
			c.mu.Lock()
			ch := c.pending[m.ID]
			delete(c.pending, m.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
			if m.Code == CodeVersion {
				return // the helper closes after a version refusal
			}
		}
	}
}

// Events receives a value whenever the system route table changes. Events
// coalesce; re-read the table on each.
func (c *Client) Events() <-chan struct{} { return c.events }

// Done is closed when the connection ends; the helper has then swept every
// route it made.
func (c *Client) Done() <-chan struct{} { return c.done }

// Close ends the connection.
func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) call(ctx context.Context, m Msg) error {
	_, err := c.request(ctx, m)
	return err
}

// request sends m and returns the helper's reply.
func (c *Client) request(ctx context.Context, m Msg) (Msg, error) {
	ch := make(chan Msg, 1)
	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return Msg{}, ErrClosed
	default:
	}
	c.nextID++
	m.ID, m.V = c.nextID, Version
	c.pending[m.ID] = ch
	err := c.enc.Encode(m)
	c.mu.Unlock()
	if err != nil {
		return Msg{}, ErrClosed
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return Msg{}, ErrClosed
		}
		if r.Code != "" {
			return r, &Error{r.Code, r.Reason}
		}
		return r, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, m.ID)
		c.mu.Unlock()
		return Msg{}, ctx.Err()
	}
}

// Tunnel starts a system-wide tunnel: m is an OpenVPNStart, OpenConnectStart
// or WireGuardStart naming its handle in m.Tunnel. on receives the tunnel's
// events, from the read loop, until one says it ended; when the connection
// ends instead (Done), the helper has stopped the tunnel.
func (c *Client) Tunnel(ctx context.Context, m Msg, on func(Msg)) (Msg, error) {
	c.mu.Lock()
	c.tunnels[m.Tunnel] = on
	c.mu.Unlock()
	r, err := c.request(ctx, m)
	if err != nil {
		c.mu.Lock()
		delete(c.tunnels, m.Tunnel)
		c.mu.Unlock()
	}
	return r, err
}

// StopTunnel stops the tunnel with handle h.
func (c *Client) StopTunnel(ctx context.Context, h string) error {
	return c.call(ctx, Msg{T: StopTunnel, Tunnel: h})
}

// Add adds a route through a tunnel interface, or with r.Gateway through the
// local default gateway.
func (c *Client) Add(ctx context.Context, r Route) error {
	return c.call(ctx, Msg{T: Add, Route: &r})
}

// Delete deletes a route Hostbay added, or a vendor route on a tunnel
// interface, which the helper restores when this connection ends.
func (c *Client) Delete(ctx context.Context, r Route) error {
	return c.call(ctx, Msg{T: Delete, Route: &r})
}

// SetMTU sets a tunnel interface's MTU.
func (c *Client) SetMTU(ctx context.Context, iface string, mtu int) error {
	return c.call(ctx, Msg{T: SetMTU, Iface: iface, MTU: mtu})
}

// Change is one step of a route plan: Op is Add or Delete.
type Change struct {
	Op    string
	Route Route
}

// Apply sends each change in order and returns each one's error at the same
// index, so a report can name the routes that failed; one failure does not
// stop the rest.
func (c *Client) Apply(ctx context.Context, changes []Change) []error {
	errs := make([]error, len(changes))
	for i, ch := range changes {
		errs[i] = c.call(ctx, Msg{T: ch.Op, Route: &ch.Route})
	}
	return errs
}
