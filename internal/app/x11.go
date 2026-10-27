package app

import (
	"context"
	"errors"
	"time"

	"hostbay/internal/remotedesktop"
	"hostbay/internal/sshx"
	"hostbay/internal/store"
)

// x11DesktopTimeout bounds starting Xvnc and the desktop on the host.
const x11DesktopTimeout = 30 * time.Second

// X11Service checks this computer's X server for X11 forwarding and opens
// remote X11 desktops in the VNC tab (R47, KTD20).
type X11Service struct {
	conns *ConnService
	vnc   *VncService
}

func NewX11Service(conns *ConnService, vnc *VncService) *X11Service {
	return &X11Service{conns: conns, vnc: vnc}
}

// X11Status is this computer's X server as forwarded X11 programs would
// reach it: where it answers, or the problem, naming the X server to
// install for this OS.
type X11Status struct {
	Display string `json:"display,omitempty"`
	Problem string `json:"problem,omitempty"`
}

// LocalXServer checks that this computer's X server answers.
func (s *X11Service) LocalXServer() X11Status {
	addr, err := sshx.LocalXServer()
	if err != nil {
		return X11Status{Problem: err.Error()}
	}
	return X11Status{Display: addr}
}

// X11DesktopOpen starts an X11 desktop on an SSH host and returns a vncId,
// streamed and closed like VncConnect's. Xvnc runs on the host behind a
// private Unix socket reached over the host's SSH connection, with no TCP
// listener and a fresh X cookie (KTD20); it runs the host's X11 desktop
// command. VncClose, or the tab going away, ends Xvnc and the desktop.
// Host-key and secret prompts come back as from Connect.
func (s *X11Service) X11DesktopOpen(ctx context.Context, hostID string) (string, error) {
	h, err := s.conns.host(hostID)
	if err != nil {
		return "", err
	}
	if h.KindName() != "ssh" {
		return "", errors.New("an X11 desktop needs an SSH host")
	}
	t, err := s.conns.target(hostID)
	if err != nil {
		return "", err
	}
	l, err := s.conns.acquireLease(ctx, t)
	if err != nil {
		return "", err
	}
	cl, err := l.Live(ctx)
	if err != nil {
		l.Release()
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, x11DesktopTimeout)
	defer cancel()
	rfb, err := sshx.StartXvnc(ctx, cl.Client, store.Deref(h.X11Desktop))
	if err != nil {
		l.Release()
		return "", err
	}
	conn, err := remotedesktop.SignIn(rfb, remotedesktop.VNCAuth{Security: "none"})
	if err != nil {
		rfb.Close()
		l.Release()
		return "", err
	}
	return s.vnc.add(&leasedConn{Conn: conn, release: l.Release}, hostID), nil
}
