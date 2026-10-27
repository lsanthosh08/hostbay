// Package remotedesktop relays remote desktop sessions between a server Go
// has already signed in to and the page, over a Wails GoStream (KTD19).
// Credentials stay in Go: the page only ever sees the session after
// sign-in.
package remotedesktop

import (
	"bytes"
	"io"
	"net"
)

// relayBuf bounds one read from either side, and so one frame to the page.
const relayBuf = 64 << 10

// Page is the page's end of a GoStream; *application.StreamConn is one.
type Page interface {
	Receive() ([]byte, error)
	Send([]byte) error
}

// pageConn reads the page's frames as one byte stream and sends each write
// as a frame of its own.
type pageConn struct {
	p   Page
	buf []byte
}

func (c *pageConn) Read(b []byte) (int, error) {
	for len(c.buf) == 0 {
		f, err := c.p.Receive()
		if err != nil {
			return 0, err
		}
		c.buf = f
	}
	n := copy(b, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func (c *pageConn) Write(b []byte) (int, error) {
	// Send keeps the slice until it is delivered, and io.Copy reuses its
	// buffer, so each frame gets its own copy.
	if err := c.p.Send(bytes.Clone(b)); err != nil {
		return 0, err
	}
	return len(b), nil
}

// Relay runs greet with the page, then copies bytes both ways between the
// page and server until either side ends. It closes server before
// returning, so the page going away hangs up on the server.
func Relay(page Page, server net.Conn, greet func(page io.ReadWriter) error) error {
	defer server.Close()
	pc := &pageConn{p: page}
	if greet != nil {
		if err := greet(pc); err != nil {
			return err
		}
	}
	return pipe(pc, server)
}

// pipe copies bytes both ways until either side ends.
func pipe(pc *pageConn, server net.Conn) error {
	go func() {
		_, _ = io.CopyBuffer(server, pc, make([]byte, relayBuf))
		server.Close()
	}()
	_, err := io.CopyBuffer(pc, server, make([]byte, relayBuf))
	return err
}
