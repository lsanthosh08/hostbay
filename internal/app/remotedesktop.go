package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"hostbay/internal/remotedesktop"
	"hostbay/internal/tlstrust"
)

// VncStreamName is the GoStream a VNC session runs over (R45, KTD19). After
// VncConnect has signed in, the page opens it, sends the vncId as its first
// frame and hands the open stream to noVNC as its channel. From then on the
// stream carries plain RFB from a server that asks for no authentication,
// so the password never reaches the page. An unknown or already used vncId
// gets nothing and the stream closes.
const VncStreamName = "vnc"

// vncClaimTimeout is how long a signed-in server waits for its page.
const vncClaimTimeout = 30 * time.Second

// VncService signs in to VNC hosts in Go and relays each session to its
// tab (KTD19).
type VncService struct {
	creds *HostCredentials
	conns *ConnService
	certs *tlstrust.Store

	mu       sync.Mutex
	sessions map[string]*vncSession
}

type vncSession struct {
	conn    net.Conn
	claimed bool // a stream is relaying it
}

func NewVncService(creds *HostCredentials, conns *ConnService, certs *tlstrust.Store) *VncService {
	return &VncService{creds: creds, conns: conns, certs: certs, sessions: map[string]*vncSession{}}
}

// VncConnect signs in to a VNC host and returns the vncId its tab streams.
// The server is reached through the host's jump host or proxy, never around
// them (KTD22), and in its security mode (KTD18): VeNCrypt by default, or
// plain VNC when the host says so. Under VeNCrypt an unknown certificate
// fails with HostKeyUnknown (details.kind "vncCert"): answer it with
// VncTrustCert and connect again. A changed one fails with HostKeyChanged.
// Either way no password was sent. A refused password is SecretNeeded.
func (s *VncService) VncConnect(ctx context.Context, hostID string) (string, error) {
	c, err := s.creds.Lookup(hostID)
	if err != nil {
		return "", err
	}
	h := c.Host
	if h.KindName() != "vnc" {
		return "", errors.New("this host is not a VNC host")
	}
	port := h.Port
	if port == 0 {
		port = 5900
	}
	auth := remotedesktop.VNCAuth{Security: h.VNCSecurity(), User: h.User, Password: c.Password}
	signIn := func(cfg *tls.Config) (net.Conn, error) {
		conn, err := s.conns.dialHost(ctx, &h, port)
		if err != nil {
			return nil, err
		}
		a := auth
		a.TLS = cfg
		rc, err := remotedesktop.SignIn(conn, a)
		if err != nil {
			conn.Close()
		}
		return rc, err
	}
	var conn net.Conn
	if auth.Security == "vencrypt" {
		// Pinned by host and real target, whatever the jump host or proxy.
		_, err = s.certs.Connect("vnc", hostID, h.Hostname, port, func(cfg *tls.Config) error {
			var err error
			conn, err = signIn(cfg)
			return err
		})
	} else {
		conn, err = signIn(nil)
	}
	if err != nil {
		return "", withHostID(err, hostID)
	}

	return s.add(conn, hostID), nil
}

// add holds a signed-in server for its page and returns its vncId. One
// no page claims in time is closed.
func (s *VncService) add(conn net.Conn, hostID string) string {
	id := newRequestID()
	s.mu.Lock()
	s.sessions[id] = &vncSession{conn: conn}
	s.mu.Unlock()
	time.AfterFunc(vncClaimTimeout, func() {
		s.mu.Lock()
		v := s.sessions[id]
		if v != nil && !v.claimed {
			delete(s.sessions, id)
			v.conn.Close()
		}
		s.mu.Unlock()
	})
	s.creds.touch(hostID)
	return id
}

// VncTrustCert answers the HostKeyUnknown prompt (details.kind "vncCert")
// carrying requestID; accepting pins the certificate.
func (s *VncService) VncTrustCert(requestID string, accept bool) error {
	return s.certs.Answer(requestID, accept)
}

// VncClose ends the session. Unknown IDs are already closed.
func (s *VncService) VncClose(vncID string) error {
	s.mu.Lock()
	v := s.sessions[vncID]
	delete(s.sessions, vncID)
	s.mu.Unlock()
	if v != nil {
		v.conn.Close()
	}
	return nil
}

// serve relays the session whose vncId the page sends first.
func (s *VncService) serve(page remotedesktop.Page) {
	id, err := page.Receive()
	if err != nil {
		return
	}
	s.mu.Lock()
	v := s.sessions[string(id)]
	ok := v != nil && !v.claimed
	if ok {
		v.claimed = true
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	defer s.VncClose(string(id))
	_ = remotedesktop.Relay(page, v.conn, remotedesktop.OfferNone)
}

// ServeVncStream is the VncStreamName handler.
func ServeVncStream(s *VncService) application.StreamHandler {
	return func(c *application.StreamConn) {
		defer c.Close()
		s.serve(c)
	}
}

// SpiceStreamName is the GoStream each SPICE channel runs over (R46,
// KTD19). After SpiceConnect has signed in to the main channel, the page
// opens one stream per channel, main first, sends the spiceId as each
// one's first frame and hands the open stream to spice-html5 as that
// channel's socket. The page links each channel as usual with an empty
// password: Go signs in to the server itself, sealing the real ticket under
// that channel's link key, drops the page's ticket and tells the page the
// link succeeded, so the password never reaches the page. An unknown
// spiceId, a channel before main, or a channel already linked gets nothing
// and the stream closes. The main stream closing ends the session.
const SpiceStreamName = "spice"

// SpiceService signs in to SPICE hosts in Go and relays each session's
// channels to its tab (KTD19).
type SpiceService struct {
	creds *HostCredentials
	conns *ConnService
	certs *tlstrust.Store

	mu       sync.Mutex
	sessions map[string]*spiceSession
}

// spiceSession is one signed-in SPICE session. Its channels each dial the
// server again, the way the main channel did.
type spiceSession struct {
	dial     func() (net.Conn, error)
	password string      // held in Go for the channels' tickets
	tls      *tls.Config // the main channel's, already pinned; nil for a plain port
	main     net.Conn
	reply    []byte            // the main channel's link reply, replayed to the page
	linked   map[[2]uint8]bool // channel type and ID; main first, each once
	conns    []net.Conn        // channel connections, closed with the session
	closed   bool
}

// spiceMain is the main channel's type and ID.
var spiceMain = [2]uint8{1, 0}

func NewSpiceService(creds *HostCredentials, conns *ConnService, certs *tlstrust.Store) *SpiceService {
	return &SpiceService{creds: creds, conns: conns, certs: certs, sessions: map[string]*spiceSession{}}
}

// SpiceConnect signs in to a SPICE host's main channel and returns the
// spiceId its tab streams. Every channel is reached through the host's
// jump host or proxy, never around them (KTD22), and in its security mode
// (KTD18): the port is a SPICE tls-port by default, or plain when the host
// says so; there is no fallback between them. On a TLS port an unknown
// certificate fails with HostKeyUnknown (details.kind "spiceCert"): answer
// it with SpiceTrustCert and connect again. A changed one fails with
// HostKeyChanged. Either way no ticket was sent. A refused password is
// SecretNeeded.
func (s *SpiceService) SpiceConnect(ctx context.Context, hostID string) (string, error) {
	c, err := s.creds.Lookup(hostID)
	if err != nil {
		return "", err
	}
	h := c.Host
	if h.KindName() != "spice" {
		return "", errors.New("this host is not a SPICE host")
	}
	port := h.Port
	if port == 0 {
		port = 5900
	}
	v := &spiceSession{
		// Later channels outlive this call, so they dial without its context.
		dial:     func() (net.Conn, error) { return s.conns.dialHost(context.Background(), &h, port) },
		password: c.Password,
		linked:   map[[2]uint8]bool{},
	}
	signIn := func(cfg *tls.Config) error {
		conn, err := s.conns.dialHost(ctx, &h, port)
		if err != nil {
			return err
		}
		v.main, v.reply, err = remotedesktop.SpiceSignIn(conn, remotedesktop.SpiceMainLink, c.Password, cfg)
		if err != nil {
			conn.Close()
		}
		return err
	}
	if h.SpiceSecurity() == "tls" {
		// Pinned by host and real target, whatever the jump host or proxy.
		v.tls, err = s.certs.Connect("spice", hostID, h.Hostname, port, signIn)
	} else {
		err = signIn(nil)
	}
	if err != nil {
		return "", withHostID(err, hostID)
	}

	id := newRequestID()
	s.mu.Lock()
	s.sessions[id] = v
	s.mu.Unlock()
	time.AfterFunc(vncClaimTimeout, func() {
		s.mu.Lock()
		unclaimed := !v.linked[spiceMain]
		s.mu.Unlock()
		if unclaimed {
			s.SpiceClose(id)
		}
	})
	s.creds.touch(hostID)
	return id, nil
}

// SpiceTrustCert answers the HostKeyUnknown prompt (details.kind
// "spiceCert") carrying requestID; accepting pins the certificate.
func (s *SpiceService) SpiceTrustCert(requestID string, accept bool) error {
	return s.certs.Answer(requestID, accept)
}

// SpiceClose ends the session and all its channels. Unknown IDs are
// already closed.
func (s *SpiceService) SpiceClose(spiceID string) error {
	s.mu.Lock()
	v := s.sessions[spiceID]
	delete(s.sessions, spiceID)
	var conns []net.Conn
	if v != nil {
		v.closed = true
		conns = append(v.conns, v.main)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
	return nil
}

// channel returns the signed-in server connection for the page's link
// message: the main channel Go linked in SpiceConnect, or a new channel Go
// signs in to now.
func (s *SpiceService) channel(v *spiceSession, link []byte) (net.Conn, []byte, error) {
	typ, id := remotedesktop.SpiceChannel(link)
	key := [2]uint8{typ, id}
	main := key == spiceMain
	s.mu.Lock()
	switch {
	case v.closed:
		s.mu.Unlock()
		return nil, nil, errors.New("the SPICE session has ended")
	case v.linked[key]:
		s.mu.Unlock()
		return nil, nil, errors.New("this SPICE channel is already linked")
	case !main && !v.linked[spiceMain]:
		s.mu.Unlock()
		return nil, nil, errors.New("a SPICE channel opened before the main channel")
	case main && !bytes.Equal(link, remotedesktop.SpiceMainLink):
		s.mu.Unlock()
		return nil, nil, errors.New("the page's SPICE client linked the main channel differently from Go; is spice-html5 the vendored version?")
	}
	v.linked[key] = true
	s.mu.Unlock()
	if main {
		return v.main, v.reply, nil
	}

	conn, err := v.dial()
	if err != nil {
		return nil, nil, err
	}
	rc, reply, err := remotedesktop.SpiceSignIn(conn, link, v.password, v.tls)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	s.mu.Lock()
	closed := v.closed
	if !closed {
		v.conns = append(v.conns, rc)
	}
	s.mu.Unlock()
	if closed {
		rc.Close()
		return nil, nil, errors.New("the SPICE session has ended")
	}
	return rc, reply, nil
}

// serve relays the channel the page links on the session whose spiceId it
// sends first.
func (s *SpiceService) serve(page remotedesktop.Page) {
	id, err := page.Receive()
	if err != nil {
		return
	}
	s.mu.Lock()
	v := s.sessions[string(id)]
	s.mu.Unlock()
	if v == nil {
		return
	}
	main := false
	_ = remotedesktop.RelaySpice(page, func(link []byte) (net.Conn, []byte, error) {
		conn, reply, err := s.channel(v, link)
		typ, ch := remotedesktop.SpiceChannel(link)
		main = err == nil && [2]uint8{typ, ch} == spiceMain
		return conn, reply, err
	})
	if main {
		s.SpiceClose(string(id))
	}
}

// ServeSpiceStream is the SpiceStreamName handler.
func ServeSpiceStream(s *SpiceService) application.StreamHandler {
	return func(c *application.StreamConn) {
		defer c.Close()
		s.serve(c)
	}
}
