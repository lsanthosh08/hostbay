package tunnel

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
)

// The SOCKS5 server side (RFC 1928) for dynamic tunnels: no authentication
// and CONNECT only. The handshake follows uniterm's
// backend/session/tunnel_forward.go (Apache-2.0), with replies added for an
// unsupported auth method.

const (
	socksVersion       = 5
	socksNoAuth        = 0x00
	socksNoMethod      = 0xff
	socksCmdConnect    = 0x01
	socksOK            = 0x00
	socksUnreachable   = 0x04
	socksBadCommand    = 0x07
	socksBadAddrType   = 0x08
	socksAddrIPv4      = 0x01
	socksAddrDomain    = 0x03
	socksAddrIPv6      = 0x04
	socksHandshakeWait = 30 // seconds a client has to finish the handshake
)

// socksHandshake reads a client's greeting and CONNECT request from c and
// returns the requested host:port, with a domain name left unresolved so the
// host resolves it. Anything unsupported gets the matching SOCKS error reply
// and an error. The caller sends the final reply once it has dialled.
func socksHandshake(c io.ReadWriter) (string, error) {
	var head [2]byte
	if _, err := io.ReadFull(c, head[:]); err != nil {
		return "", err
	}
	if head[0] != socksVersion {
		return "", fmt.Errorf("socks: version %d is not supported", head[0])
	}
	methods := make([]byte, head[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return "", err
	}
	if !slices.Contains(methods, socksNoAuth) {
		c.Write([]byte{socksVersion, socksNoMethod})
		return "", errors.New("socks: client requires authentication")
	}
	if _, err := c.Write([]byte{socksVersion, socksNoAuth}); err != nil {
		return "", err
	}

	var req [4]byte // version, command, reserved, address type
	if _, err := io.ReadFull(c, req[:]); err != nil {
		return "", err
	}
	if req[0] != socksVersion {
		return "", fmt.Errorf("socks: request version %d is not supported", req[0])
	}
	if req[1] != socksCmdConnect {
		socksReply(c, socksBadCommand)
		return "", fmt.Errorf("socks: command %d is not supported", req[1])
	}
	var host string
	switch req[3] {
	case socksAddrIPv4, socksAddrIPv6:
		ip := make(net.IP, 4)
		if req[3] == socksAddrIPv6 {
			ip = make(net.IP, 16)
		}
		if _, err := io.ReadFull(c, ip); err != nil {
			return "", err
		}
		host = ip.String()
	case socksAddrDomain:
		var n [1]byte
		if _, err := io.ReadFull(c, n[:]); err != nil {
			return "", err
		}
		name := make([]byte, n[0])
		if _, err := io.ReadFull(c, name); err != nil {
			return "", err
		}
		host = string(name)
	default:
		socksReply(c, socksBadAddrType)
		return "", fmt.Errorf("socks: address type %d is not supported", req[3])
	}
	var port [2]byte
	if _, err := io.ReadFull(c, port[:]); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))), nil
}

// socksReply sends a reply with an empty IPv4 bound address, which CONNECT
// clients ignore.
func socksReply(w io.Writer, code byte) error {
	_, err := w.Write([]byte{socksVersion, code, 0, socksAddrIPv4, 0, 0, 0, 0, 0, 0})
	return err
}
