// Package socks is the SOCKS5 server (RFC 1928) hostbay-vpn serves on
// loopback for Hostbay to dial through a VPN (KTD2). Unlike the dynamic
// tunnel's server in internal/tunnel, every client must log in with the
// per-run user and password (RFC 1929), and UDP ASSOCIATE is served for mosh.
//
// The server never resolves a name itself: a domain address goes to Dial as
// it came, so the engine resolves it through the tunnel's DNS.
package socks

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"sync"
	"time"
)

const (
	version         = 5
	authVersion     = 1 // RFC 1929's subnegotiation version
	methodLogin     = 0x02
	methodNone      = 0xff
	cmdConnect      = 0x01
	cmdUDPAssociate = 0x03
	repOK           = 0x00
	repFailure      = 0x01
	repUnreachable  = 0x04
	repBadCommand   = 0x07
	repBadAddrType  = 0x08
	addrIPv4        = 0x01
	addrDomain      = 0x03
	addrIPv6        = 0x04

	handshakeWait = 30 * time.Second // a client has this long to log in and ask
	dialWait      = 30 * time.Second
	maxDatagram   = 65535
)

// Server serves SOCKS5 to clients that log in as User with Pass. Dial opens
// "tcp" connections for CONNECT and connected "udp" sockets, one per
// destination, for UDP ASSOCIATE.
type Server struct {
	User, Pass string
	Dial       func(ctx context.Context, network, address string) (net.Conn, error)
}

// Serve accepts clients on ln until it is closed.
func (s *Server) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(handshakeWait))
	if err := s.login(c); err != nil {
		return
	}
	var req [4]byte // version, command, reserved, address type
	if _, err := io.ReadFull(c, req[:]); err != nil || req[0] != version {
		return
	}
	target, err := readAddr(c, req[3])
	if err != nil {
		reply(c, repBadAddrType, nil)
		return
	}
	switch req[1] {
	case cmdConnect:
		s.connect(c, target)
	case cmdUDPAssociate:
		s.associate(c, target)
	default:
		reply(c, repBadCommand, nil)
	}
}

// login runs the greeting and the RFC 1929 login. A client that cannot log in
// gets a refusal and nothing is dialled.
func (s *Server) login(c io.ReadWriter) error {
	var head [2]byte
	if _, err := io.ReadFull(c, head[:]); err != nil {
		return err
	}
	if head[0] != version {
		return fmt.Errorf("socks: version %d is not supported", head[0])
	}
	methods := make([]byte, head[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return err
	}
	if !slices.Contains(methods, methodLogin) {
		c.Write([]byte{version, methodNone})
		return errors.New("socks: client cannot log in")
	}
	if _, err := c.Write([]byte{version, methodLogin}); err != nil {
		return err
	}
	var ver [1]byte
	if _, err := io.ReadFull(c, ver[:]); err != nil {
		return err
	}
	user, err := readShort(c)
	if err != nil {
		return err
	}
	pass, err := readShort(c)
	if err != nil {
		return err
	}
	// Both compares run, so the time taken does not say which one failed.
	ok := subtle.ConstantTimeCompare(user, []byte(s.User)) & subtle.ConstantTimeCompare(pass, []byte(s.Pass))
	if ver[0] != authVersion || ok != 1 {
		c.Write([]byte{authVersion, 0x01})
		return errors.New("socks: wrong login")
	}
	_, err = c.Write([]byte{authVersion, 0x00})
	return err
}

func (s *Server) connect(c net.Conn, target string) {
	ctx, cancel := context.WithTimeout(context.Background(), dialWait)
	up, err := s.Dial(ctx, "tcp", target)
	cancel()
	if err != nil {
		reply(c, repUnreachable, nil)
		return
	}
	defer up.Close()
	if reply(c, repOK, nil) != nil {
		return
	}
	c.SetDeadline(time.Time{})
	done := make(chan struct{})
	go func() {
		io.Copy(up, c)
		up.Close()
		close(done)
	}()
	io.Copy(c, up)
	c.Close()
	<-done
}

// associate relays datagrams between the client and the destinations it
// names until the client's TCP connection closes. Only datagrams from the
// client's address and declared port are relayed; a client that declares port
// 0 is held to the port of its first datagram.
func (s *Server) associate(c net.Conn, declared string) {
	local := c.LocalAddr().(*net.TCPAddr)
	relay, err := net.ListenUDP("udp", &net.UDPAddr{IP: local.IP})
	if err != nil {
		reply(c, repFailure, nil)
		return
	}
	defer relay.Close()
	if reply(c, repOK, relay.LocalAddr().(*net.UDPAddr)) != nil {
		return
	}
	c.SetDeadline(time.Time{})

	clientIP := c.RemoteAddr().(*net.TCPAddr).IP
	_, p, _ := net.SplitHostPort(declared)
	clientPort, _ := strconv.Atoi(p)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		io.Copy(io.Discard, c) // the association lives as long as this connection
		cancel()
		relay.Close()
	}()

	var mu sync.Mutex
	conns := map[string]net.Conn{}
	defer func() {
		mu.Lock()
		for _, up := range conns {
			up.Close()
		}
		mu.Unlock()
	}()

	buf := make([]byte, maxDatagram)
	for {
		n, from, err := relay.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if !from.IP.Equal(clientIP) || (clientPort != 0 && from.Port != clientPort) {
			continue
		}
		if clientPort == 0 {
			clientPort = from.Port
		}
		// RSV(2) FRAG(1) ATYP ADDR PORT DATA; fragments are not supported.
		if n < 4 || buf[2] != 0 {
			continue
		}
		r := bytes.NewReader(buf[4:n])
		dest, err := readAddr(r, buf[3])
		if err != nil {
			continue
		}
		data := buf[n-r.Len() : n]
		head := slices.Clone(buf[:n-r.Len()])
		mu.Lock()
		up := conns[dest]
		mu.Unlock()
		if up == nil {
			dctx, dcancel := context.WithTimeout(ctx, dialWait)
			up, err = s.Dial(dctx, "udp", dest)
			dcancel()
			if err != nil {
				continue
			}
			mu.Lock()
			conns[dest] = up
			mu.Unlock()
			// Replies carry the destination as the client wrote it.
			go func(up net.Conn, head []byte, to *net.UDPAddr) {
				b := make([]byte, maxDatagram)
				copy(b, head)
				for {
					m, err := up.Read(b[len(head):])
					if err != nil {
						return
					}
					relay.WriteToUDP(b[:len(head)+m], to)
				}
			}(up, head, from)
		}
		up.Write(data)
	}
}

// readAddr reads a SOCKS address of type atyp and its port and returns
// host:port, with a domain left unresolved.
func readAddr(r io.Reader, atyp byte) (string, error) {
	var host string
	switch atyp {
	case addrIPv4, addrIPv6:
		ip := make(net.IP, 4)
		if atyp == addrIPv6 {
			ip = make(net.IP, 16)
		}
		if _, err := io.ReadFull(r, ip); err != nil {
			return "", err
		}
		host = ip.String()
	case addrDomain:
		name, err := readShort(r)
		if err != nil {
			return "", err
		}
		host = string(name)
	default:
		return "", fmt.Errorf("socks: address type %d is not supported", atyp)
	}
	var port [2]byte
	if _, err := io.ReadFull(r, port[:]); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))), nil
}

// readShort reads a length byte and that many bytes.
func readShort(r io.Reader) ([]byte, error) {
	var n [1]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	b := make([]byte, n[0])
	_, err := io.ReadFull(r, b)
	return b, err
}

// reply sends a reply with bound address bnd, or an empty IPv4 address,
// which CONNECT clients ignore.
func reply(w io.Writer, code byte, bnd *net.UDPAddr) error {
	b := []byte{version, code, 0, addrIPv4, 0, 0, 0, 0, 0, 0}
	if bnd != nil {
		copy(b[4:8], bnd.IP.To4())
		binary.BigEndian.PutUint16(b[8:], uint16(bnd.Port))
	}
	_, err := w.Write(b)
	return err
}
