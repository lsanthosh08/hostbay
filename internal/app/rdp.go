package app

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"

	"github.com/wailsapp/wails/v3/pkg/application"

	"hostbay/internal/rdp"
	"hostbay/internal/sshx"
	"hostbay/internal/store"
	"hostbay/internal/tlstrust"
)

// RdpStreamName is the GoStream that carries a session's frames, sizes and
// end to the page (see rdp.MsgFrame). The page opens it after RdpConnect and
// sends the rdpId as its first frame.
const RdpStreamName = "rdp"

// RdpService drives remote desktop sessions through the RDP helper, one
// helper process per session. Each session has a backend-made rdpId, so
// RDP-only hosts and two RDP tabs on one host work (KTD2).
type RdpService struct {
	creds  *HostCredentials
	conns  *ConnService
	certs  *tlstrust.Store
	helper func() (string, error) // rdp.HelperPath; tests point it at a fake
	emit   func(name string, data any)

	mu       sync.Mutex
	sessions map[string]*rdp.Session
}

func NewRdpService(creds *HostCredentials, conns *ConnService, certs *tlstrust.Store) *RdpService {
	return &RdpService{creds: creds, conns: conns, certs: certs, helper: rdp.HelperPath, emit: emitEvent, sessions: map[string]*rdp.Session{}}
}

// RdpConnect starts a session and returns its rdpId. An unknown server
// certificate fails with HostKeyUnknown (details.kind "rdpCert"): answer it
// with RdpTrustCert and connect again. A changed one fails with
// HostKeyChanged naming both fingerprints. Either way no credential was sent.
// A host behind a jump host is reached through a local forward on that jump
// host's shared connection (R22).
func (s *RdpService) RdpConnect(ctx context.Context, hostID string, width, height int) (string, error) {
	if width < 200 || height < 200 || width > 8192 || height > 8192 {
		return "", errors.New("the desktop size must be between 200 and 8192 pixels a side")
	}
	c, err := s.creds.Lookup(hostID)
	if err != nil {
		return "", err
	}
	h := c.Host
	port := 3389
	if h.RdpPort != nil && *h.RdpPort != 0 {
		port = *h.RdpPort
	}
	security := "auto"
	if h.RdpSecurity != nil && *h.RdpSecurity != "" {
		security = *h.RdpSecurity
	}
	if security != "auto" && security != "nla" && security != "tls" {
		return "", errors.New("unknown RDP security mode " + strconv.Quote(security))
	}
	path, err := s.helper()
	if err != nil {
		return "", err
	}

	dialHost, dialPort, release, err := s.dialAddr(ctx, h, port)
	if err != nil {
		return "", err
	}

	id := newRequestID()
	sess, err := rdp.Start(ctx, path, rdp.Connect{
		DialHost: dialHost, DialPort: dialPort, TargetHost: h.Hostname,
		User: h.User, Password: c.Password, Width: width, Height: height, Security: security,
	}, rdp.Hooks{
		// Pinned by host and real target, whatever local port was forwarded.
		CheckCert:      func(der []byte) error { return s.certs.Check("rdp", hostID, h.Hostname, port, der) },
		ClipboardText:  func(text string) { s.emit(RdpClipboardEvent(id), text) },
		ClipboardFiles: func(paths []string) { s.emit(RdpClipFilesEvent(id), paths) },
		// The session stays listed until the page has read why it ended
		// (ServeRdpStream) or closed it (RdpClose).
		Exited:    func(string) { release() },
		Clipboard: windowClipboard(),
	})
	if err != nil {
		release()
		return "", err
	}
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()
	s.creds.touch(hostID)
	return id, nil
}

// dialAddr is where the helper dials to reach h's port, and how to let go
// of it: h itself, or a local forward through its jump host, VPN profile or
// proxy (KTD5, KTD22).
func (s *RdpService) dialAddr(ctx context.Context, h store.Host, port int) (string, int, func(), error) {
	target := net.JoinHostPort(h.Hostname, strconv.Itoa(port))
	if h.Jumped() {
		return s.forward(ctx, h, target)
	}
	d, err := s.creds.st.Data()
	if err != nil {
		return "", 0, nil, err
	}
	p, err := s.creds.dialVia(d, &h, "")
	if err == nil {
		err = s.creds.vpnUp(ctx, p)
	}
	if err != nil {
		return "", 0, nil, err
	}
	if p == nil {
		return h.Hostname, port, func() {}, nil
	}
	return viaProxy(p, target)
}

// forward opens a local port carried to target through h's jump host's
// shared connection, and returns where to dial and how to let go of both.
func (s *RdpService) forward(ctx context.Context, h store.Host, target string) (string, int, func(), error) {
	t, err := s.conns.targetVia(*h.JumpHostID, store.Deref(h.VPNID))
	if err != nil {
		return "", 0, nil, err
	}
	lease, err := s.conns.acquire(ctx, t) // an unknown jump host key becomes a trust prompt
	if err != nil {
		return "", 0, nil, err
	}
	l, err := s.conns.lease(lease)
	var f *sshx.Forward
	if err == nil {
		f, err = l.Forward(target)
	}
	if err != nil {
		s.conns.Disconnect(lease)
		return "", 0, nil, err
	}
	return "127.0.0.1", f.Addr().(*net.TCPAddr).Port, func() { f.Close(); s.conns.Disconnect(lease) }, nil
}

// viaProxy opens a local port carried to target through p, a proxy or VPN
// profile, since the helper dials only plain addresses; a failed proxy or
// VPN fails each connection and never falls back to a direct one (KTD22).
func viaProxy(p *sshx.Proxy, target string) (string, int, func(), error) {
	f, err := sshx.ForwardFunc(func() (net.Conn, error) {
		ctx, cancel := context.WithTimeout(context.Background(), sshx.DefaultTimeout)
		defer cancel()
		return sshx.DialTCP(ctx, p, target)
	})
	if err != nil {
		return "", 0, nil, err
	}
	return "127.0.0.1", f.Addr().(*net.TCPAddr).Port, func() { f.Close() }, nil
}

// windowClipboard is the app's clipboard, or nil before the app runs.
func windowClipboard() rdp.Clipboard {
	if a := application.Get(); a != nil {
		return a.Clipboard
	}
	return nil
}

func (s *RdpService) session(rdpID string) (*rdp.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.sessions[rdpID]; sess != nil {
		return sess, nil
	}
	return nil, errors.New("the RDP session has ended")
}

// RdpTrustCert answers the HostKeyUnknown prompt (details.kind "rdpCert")
// carrying requestID; accepting pins the certificate.
func (s *RdpService) RdpTrustCert(requestID string, accept bool) error {
	return s.certs.Answer(requestID, accept)
}

func (s *RdpService) RdpInput(rdpID, kind string, x, y, button int, down bool, scancode, delta int) error {
	sess, err := s.session(rdpID)
	if err != nil {
		return err
	}
	return sess.Input(kind, x, y, button, down, scancode, delta)
}

// RdpClose ends the session. Unknown IDs are already closed.
func (s *RdpService) RdpClose(rdpID string) error {
	if sess, err := s.session(rdpID); err == nil {
		s.forget(rdpID)
		sess.Close()
	}
	return nil
}

func (s *RdpService) forget(rdpID string) {
	s.mu.Lock()
	delete(s.sessions, rdpID)
	s.mu.Unlock()
}

// RdpSetClipboard offers local text to the remote.
func (s *RdpService) RdpSetClipboard(rdpID, text string) error {
	sess, err := s.session(rdpID)
	if err != nil {
		return err
	}
	return sess.SetClipboardText(text)
}

// There is deliberately no bound call that offers local files to the
// remote: the helper reads the local clipboard itself, and such a call would
// let a script in the page send any file the user can read (~/.ssh, say) to
// the server.

// ServeRdpStream is the RdpStreamName handler: it reads the rdpId the page
// sends first, then streams that session's messages, starting with those
// held since connect, until the session ends or the page goes away.
func ServeRdpStream(s *RdpService) application.StreamHandler {
	return func(c *application.StreamConn) {
		defer c.Close()
		id, err := c.Receive()
		if err != nil {
			return
		}
		sess, err := s.session(string(id))
		if err != nil {
			return
		}
		_ = sess.Attach(c.Context(), c.Send)
		select {
		case <-sess.Done():
			s.forget(string(id))
		default:
		}
	}
}
